package post

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleGetServesCommunityViewerModeration proves post.get hydrates
// viewer.communityModeration on each union member a community moderator can
// act on: the live post, the community removal tombstone (restore only), and
// the instance removal tombstone (from the post's community admission status).
// Which actions apply is owned by the moderation rule tests.
func TestHandleGetServesCommunityViewerModeration(t *testing.T) {
	const (
		instanceDID  = "did:web:instance.test"
		communityDID = "did:plc:postgetcommunity"
		moderatorDID = "did:plc:postgetmoderator"
		authorDID    = "did:plc:postgetauthor"
		uri          = "at://did:plc:postgetauthor/social.coves.community.post/3lpostget"
	)
	viewer := moderationtest.CommunityViewer{Authority: map[string]map[string]moderation.CommunityViewerAuthority{
		communityDID: {moderatorDID: moderation.CommunityAuthorityModerator},
	}}
	community := func() *posts.CommunityRef {
		return &posts.CommunityRef{DID: communityDID, Handle: "community.test", Name: "community"}
	}
	livePost := func() *posts.PostResult {
		return &posts.PostResult{Post: &posts.PostView{
			URI: uri, CID: "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", Status: "accepted",
			Author: &posts.AuthorView{DID: authorDID, Handle: "author.test"}, Community: community(),
		}}
	}
	communityRemoved := func() *posts.PostResult {
		return &posts.PostResult{Removed: &posts.RemovedPost{URI: uri, Removed: true, Code: "rule-violation", CommunityDID: communityDID}}
	}
	instanceRemoved := func() *posts.PostResult {
		return &posts.PostResult{Moderated: &posts.ModeratedPost{
			URI: uri, AuthorDID: authorDID, Community: community(), CommunityStatus: "accepted",
			Moderation: &posts.ModerationView{State: "removed", Sources: []posts.ModerationSourceView{{
				AuthorityDID: instanceDID, Scope: posts.ModerationScopeView{Kind: "instance"},
			}}},
		}}
	}

	for _, test := range []struct {
		name       string
		result     func() *posts.PostResult
		viewer     string
		wantViewer any // the served item's viewer, or nil when absent
		// wantCommunityModeration, when set, is asserted on viewer.communityModeration alone.
		wantCommunityModeration []any
	}{
		{
			name: "live accepted post, moderator", result: livePost, viewer: moderatorDID,
			wantCommunityModeration: []any{"remove", "ban-author"},
		},
		{
			name: "community-removed post, moderator", result: communityRemoved, viewer: moderatorDID,
			wantViewer: map[string]any{"communityModeration": []any{"restore"}},
		},
		{name: "community-removed post, signed out", result: communityRemoved, viewer: ""},
		{
			name: "instance-removed post accepted in its community, moderator", result: instanceRemoved, viewer: moderatorDID,
			wantViewer: map[string]any{"communityModeration": []any{"remove", "ban-author"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewGetHandler(&mockGetPostService{getPostsFunc: func(context.Context, posts.GetPostsRequest) ([]*posts.PostResult, error) {
				return []*posts.PostResult{test.result()}, nil
			}}, nil, nil).
				WithViewerModeration(moderation.NewAllowlistAuthority(nil), instanceDID).
				WithCommunityViewerModeration(viewer)
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.post.get?"+url.Values{"uris": {uri}}.Encode(), nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			recorder := httptest.NewRecorder()

			handler.HandleGet(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var body struct {
				Posts []map[string]any `json:"posts"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Len(t, body.Posts, 1)
			item := body.Posts[0]
			if test.wantCommunityModeration != nil {
				viewerState, ok := item["viewer"].(map[string]any)
				require.True(t, ok, "viewer state missing: %s", recorder.Body.String())
				assert.Equal(t, test.wantCommunityModeration, viewerState["communityModeration"])
				return
			}
			if test.wantViewer == nil {
				assert.NotContains(t, item, "viewer", "a signed-out request carries no viewer state")
				return
			}
			assert.Equal(t, test.wantViewer, item["viewer"])
		})
	}
}
