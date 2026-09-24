//go:build integration

package postgres_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderationImageCIDOne = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	moderationImageCIDTwo = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	moderationCommentCID  = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
)

func moderationTwoImageEmbed() string {
	return fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":10},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":10},"alt":""}]}`, moderationImageCIDOne, moderationImageCIDTwo)
}

func indexedModerationComment(t *testing.T, db *sql.DB, indexedRoot bool, embed string) (moderation.StrongRef, string, string) {
	t.Helper()
	authorName := testkit.UniqueIDWithPrefix(t, "modauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityDID := ""
	rootURI := "at://" + fixtures.DID(testkit.UniqueIDWithPrefix(t, "modroot")) + "/" + moderation.PostV2Collection + "/" + testkit.TID()
	rootCID := "bafyreiunindexedroot"
	if indexedRoot {
		communityName := testkit.UniqueIDWithPrefix(t, "modcommunity")
		var err error
		communityDID, err = fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
		require.NoError(t, err)
		rootURI = fixtures.Post(t, db, communityDID, authorDID, "moderated post", 0, time.Now())
		post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), rootURI)
		require.NoError(t, err)
		rootCID = post.CID
	}
	rkey := testkit.TID()
	subject := moderation.StrongRef{
		URI: "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey,
		CID: moderationCommentCID,
	}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'original indexed content', NULLIF($7, '')::jsonb, NOW())
	`, subject.URI, subject.CID, rkey, authorDID, rootURI, rootCID, embed)
	require.NoError(t, err)
	return subject, authorDID, communityDID
}

func newPostgresModerationService(db *sql.DB) moderation.Service {
	return moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
}

func TestModerationRepositoryPersistsRemovalAndAssociation(t *testing.T) {
	for _, test := range []struct {
		name        string
		indexedRoot bool
		embed       string
	}{
		{name: "indexed root captures community and image blocks", indexedRoot: true, embed: moderationTwoImageEmbed()},
		{name: "unindexed root has no community association", indexedRoot: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			subject, authorDID, communityDID := indexedModerationComment(t, db, test.indexedRoot, test.embed)
			service := newPostgresModerationService(db)
			adminA := fixtures.DID(testkit.UniqueIDWithPrefix(t, "modadmina"))
			adminB := fixtures.DID(testkit.UniqueIDWithPrefix(t, "modadminb"))
			request := moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-a",
				Reason: "social.coves.moderation.defs#reasonSpam", PrivateNote: "reviewed report",
			}

			removed, err := service.RemoveContent(t.Context(), adminA, request)
			require.NoError(t, err)
			require.NotNil(t, removed)
			require.NotNil(t, removed.Action)
			assert.Equal(t, moderation.OutcomeApplied, removed.Outcome)
			assert.Equal(t, "v1", removed.State.Version)
			assert.Equal(t, moderation.ModerationStateRemoved, removed.State.Moderation.State)
			require.NotNil(t, removed.State.LocalRemoval)
			assert.Equal(t, removed.Action.ID, removed.State.LocalRemoval.ActionID)

			var actionCount int
			var actor, authority, scope, collection, observedCID, actionKind, reason, note, origin string
			var associatedCommunity sql.NullString
			err = db.QueryRowContext(t.Context(), `
				SELECT actor_did, authority_did, scope_kind, subject_collection,
				       subject_community_did, observed_cid, action, reason, private_note, origin
				FROM moderation_actions WHERE subject_uri = $1 AND id = $2
			`, subject.URI, removed.Action.ID).Scan(
				&actor, &authority, &scope, &collection, &associatedCommunity,
				&observedCID, &actionKind, &reason, &note, &origin,
			)
			require.NoError(t, err)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, subject.URI).Scan(&actionCount))
			assert.Equal(t, 1, actionCount)
			assert.Equal(t, adminA, actor)
			assert.Equal(t, fixtures.InstanceDID(), authority)
			assert.Equal(t, moderation.ScopeInstance, scope)
			assert.Equal(t, moderation.CommentCollection, collection)
			assert.Equal(t, sql.NullString{String: communityDID, Valid: test.indexedRoot}, associatedCommunity)
			assert.Equal(t, subject.CID, observedCID)
			assert.Equal(t, moderation.ActionRemove, actionKind)
			assert.Equal(t, request.Reason, reason)
			assert.Equal(t, request.PrivateNote, note)
			assert.Equal(t, moderation.OriginLocal, origin)

			var version int64
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT version FROM moderation_subjects WHERE subject_uri = $1`, subject.URI).Scan(&version))
			assert.EqualValues(t, 1, version)
			var decisionCount int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1`, subject.URI).Scan(&decisionCount))
			assert.Equal(t, 1, decisionCount)
			var activeAction string
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT active_action_id FROM moderation_decisions
				WHERE subject_uri = $1 AND authority_did = $2 AND scope_kind = 'instance' AND kind = 'removal' AND active
			`, subject.URI, fixtures.InstanceDID()).Scan(&activeAction))
			assert.Equal(t, removed.Action.ID, activeAction)
			var keyCount int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys`).Scan(&keyCount))
			assert.Equal(t, 1, keyCount)
			var storedKeyCount int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1 AND authority_did = $2 AND key = $3`, adminA, fixtures.InstanceDID(), "remove-a").Scan(&storedKeyCount))
			assert.Equal(t, 1, storedKeyCount)

			var blockCount int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_media_blocks WHERE action_id = $1`, removed.Action.ID).Scan(&blockCount))
			if test.indexedRoot {
				assert.Equal(t, 2, blockCount)
				rows, err := db.QueryContext(t.Context(), `SELECT owner_did, blob_cid, action_id, active FROM moderation_media_blocks WHERE action_id = $1`, removed.Action.ID)
				require.NoError(t, err)
				seen := make(map[string]bool)
				for rows.Next() {
					var owner sql.NullString
					var blobCID, actionID string
					var active bool
					require.NoError(t, rows.Scan(&owner, &blobCID, &actionID, &active))
					assert.Equal(t, sql.NullString{String: authorDID, Valid: true}, owner, "spam blocks only the comment owner's blob")
					assert.Equal(t, removed.Action.ID, actionID)
					assert.True(t, active)
					seen[blobCID] = true
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				assert.Equal(t, map[string]bool{moderationImageCIDOne: true, moderationImageCIDTwo: true}, seen)
			} else {
				assert.Zero(t, blockCount)
			}

			var originalContent, indexedCID string
			var storedEmbed sql.NullString
			var deletedAt sql.NullTime
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT content, cid, embed::text, deleted_at FROM comments WHERE uri = $1`, subject.URI).Scan(&originalContent, &indexedCID, &storedEmbed, &deletedAt))
			assert.Equal(t, "original indexed content", originalContent, "removal must overlay, not alter the indexed row")
			assert.Equal(t, subject.CID, indexedCID)
			if test.indexedRoot {
				assert.JSONEq(t, test.embed, storedEmbed.String)
			}
			assert.False(t, deletedAt.Valid)

			state, err := service.GetSubjectState(t.Context(), subject.URI)
			require.NoError(t, err)
			assert.Equal(t, "v1", state.Version)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			require.NotNil(t, state.LocalRemoval)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)

			request.ExpectedVersion = "v1"
			request.IdempotencyKey = "remove-b"
			unchanged, err := service.RemoveContent(t.Context(), adminB, request)
			require.NoError(t, err)
			require.NotNil(t, unchanged)
			assert.Equal(t, moderation.OutcomeUnchanged, unchanged.Outcome)
			assert.Nil(t, unchanged.Action)
			assert.Equal(t, "v1", unchanged.State.Version)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, subject.URI).Scan(&actionCount))
			assert.Equal(t, 1, actionCount)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT version FROM moderation_subjects WHERE subject_uri = $1`, subject.URI).Scan(&version))
			assert.EqualValues(t, 1, version)
		})
	}
}

