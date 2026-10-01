//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func labelPrecedencePost(t *testing.T, service posts.Service, subject moderation.StrongRef) *posts.PostResult {
	t.Helper()
	results, err := service.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	return results[0]
}

func labelPrecedenceJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

func requireLabelPrecedenceTombstone(t *testing.T, result *posts.PostResult, subject moderation.StrongRef) {
	t.Helper()
	require.NotNil(t, result.Moderated, "instance removal must take precedence over the active label")
	assert.Nil(t, result.Post)
	assert.Equal(t, subject.URI, result.Moderated.URI)
	require.NotNil(t, result.Moderated.Moderation)
	assert.Equal(t, moderation.ModerationStateRemoved, result.Moderated.Moderation.State)
	assert.Empty(t, result.Moderated.Moderation.ContentLabels)
	encoded := labelPrecedenceJSON(t, result)
	assert.Contains(t, string(encoded), `"$type":"social.coves.community.post.defs#moderatedPost"`)
	for _, field := range []string{`"contentLabels"`, `"record"`, `"title"`, `"content"`} {
		assert.NotContains(t, string(encoded), field, "the tombstone must not leak classification or author content")
	}
}

func TestModerationLabelPostGetRemovalPrecedenceAndRestore(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		retractOnRemoval bool
	}{
		{name: "label survives removal and restore"},
		{name: "retract while removed does not restore post", retractOnRemoval: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			communityDID, authorDID := moderationPostTombstoneActors(t, db)
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "private label precedence content", "")
			service := newPostgresModerationService(db)
			postService := moderationPostGetService(db, postgres.NewPostRepository(db))
			before := labelPrecedencePost(t, postService, subject)
			require.NotNil(t, before.Post, "the accepted post must be visible before mutation")
			label := labelPost(t, service, fixtures.DID("labelprecedenceadmin"), subject, "v0", "precedence-label")
			// removeIndexedModerationPost is pinned to v0; the label has advanced
			// this subject to v1, so remove with its actual expected version.
			removed, err := service.RemoveContent(t.Context(), fixtures.DID("postremovaladmin"), moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v1", IdempotencyKey: "precedence-remove",
				Reason: "social.coves.moderation.defs#reasonSpam",
			})
			require.NoError(t, err)
			require.NotNil(t, removed.Action)
			requireLabelPrecedenceTombstone(t, labelPrecedencePost(t, postService, subject), subject)
			state, err := service.GetSubjectState(t.Context(), subject.URI)
			require.NoError(t, err)
			require.NotNil(t, state)
			requirePersistedLabelState(t, *state, label.Action.ID, "v2", moderation.ModerationStateRemoved)
			require.NotNil(t, state.LocalRemoval)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)

			restorable := *removed
			if scenario.retractOnRemoval {
				retracted, err := service.RetractContentLabel(t.Context(), fixtures.DID("labelprecedenceadmin"), moderation.RetractContentLabelRequest{
					ActionID: label.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v2", IdempotencyKey: "precedence-retract",
				})
				require.NoError(t, err)
				require.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
				assert.Equal(t, moderation.ModerationStateRemoved, retracted.State.Moderation.State)
				assert.Empty(t, retracted.State.LocalLabels)
				requireLabelPrecedenceTombstone(t, labelPrecedencePost(t, postService, subject), subject)
				state, err = service.GetSubjectState(t.Context(), subject.URI)
				require.NoError(t, err)
				assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
				assert.Empty(t, state.LocalLabels)
				// The existing restore helper reads the version from its result.
				restorable.State.Version = retracted.State.Version
			}
			restoreModerationPost(t, service, subject, &restorable)
			visible := labelPrecedencePost(t, postService, subject)
			require.NotNil(t, visible.Post, "restored accepted post must serve a normal postView")
			assert.Nil(t, visible.Moderated)
			assert.Equal(t, subject.URI, visible.Post.URI)
			encoded := labelPrecedenceJSON(t, visible.Post)
			if scenario.retractOnRemoval {
				assert.Nil(t, visible.Post.Moderation)
				assert.NotContains(t, string(encoded), `"moderation"`)
			} else {
				assert.Equal(t, labelViewExpected(posts.ModerationSourceView{
					AuthorityDID: fixtures.InstanceDID(), Scope: posts.ModerationScopeView{Kind: moderation.ScopeInstance},
				}), visible.Post.Moderation)
				assert.Contains(t, string(encoded), `"contentLabels"`)
			}
		})
	}
}

func TestModerationLabelPostGetAuthorDeletionAllowsRetractWithoutReview(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "author deleted label", "")
	service := newPostgresModerationService(db)
	postService := moderationPostGetService(db, postgres.NewPostRepository(db))
	require.NotNil(t, labelPrecedencePost(t, postService, subject).Post)
	label := labelPost(t, service, fixtures.DID("labeldeletedadmin"), subject, "v0", "deleted-label")
	_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.URI)
	require.NoError(t, err)
	deletedBefore := labelPrecedencePost(t, postService, subject)
	require.NotNil(t, deletedBefore.NotFound, "author-deleted post is unavailable before retraction")
	retracted, err := service.RetractContentLabel(t.Context(), fixtures.DID("labeldeletedadmin"), moderation.RetractContentLabelRequest{
		ActionID: label.Action.ID, ExpectedVersion: "v1", IdempotencyKey: "deleted-retract",
	})
	require.NoError(t, err)
	require.NotNil(t, retracted)
	assert.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
	assert.Equal(t, moderation.RecordStateDeleted, retracted.State.RecordState)
	assert.Empty(t, retracted.State.LocalLabels)
	deletedAfter := labelPrecedencePost(t, postService, subject)
	require.NotNil(t, deletedAfter.NotFound)
	assert.Nil(t, deletedAfter.Post)
	assert.Nil(t, deletedAfter.Moderated)
}
