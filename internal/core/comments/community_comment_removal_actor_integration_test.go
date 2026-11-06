//go:build integration

package comments_test

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An author's profile activity keeps the comments a community removed, so the
// author can see what was taken down and by whom; nobody else sees them there.
// An instance removal is different: it stays hidden from the author too.
func TestCommunityCommentRemovalActorCommentsVisibleOnlyToAuthor(t *testing.T) {
	db, commentService, moderationService, postURI, communityRemoved, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	communityDID := post.CommunityDID
	instanceRemovedURI := postRemovalComment(t, db, authorDID, postURI, post.CID, "removed by the instance", time.Now().Add(-time.Minute))
	visibleURI := postRemovalComment(t, db, authorDID, postURI, post.CID, "never moderated", time.Now().Add(-2*time.Minute))

	// The community's removal, as the consumer records an inherited decision.
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, subject_root_uri, observed_cid,
		     action, origin, created_at)
		VALUES ('ccr-actor-apply', $1, $1, 'community', $1, $2, 'social.coves.community.comment', $1, $3, $4,
		        'apply-removal', 'inherited', NOW())
	`, communityDID, communityRemoved.URI, postURI, communityRemoved.CID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'community', $1, $2, 'removal', NULL, 'ccr-actor-apply', TRUE)
	`, communityDID, communityRemoved.URI)
	require.NoError(t, err)

	removed, err := moderationService.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "ccractoradmin")), moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: instanceRemovedURI, CID: moderatedCommentCID},
		ExpectedVersion: "v0", IdempotencyKey: "ccr-actor-instance-remove", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)

	list := func(viewerDID *string) map[string]*comments.CommentView {
		t.Helper()
		response, err := commentService.GetActorComments(t.Context(), &comments.GetActorCommentsRequest{
			ActorDID: authorDID, Limit: 50, ViewerDID: viewerDID,
		})
		require.NoError(t, err)
		byURI := map[string]*comments.CommentView{}
		for _, view := range response.Comments {
			byURI[view.URI] = view
		}
		return byURI
	}

	t.Run("author sees the community-removed comment with its removal source", func(t *testing.T) {
		got := list(&authorDID)
		require.Contains(t, got, visibleURI, "fixture: the unmoderated comment is listed")
		assert.NotContains(t, got, instanceRemovedURI, "an instance-removed comment stays hidden from its author")
		require.Contains(t, got, communityRemoved.URI, "the author's own community-removed comment is listed for the author")

		view := got[communityRemoved.URI]
		assert.False(t, view.IsDeleted)
		encoded, err := json.Marshal(view)
		require.NoError(t, err)
		var served struct {
			Record struct {
				Content string `json:"content"`
			} `json:"record"`
			Moderation *struct {
				State   string `json:"state"`
				Sources []struct {
					AuthorityDID string `json:"authorityDid"`
					Scope        struct {
						Kind         string `json:"kind"`
						CommunityDID string `json:"communityDid"`
					} `json:"scope"`
				} `json:"sources"`
			} `json:"moderation"`
		}
		require.NoError(t, json.Unmarshal(encoded, &served))
		assert.Equal(t, "private original moderation text", served.Record.Content, "the author keeps the comment's content")
		require.NotNil(t, served.Moderation, "the removal must be attributed: %s", encoded)
		assert.Equal(t, "removed", served.Moderation.State)
		require.Len(t, served.Moderation.Sources, 1, "served: %s", encoded)
		assert.Equal(t, communityDID, served.Moderation.Sources[0].AuthorityDID)
		assert.Equal(t, "community", served.Moderation.Sources[0].Scope.Kind)
		assert.Equal(t, communityDID, served.Moderation.Sources[0].Scope.CommunityDID)
	})

	otherDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "ccractorviewer"))
	for name, viewer := range map[string]*string{"another signed-in viewer": &otherDID, "anonymous viewer": nil} {
		t.Run(name+" does not see removed comments", func(t *testing.T) {
			got := list(viewer)
			require.Contains(t, got, visibleURI, "fixture: the unmoderated comment is listed")
			assert.NotContains(t, got, communityRemoved.URI)
			assert.NotContains(t, got, instanceRemovedURI)
		})
	}
}
