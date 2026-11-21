package feed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedFeedService serves one fixed page for every getFeed request.
type fixedFeedService struct{ response *feedgen.GetFeedResponse }

func (s fixedFeedService) GetFeed(context.Context, feedgen.GetFeedRequest) (*feedgen.GetFeedResponse, error) {
	return s.response, nil
}

// TestGetFeed_ServesCommunityViewerModeration proves getFeed (and so the
// community, discover and timeline aliases it serves) hydrates viewer.communityModeration per post from the viewer's authority in
// that post's community, beside the instance list, with other viewer fields
// kept. Which actions apply is owned by the moderation rule tests.
func TestGetFeed_ServesCommunityViewerModeration(t *testing.T) {
	const (
		instanceDID    = "did:web:instance.test"
		communityC     = "did:plc:feedcommunityc"
		communityD     = "did:plc:feedcommunityd"
		moderatorDID   = "did:plc:feedmoderator"
		adminDID       = "did:plc:feedadminmoderator"
		noRoleDID      = "did:plc:feednorole"
		plainAuthor    = "did:plc:feedplainauthor"
		roleAuthor     = "did:plc:feedroleauthor"
		postInC        = "at://did:plc:feedplainauthor/social.coves.community.post/inc"
		postInD        = "at://did:plc:feedplainauthor/social.coves.community.post/ind"
		roleAuthorPost = "at://did:plc:feedroleauthor/social.coves.community.post/roleauthor"
		savedURI       = "at://did:plc:feedmoderator/social.coves.feed.save/inc"
	)
	viewer := moderationtest.CommunityViewer{
		Authority: map[string]map[string]moderation.CommunityViewerAuthority{
			communityC: {moderatorDID: moderation.CommunityAuthorityModerator, adminDID: moderation.CommunityAuthorityModerator},
		},
		Roles: map[string]map[string]bool{communityC: {roleAuthor: true, moderatorDID: true, adminDID: true}},
	}
	feed := func() *feedgen.GetFeedResponse {
		saved := savedURI
		post := func(uri, author, community string) *feedgen.FeedViewPost {
			return &feedgen.FeedViewPost{Post: &posts.PostView{
				URI: uri, CID: "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", Status: "accepted",
				Author:    &posts.AuthorView{DID: author, Handle: "author.test"},
				Community: &posts.CommunityRef{DID: community, Handle: "community.test", Name: "community"},
			}}
		}
		inC := post(postInC, plainAuthor, communityC)
		inC.Post.Viewer = &posts.ViewerState{Saved: true, SavedURI: &saved}
		return &feedgen.GetFeedResponse{Feed: []*feedgen.FeedViewPost{
			inC, post(postInD, plainAuthor, communityD), post(roleAuthorPost, roleAuthor, communityC),
		}}
	}

	type want struct {
		community []any // nil: viewer.communityModeration absent
		instance  []any // nil: viewer.moderation absent
	}
	for _, test := range []struct {
		name   string
		viewer string
		want   map[string]want
	}{
		{
			name: "moderator of the post's community", viewer: moderatorDID,
			want: map[string]want{
				postInC:        {community: []any{"remove", "ban-author"}},
				postInD:        {},
				roleAuthorPost: {community: []any{"remove"}},
			},
		},
		{
			name: "signed-in viewer with no role who is not an admin", viewer: noRoleDID,
			want: map[string]want{postInC: {}, postInD: {}, roleAuthorPost: {}},
		},
		{
			name: "signed out", viewer: "",
			want: map[string]want{postInC: {}, postInD: {}, roleAuthorPost: {}},
		},
		{
			name: "instance admin who moderates the post's community", viewer: adminDID,
			want: map[string]want{
				postInC:        {community: []any{"remove", "ban-author"}, instance: []any{"remove", "label"}},
				postInD:        {instance: []any{"remove", "label"}},
				roleAuthorPost: {community: []any{"remove"}, instance: []any{"remove", "label"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewGetFeedHandler(fixedFeedService{response: feed()}, fakeResolver{known: map[string]bool{communityC: true}}, nil, nil).
				WithViewerModeration(moderation.NewAllowlistAuthority([]string{adminDID}), instanceDID).
				WithCommunityViewerModeration(viewer)
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getFeed?scope=community&community="+communityC, nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			recorder := httptest.NewRecorder()

			handler.HandleGetFeed(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
			var body struct {
				Feed []struct {
					Post struct {
						URI    string         `json:"uri"`
						Viewer map[string]any `json:"viewer"`
					} `json:"post"`
				} `json:"feed"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Len(t, body.Feed, 3)
			for _, item := range body.Feed {
				expected := test.want[item.Post.URI]
				assertActionList(t, item.Post.Viewer, "communityModeration", expected.community, item.Post.URI)
				assertActionList(t, item.Post.Viewer, "moderation", expected.instance, item.Post.URI)
				if item.Post.URI == postInC {
					assert.Equal(t, true, item.Post.Viewer["saved"], "saved state kept beside the action lists")
					assert.Equal(t, savedURI, item.Post.Viewer["savedUri"], "saved URI kept beside the action lists")
				}
			}
		})
	}
}

// assertActionList asserts viewer[key] is want, or that key is absent when
// want is nil.
func assertActionList(t *testing.T, viewer map[string]any, key string, want []any, item string) {
	t.Helper()
	if want == nil {
		assert.NotContains(t, viewer, key, "%s viewer.%s must be absent", item, key)
		return
	}
	assert.Equal(t, want, viewer[key], "%s viewer.%s", item, key)
}