func TestModerationRepositoryRollsBackAfterActionInsertFailure(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, moderationTwoImageEmbed())
	_, err := db.ExecContext(t.Context(), `
		CREATE FUNCTION reject_moderation_decision() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected decision failure after action insert';
		END;
		$$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		CREATE TRIGGER reject_moderation_decision BEFORE INSERT ON moderation_decisions
		FOR EACH ROW EXECUTE FUNCTION reject_moderation_decision();
	`)
	require.NoError(t, err)
	result, err := newPostgresModerationService(db).RemoveContent(t.Context(), fixtures.DID("modrollback"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "rollback-key",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.ErrorContains(t, err, "injected decision failure after action insert")
	assert.Nil(t, result)
	for _, test := range []struct {
		name  string
		query string
	}{
		{"action", `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`},
		{"decision", `SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1`},
		{"version greater than zero", `SELECT count(*) FROM moderation_subjects WHERE subject_uri = $1 AND version > 0`},
		{"idempotency key", `SELECT count(*) FROM moderation_idempotency_keys WHERE key = 'rollback-key' AND stored_result IS NOT NULL`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var count int
			if test.name == "idempotency key" {
				require.NoError(t, db.QueryRowContext(t.Context(), test.query).Scan(&count))
			} else {
				require.NoError(t, db.QueryRowContext(t.Context(), test.query, subject.URI).Scan(&count))
			}
			assert.Zero(t, count)
		})
	}
	var mediaBlockCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_media_blocks`).Scan(&mediaBlockCount))
	assert.Zero(t, mediaBlockCount)
}

func TestModerationRepositoryRemovesAuthorDeletedCommentWithDifferentCID(t *testing.T) {
	db := testkit.DB(t)
	subject, authorDID, _ := indexedModerationComment(t, db, true, "")
	_, err := db.ExecContext(t.Context(), `
		UPDATE comments SET deleted_at = NOW(), deletion_reason = 'author', deleted_by = $1 WHERE uri = $2
	`, authorDID, subject.URI)
	require.NoError(t, err)
	lastIndexedCID := subject.CID
	subject.CID = moderationImageCIDTwo // Deliberately different from the last indexed comment CID.
	removed, err := newPostgresModerationService(db).RemoveContent(t.Context(), fixtures.DID("moddeleteadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "deleted-comment-removal",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	assert.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	assert.Equal(t, moderation.RecordStateDeleted, removed.State.RecordState)
	assert.Equal(t, moderation.ModerationStateRemoved, removed.State.Moderation.State)
	var observedCID string
	var actionCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT observed_cid FROM moderation_actions WHERE id = $1 AND subject_uri = $2 AND action = 'remove'`, removed.Action.ID, subject.URI).Scan(&observedCID))
	assert.Equal(t, lastIndexedCID, observedCID)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, subject.URI).Scan(&actionCount))
	assert.Equal(t, 1, actionCount)
	state, err := newPostgresModerationService(db).GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, moderation.RecordStateDeleted, state.RecordState)
	assert.Nil(t, state.CurrentSubject)
	assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
	require.NotNil(t, state.LocalRemoval)
	assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
}
