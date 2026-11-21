package comments

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"
	"Coves/internal/core/posts"

	core "Coves/internal/core/comments"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetComments_ServesCommunityViewerModeration proves the comment thread
// hydrates viewer.communityModeration on every comment from the viewer's
// authority in the thread's community: a live comment, a community-removed
// placeholder (served IsDeleted, as the service builds it), a comment removed
// only by the instance (not restorable by the community), an author-deleted
// placeholder (no actions) and a nested reply by a role holder. Which actions apply is owned by the moderation rule tests.
func TestGetComments_ServesCommunityViewerModeration(t *testing.T) {
	const (
		communityDID = "did:plc:commentscommunity"
		moderatorDID = "did:plc:commentsmoderator"
		authorDID    = "did:plc:commentsplainauthor"
		liveURI      = "at://did:plc:commentsplainauthor/social.coves.community.comment/live"
		removedURI   = "at://did:plc:commentsplainauthor/social.coves.community.comment/removed"
		moderatorURI = "at://did:plc:commentsmoderator/social.coves.community.comment/byamoderator"
		instanceURI  = "at://did:plc:commentsplainauthor/social.coves.community.comment/instanceremoved"
		deletedURI   = "at://did:plc:commentsplainauthor/social.coves.community.comment/authordeleted"
		instanceDID  = "did:plc:testinstance"
	)
	viewer := moderationtest.CommunityViewer{
		Authority: map[string]map[string]moderation.CommunityViewerAuthority{communityDID: {moderatorDID: moderation.CommunityAuthorityModerator}},
		Roles:     map[string]map[string]bool{communityDID: {moderatorDID: true}},
	}
	thread := func() *core.GetCommentsResponse {
		moderatorReason := core.DeletionReasonModerator
		instanceReason := core.DeletionReasonModerator
		authorReason := core.DeletionReasonAuthor
		return &core.GetCommentsResponse{
			Post: map[string]string{"uri": testPostURI},
			Comments: []*core.ThreadViewComment{
				{
					Comment: &core.CommentView{
						URI: liveURI, CommunityDID: communityDID,
						Author: &posts.AuthorView{DID: authorDID, Handle: "author.test"},
						Record: map[string]any{"$type": "social.coves.community.comment", "content": "live"},
					},
					Replies: []*core.ThreadViewComment{{Comment: &core.CommentView{
						URI: moderatorURI, CommunityDID: communityDID,
						Author: &posts.AuthorView{DID: moderatorDID, Handle: "moderator.test"},
						Record: map[string]any{"$type": "social.coves.community.comment", "content": "by a moderator"},
					}}},
				},
				{Comment: &core.CommentView{
					URI: removedURI, CommunityDID: communityDID,
					Author:    &posts.AuthorView{DID: authorDID, Handle: "handle.invalid"},
					IsDeleted: true, DeletionReason: &moderatorReason,
					Moderation: &core.ModerationView{State: "removed", Sources: []core.ModerationSourceView{{
						AuthorityDID: communityDID, Scope: core.ModerationScopeView{Kind: "community", CommunityDID: communityDID},
					}}},
				}},
				{Comment: &core.CommentView{
					URI: instanceURI, CommunityDID: communityDID,
					Author:    &posts.AuthorView{DID: authorDID, Handle: "handle.invalid"},
					IsDeleted: true, DeletionReason: &instanceReason,
					Moderation: &core.ModerationView{State: "removed", Sources: []core.ModerationSourceView{{
						AuthorityDID: instanceDID, Scope: core.ModerationScopeView{Kind: "instance"},
					}}},
				}},
				{Comment: &core.CommentView{
					URI: deletedURI, CommunityDID: communityDID,
					Author:    &posts.AuthorView{DID: authorDID, Handle: "handle.invalid"},
					IsDeleted: true, DeletionReason: &authorReason,
				}},
			},
		}
	}

	for _, test := range []struct {
		name   string
		viewer string
		want   map[string][]string // comment URI -> viewer.communityModeration; nil: absent
	}{
		{name: "moderator", viewer: moderatorDID, want: map[string][]string{
			liveURI:      {"remove", "ban-author"},
			removedURI:   {"restore", "ban-author"},
			instanceURI:  {"remove", "ban-author"},
			moderatorURI: {"remove"},
		}},
		{name: "signed out", viewer: "", want: map[string][]string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.comment.getComments?post="+testPostURI, nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			recorder := httptest.NewRecorder()

			NewGetCommentsHandler(&fakeCommentService{resp: thread()}, nil, moderation.NewAllowlistAuthority(nil), instanceDID).
				WithCommunityViewerModeration(viewer).
				HandleGetComments(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
			var got core.GetCommentsResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
			require.Len(t, got.Comments, 4)
			require.Len(t, got.Comments[0].Replies, 1)
			served := map[string]*core.CommentView{
				liveURI:      got.Comments[0].Comment,
				moderatorURI: got.Comments[0].Replies[0].Comment,
				removedURI:   got.Comments[1].Comment,
				instanceURI:  got.Comments[2].Comment,
				deletedURI:   got.Comments[3].Comment,
			}
			for uri, view := range served {
				var actions []string
				if view.Viewer != nil {
					actions = view.Viewer.CommunityModeration
				}
				assert.Equal(t, test.want[uri], actions, "%s viewer.communityModeration", uri)
			}
		})
	}
}
