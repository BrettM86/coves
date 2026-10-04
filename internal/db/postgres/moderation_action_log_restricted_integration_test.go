//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func restrictedLogService(db *sql.DB) moderation.Service {
	return moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour,
			MaxLiveIdempotencyKeys: 1000, CursorSecret: "restricted-log-cursor-secret",
		},
	)
}

func restrictedLogComment(t *testing.T, db *sql.DB, authorDID string, root moderation.StrongRef) moderation.StrongRef {
	t.Helper()
	rkey := testkit.TID()
	subject := moderation.StrongRef{URI: "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey, CID: moderationCommentCID}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'restricted log comment', NOW())
	`, subject.URI, subject.CID, rkey, authorDID, root.URI, root.CID)
	require.NoError(t, err)
	return subject
}

func restrictedLogRemove(t *testing.T, service moderation.Service, subject moderation.StrongRef) string {
	t.Helper()
	result, err := service.RemoveContent(t.Context(), fixtures.DID("restrictedlogadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: testkit.UniqueIDWithPrefix(t, "remove"),
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	return result.Action.ID
}

func restrictedLogPage(t *testing.T, service moderation.Service, params moderation.ListActionsParams) (*moderation.ActionPage, string) {
	t.Helper()
	page, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	return page, string(encoded)
}

func restrictedLogIDs(page *moderation.ActionPage) []string {
	ids := make([]string, 0, len(page.Actions))
	for _, action := range page.Actions {
		ids = append(ids, action.Ref.ActionID)
	}
	return ids
}

// Removals are public by default (decided 2026-10-03): the public log names a
// subject while its own post or comment row is indexed, whatever its community
// admission status and even after its author soft-deleted it. Only a subject whose row is gone, as after account erasure,
// is restricted: listed without its subject, and excluded whenever a subject,
// collection or community filter could tie it to its target. Access is decided
// when the log is read, so a subject indexed again shows normally.
func TestModerationActionLogRestrictsOnlyUnindexedSubjects(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	service := restrictedLogService(db)

	post := func(title, status string) moderation.StrongRef {
		subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, title, "")
		switch status {
		case "":
		case "unadmitted":
			_, err := db.ExecContext(t.Context(), `DELETE FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2`, communityDID, subject.URI)
			require.NoError(t, err)
		default:
			_, err := db.ExecContext(t.Context(), `
				UPDATE community_post_admissions SET status = $3, accepted_cid = NULL,
					decision_code = 'rule-violation', decision_at = NOW(), updated_at = NOW()
				WHERE community_did = $1 AND post_uri = $2
			`, communityDID, subject.URI, status)
			require.NoError(t, err)
		}
		return subject
	}
	accepted := post("accepted", "")
	pending := post("pending", "pending")
	orphanRoot := post("orphan root", "")
	erasedPost := post("erased post", "")
	erasedComment := restrictedLogComment(t, db, authorDID, accepted)
	softDeletedPost := post("soft deleted post", "")
	softDeletedComment := restrictedLogComment(t, db, authorDID, accepted)

	type entry struct {
		subject    moderation.StrongRef
		collection string
		restricted bool
	}
	subjects := []entry{
		{accepted, moderation.PostV2Collection, false},
		{pending, moderation.PostV2Collection, false},
		{post("pending reacceptance", "pending_reacceptance"), moderation.PostV2Collection, false},
		{post("rejected", "rejected"), moderation.PostV2Collection, false},
		{post("community removed", "removed"), moderation.PostV2Collection, false},
		{post("unadmitted", "unadmitted"), moderation.PostV2Collection, false},
		{indexedModerationPost(t, db, moderation.LegacyPostCollection, communityDID, authorDID, "legacy", ""), moderation.LegacyPostCollection, false},
		{restrictedLogComment(t, db, authorDID, pending), moderation.CommentCollection, false},
		{restrictedLogComment(t, db, authorDID, orphanRoot), moderation.CommentCollection, false},
		{softDeletedPost, moderation.PostV2Collection, false},
		{softDeletedComment, moderation.CommentCollection, false},
		{erasedPost, moderation.PostV2Collection, true},
		{erasedComment, moderation.CommentCollection, true},
	}
	entries := map[string]entry{}
	var publicIDs []string
	for _, subject := range subjects {
		id := restrictedLogRemove(t, service, subject.subject)
		entries[id] = subject
		if !subject.restricted {
			publicIDs = append([]string{id}, publicIDs...)
		}
	}
	// Account erasure hard-deletes rows; an author's delete only sets
	// deleted_at, so the row stays and the entry stays public. A comment whose
	// root post is gone keeps its own row, so it stays public too.
	for _, statement := range []struct{ sql, uri string }{
		{`UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, softDeletedPost.URI},
		{`UPDATE comments SET deleted_at = NOW() WHERE uri = $1`, softDeletedComment.URI},
		{`DELETE FROM posts WHERE uri = $1`, erasedPost.URI},
		{`DELETE FROM comments WHERE uri = $1`, erasedComment.URI},
		{`DELETE FROM posts WHERE uri = $1`, orphanRoot.URI},
	} {
		_, err := db.ExecContext(t.Context(), statement.sql, statement.uri)
		require.NoError(t, err)
	}
	publicIn := func(collection string) []string {
		var ids []string
		for _, id := range publicIDs {
			if entries[id].collection == collection {
				ids = append(ids, id)
			}
		}
		return ids
	}

	t.Run("unfiltered public log names every indexed subject", func(t *testing.T) {
		page, encoded := restrictedLogPage(t, service, moderation.ListActionsParams{})
		require.Len(t, page.Actions, len(entries))
		for _, action := range page.Actions {
			want, found := entries[action.Ref.ActionID]
			require.True(t, found, action.Ref.ActionID)
			if want.restricted {
				assert.Nil(t, action.Subject, "an erased subject must not appear in the public log")
				assert.NotContains(t, encoded, want.subject.URI)
			} else {
				assert.Equal(t, &moderation.SubjectRefView{URI: want.subject.URI, CID: want.subject.CID, CommunityDID: communityDID}, action.Subject)
			}
		}
	})

	t.Run("admin log keeps erased subjects private", func(t *testing.T) {
		page, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{})
		require.NoError(t, err)
		require.Len(t, page.Actions, len(entries))
		for _, action := range page.Actions {
			want := entries[action.Action.Ref.ActionID]
			ref := &moderation.SubjectRefView{URI: want.subject.URI, CID: want.subject.CID, CommunityDID: communityDID}
			if want.restricted {
				assert.Nil(t, action.Action.Subject)
				assert.Equal(t, ref, action.PrivateSubject)
			} else {
				assert.Equal(t, ref, action.Action.Subject)
				assert.Nil(t, action.PrivateSubject)
			}
		}
	})

	t.Run("filters select every indexed subject and never an erased one", func(t *testing.T) {
		page, _ := restrictedLogPage(t, service, moderation.ListActionsParams{Community: communityDID})
		assert.Equal(t, publicIDs, restrictedLogIDs(page), "the community filter lists every indexed subject, whatever its admission status")
		for _, collection := range []string{moderation.PostV2Collection, moderation.LegacyPostCollection, moderation.CommentCollection} {
			page, _ = restrictedLogPage(t, service, moderation.ListActionsParams{Collection: collection})
			assert.Equal(t, publicIn(collection), restrictedLogIDs(page), collection)
		}
		never := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()
		_, neverBody := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: never})
		for id, want := range entries {
			page, body := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: want.subject.URI})
			if want.restricted {
				assert.Equal(t, neverBody, body, "an erased subject filter must answer exactly as a never-moderated subject")
			} else {
				assert.Equal(t, []string{id}, restrictedLogIDs(page))
			}
		}
	})

	t.Run("filtered pagination skips erased rows before the limit", func(t *testing.T) {
		one := 1
		params := moderation.ListActionsParams{Community: communityDID, Limit: &one}
		var walked []string
		for pages := 1; ; pages++ {
			require.LessOrEqual(t, pages, len(publicIDs), "every page of the walk must hold a public action")
			page, _ := restrictedLogPage(t, service, params)
			require.Len(t, page.Actions, 1, "a page must not come back short because erased rows used its limit")
			walked = append(walked, restrictedLogIDs(page)...)
			if page.Cursor == "" {
				break
			}
			params.Cursor = page.Cursor
		}
		assert.Equal(t, publicIDs, walked)
	})

	t.Run("a subject indexed again shows normally", func(t *testing.T) {
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
			VALUES ($1, $2, $3, $4, $5, 'reindexed', NOW())
		`, erasedPost.URI, erasedPost.CID, erasedPost.URI[strings.LastIndex(erasedPost.URI, "/")+1:], authorDID, communityDID)
		require.NoError(t, err)
		page, _ := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: erasedPost.URI})
		require.Len(t, page.Actions, 1)
		assert.Equal(t, &moderation.SubjectRefView{URI: erasedPost.URI, CID: erasedPost.CID, CommunityDID: communityDID}, page.Actions[0].Subject)
		page, _ = restrictedLogPage(t, service, moderation.ListActionsParams{Community: communityDID})
		assert.Len(t, page.Actions, len(publicIDs)+1)
	})
}
