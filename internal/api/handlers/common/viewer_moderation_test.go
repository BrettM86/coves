package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

const viewerModerationInstanceDID = "did:web:coves.example"

func TestPopulateViewerModeration_PreservesVoteState(t *testing.T) {
	authority := moderation.NewAllowlistAuthority([]string{"did:plc:viewer"})
	direction, voteURI := "down", "at://did:plc:viewer/social.coves.feed.vote/existing"

	t.Run("post", func(t *testing.T) {
		post := &posts.PostView{
			URI:    "at://did:plc:author/social.coves.community.postv2/live",
			Viewer: &posts.ViewerState{Vote: &direction, VoteURI: &voteURI},
		}
		PopulateViewerModeration(authenticatedRequest(), authority, viewerModerationInstanceDID, []voteFeedPost{{post}})
		require.NotNil(t, post.Viewer)
		require.Equal(t, []string{"remove", "label"}, post.Viewer.Moderation)
		require.Equal(t, &direction, post.Viewer.Vote, "moderation hydration erased viewer.vote")
		require.Equal(t, &voteURI, post.Viewer.VoteURI, "moderation hydration erased viewer.voteUri")
	})
	t.Run("comment", func(t *testing.T) {
		view := &comments.CommentView{
			URI:    "at://did:plc:commenter/social.coves.community.comment/live",
			Viewer: &comments.CommentViewerState{Vote: &direction, VoteURI: &voteURI},
		}
		PopulateCommentViewerModeration(authenticatedRequest(), authority, viewerModerationInstanceDID, []*comments.CommentView{view})
		require.NotNil(t, view.Viewer)
		require.Equal(t, []string{"remove"}, view.Viewer.Moderation)
		require.Equal(t, &direction, view.Viewer.Vote, "moderation hydration erased viewer.vote")
		require.Equal(t, &voteURI, view.Viewer.VoteURI, "moderation hydration erased viewer.voteUri")
	})
}

func TestPopulateViewerModeration_NonAdminViewersGetNoActions(t *testing.T) {
	adminAuthority := moderation.NewAllowlistAuthority([]string{"did:plc:viewer"})
	otherAdminAuthority := moderation.NewAllowlistAuthority([]string{"did:plc:someoneelse"})
	removedByInstance := posts.ModerationView{
		State: "removed",
		Sources: []posts.ModerationSourceView{{
			AuthorityDID: viewerModerationInstanceDID,
			Scope:        posts.ModerationScopeView{Kind: "instance"},
		}},
	}

	for _, tc := range []struct {
		name      string
		request   *http.Request
		authority moderation.Authority
	}{
		{name: "signed-out viewer", request: httptest.NewRequest(http.MethodGet, "/", nil), authority: adminAuthority},
		{name: "signed-in non-admin viewer", request: authenticatedRequest(), authority: otherAdminAuthority},
		{name: "nil authority", request: authenticatedRequest(), authority: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post := &posts.PostView{URI: "at://did:plc:author/social.coves.community.postv2/live"}
			PopulateViewerModeration(tc.request, tc.authority, viewerModerationInstanceDID, []voteFeedPost{{post}})
			require.Nil(t, post.Viewer, "a live post must carry no viewer state for this viewer")

			tombstoneModeration := removedByInstance
			tombstone := &posts.ModeratedPost{Moderation: &tombstoneModeration}
			PopulateModeratedPostViewerModeration(tc.request, tc.authority, viewerModerationInstanceDID, []*posts.ModeratedPost{tombstone})
			require.Nil(t, tombstone.Viewer, "a removal tombstone must carry no viewer state for this viewer")

			comment := &comments.CommentView{URI: "at://did:plc:commenter/social.coves.community.comment/live"}
			PopulateCommentViewerModeration(tc.request, tc.authority, viewerModerationInstanceDID, []*comments.CommentView{comment})
			require.Nil(t, comment.Viewer, "a live comment must carry no viewer state for this viewer")
		})
	}
}
