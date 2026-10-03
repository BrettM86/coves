//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/json"
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

// A removal of content the public cannot see must not disclose it through the
// public modlog (PRD §6 "restricted subjects do not leak"). Restricted-ness is
// post.get's anonymous admission rule, evaluated when the log is read: a
// pending, rejected, community-removed or unadmitted postv2, and a comment
// whose root post is one of those, is listed without its subject and is
// excluded whenever a subject, collection or community filter could tie it to
// its target. A post admitted later shows normally.
func TestModerationActionLogRestrictsNonPublicSubjects(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	service := restrictedLogService(db)

	post := func(title, admissionSQL string) moderation.StrongRef {
		subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, title, "")
		if admissionSQL != "" {
			_, err := db.ExecContext(t.Context(), admissionSQL, communityDID, subject.URI)
			require.NoError(t, err)
		}
		return subject
	}
	const pendingSQL = `UPDATE community_post_admissions SET status = 'pending', accepted_cid = NULL, updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2`
	accepted := post("accepted", "")
	pending := post("pending", pendingSQL)
	rejected := post("rejected", `UPDATE community_post_admissions SET status = 'rejected', accepted_cid = NULL,
		decision_code = 'off-topic', decision_at = NOW(), updated_at = NOW() WHERE community_did = $1 AND post_uri = $2`)
	communityRemoved := post("community removed", `UPDATE community_post_admissions SET status = 'removed', accepted_cid = NULL,
		decision_code = 'rule-violation', decision_at = NOW(), updated_at = NOW() WHERE community_did = $1 AND post_uri = $2`)
	unadmitted := post("unadmitted", `DELETE FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2`)
	legacy := indexedModerationPost(t, db, moderation.LegacyPostCollection, communityDID, authorDID, "legacy", "")
	pendingRoot := post("pending root", pendingSQL)
	publicComment := restrictedLogComment(t, db, authorDID, accepted)
	restrictedComment := restrictedLogComment(t, db, authorDID, pendingRoot)

	type entry struct {
		subject    moderation.StrongRef
		restricted bool
	}
	entries := map[string]entry{}
	var publicIDs []string
	for _, subject := range []struct {
		ref        moderation.StrongRef
		restricted bool
	}{
		{accepted, false},
		{pending, true},
		{rejected, true},
		{communityRemoved, true},
		{unadmitted, true},
		{legacy, false},
		{publicComment, false},
		{restrictedComment, true},
	} {
		id := restrictedLogRemove(t, service, subject.ref)
		entries[id] = entry{subject.ref, subject.restricted}
		if !subject.restricted {
			publicIDs = append([]string{id}, publicIDs...)
		}
	}

	t.Run("unfiltered public log lists restricted actions without their subjects", func(t *testing.T) {
		page, encoded := restrictedLogPage(t, service, moderation.ListActionsParams{})
		require.Len(t, page.Actions, len(entries))
		for _, action := range page.Actions {
			want, found := entries[action.Ref.ActionID]
			require.True(t, found, action.Ref.ActionID)
			assert.Empty(t, action.Scope.CommunityDID, action.Ref.ActionID)
			if want.restricted {
				assert.Nil(t, action.Subject, "a restricted subject must not appear in the public log")
				assert.NotContains(t, encoded, want.subject.URI)
			} else {
				assert.Equal(t, &moderation.SubjectRefView{URI: want.subject.URI, CID: want.subject.CID}, action.Subject)
			}
		}
	})

	t.Run("admin log keeps restricted subjects private", func(t *testing.T) {
		page, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{})
		require.NoError(t, err)
		require.Len(t, page.Actions, len(entries))
		for _, action := range page.Actions {
			want := entries[action.Action.Ref.ActionID]
			ref := &moderation.SubjectRefView{URI: want.subject.URI, CID: want.subject.CID}
			if want.restricted {
				assert.Nil(t, action.Action.Subject)
				assert.Equal(t, ref, action.PrivateSubject)
			} else {
				assert.Equal(t, ref, action.Action.Subject)
				assert.Nil(t, action.PrivateSubject)
			}
		}
	})

	t.Run("filters never select a restricted subject", func(t *testing.T) {
		page, _ := restrictedLogPage(t, service, moderation.ListActionsParams{Community: communityDID})
		assert.Equal(t, publicIDs, restrictedLogIDs(page), "the community filter must not tie restricted subjects to the community")
		page, _ = restrictedLogPage(t, service, moderation.ListActionsParams{Collection: moderation.PostV2Collection})
		assert.Equal(t, []string{publicIDs[2]}, restrictedLogIDs(page), "only the accepted postv2 may match its collection")
		page, _ = restrictedLogPage(t, service, moderation.ListActionsParams{Collection: moderation.CommentCollection})
		assert.Equal(t, []string{publicIDs[0]}, restrictedLogIDs(page), "only the comment on an accepted root may match its collection")
		never := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()
		_, neverBody := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: never})
		for id, want := range entries {
			page, body := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: want.subject.URI})
			if want.restricted {
				assert.Equal(t, neverBody, body, "a restricted subject filter must answer exactly as a never-moderated subject")
			} else {
				assert.Equal(t, []string{id}, restrictedLogIDs(page))
			}
		}
	})

	t.Run("filtered pagination skips restricted rows before the limit", func(t *testing.T) {
		one := 1
		params := moderation.ListActionsParams{Community: communityDID, Limit: &one}
		var walked []string
		for pages := 1; ; pages++ {
			require.LessOrEqual(t, pages, len(publicIDs), "every page of the walk must hold a public action")
			page, _ := restrictedLogPage(t, service, params)
			require.Len(t, page.Actions, 1, "a page must not come back short because restricted rows used its limit")
			walked = append(walked, restrictedLogIDs(page)...)
			if page.Cursor == "" {
				break
			}
			params.Cursor = page.Cursor
		}
		assert.Equal(t, publicIDs, walked)
	})

	t.Run("a post admitted later shows normally", func(t *testing.T) {
		_, err := db.ExecContext(t.Context(), `
			UPDATE community_post_admissions SET status = 'accepted', accepted_cid = $3, updated_at = NOW()
			WHERE community_did = $1 AND post_uri = $2
		`, communityDID, pending.URI, pending.CID)
		require.NoError(t, err)
		page, _ := restrictedLogPage(t, service, moderation.ListActionsParams{Subject: pending.URI})
		require.Len(t, page.Actions, 1)
		assert.Equal(t, &moderation.SubjectRefView{URI: pending.URI, CID: pending.CID}, page.Actions[0].Subject)
		page, _ = restrictedLogPage(t, service, moderation.ListActionsParams{Community: communityDID})
		assert.Len(t, page.Actions, len(publicIDs)+1)
	})
}
