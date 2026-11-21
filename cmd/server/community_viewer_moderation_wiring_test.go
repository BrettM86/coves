package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/media"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityViewerModeratorDID = "did:plc:viewermoderationmoderator"
	communityViewerPostAuthor   = "did:plc:viewermoderationpostauthor"
	communityViewerPostURI      = "at://did:plc:viewermoderationpostauthor/social.coves.community.post/3lcommunitypost"
)

// moderatorOfOneCommunity is the role authorizer of an instance hosting
// viewerModerationCommunityDID, where communityViewerModeratorDID is a usable
// moderator and nobody else holds authority.
type moderatorOfOneCommunity struct{}

func (moderatorOfOneCommunity) Authorize(_ context.Context, communityDID, callerDID, required string) (communities.RoleAuthorization, error) {
	if communityDID == viewerModerationCommunityDID && callerDID == communityViewerModeratorDID && required == communities.RoleModerator {
		return communities.RoleAuthorization{Role: communities.RoleModerator}, nil
	}
	return communities.RoleAuthorization{}, communities.ErrUnauthorized
}

func (moderatorOfOneCommunity) ViewerRole(_ context.Context, communityDID, viewerDID string) (string, error) {
	if communityDID == viewerModerationCommunityDID && viewerDID == communityViewerModeratorDID {
		return communities.RoleModerator, nil
	}
	return "", nil
}

func (moderatorOfOneCommunity) HasUsableRole(_ context.Context, viewerDID string) (bool, error) {
	return viewerDID == communityViewerModeratorDID, nil
}

func (moderatorOfOneCommunity) RequireHostedCommunity(_ context.Context, communityDID string) error {
	if communityDID == viewerModerationCommunityDID {
		return nil
	}
	return communities.ErrUnauthorized
}

// communityPostView is an accepted post in viewerModerationCommunityDID by an
// author with no role.
func communityPostView() *posts.PostView {
	return &posts.PostView{
		URI: communityViewerPostURI, CID: "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", Status: "accepted",
		Author:    &posts.AuthorView{DID: communityViewerPostAuthor, Handle: "postauthor.test"},
		Community: &posts.CommunityRef{DID: viewerModerationCommunityDID, Handle: "community.test", Name: "community"},
	}
}

// communityPostService serves communityPostView on post.get and actor.getPosts.
type communityPostService struct{ posts.Service }

func (communityPostService) GetPosts(context.Context, posts.GetPostsRequest) ([]*posts.PostResult, error) {
	return []*posts.PostResult{{Post: communityPostView()}}, nil
}

func (communityPostService) GetAuthorPosts(context.Context, posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
	return &posts.GetAuthorPostsResponse{Feed: []*posts.FeedViewPost{{Post: communityPostView()}}}, nil
}

// communityPostFeeds serves communityPostView on post search and on every
// getFeed request, so getFeed and its aliases all serve it.
type communityPostFeeds struct{}

func (communityPostFeeds) GetFeed(context.Context, feedgen.GetFeedRequest) (*feedgen.GetFeedResponse, error) {
	return &feedgen.GetFeedResponse{Feed: []*feedgen.FeedViewPost{{Post: communityPostView()}}}, nil
}

func (communityPostFeeds) SearchPosts(context.Context, communityFeeds.SearchPostsRequest) (*communityFeeds.FeedResponse, error) {
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{{Post: communityPostView()}}}, nil
}

// hostedCommunityService serves viewerModerationCommunityDID on community.get
// and resolves every community identifier to it.
type hostedCommunityService struct{ communities.Service }

func (hostedCommunityService) ResolveCommunityIdentifier(context.Context, string) (string, error) {
	return viewerModerationCommunityDID, nil
}

func (hostedCommunityService) GetCommunity(context.Context, string) (*communities.Community, error) {
	return &communities.Community{DID: viewerModerationCommunityDID, Handle: "community.test", Name: "community"}, nil
}

// unsubscribedCommunityRepo answers community.get's viewer state reads with
// no subscription and no block.
type unsubscribedCommunityRepo struct{ communities.Repository }

func (unsubscribedCommunityRepo) GetSubscribedCommunityDIDs(context.Context, string, []string) (map[string]communities.SubscriptionViewerState, error) {
	return map[string]communities.SubscriptionViewerState{}, nil
}

func (unsubscribedCommunityRepo) IsBlocked(context.Context, string, string) (bool, error) {
	return false, nil
}

// emptyDatabase answers every query with no rows, so the Postgres readers
// cmd/server builds over app.db (community.get's ban and invitation readers)
// find nothing. Transactions and writes are refused.
type emptyDatabase struct{}

var errEmptyDatabaseWrite = errors.New("viewer moderation wiring test: the database is read-only and empty")

func (emptyDatabase) Connect(context.Context) (driver.Conn, error) { return emptyDatabase{}, nil }
func (emptyDatabase) Driver() driver.Driver                        { return emptyDatabase{} }
func (emptyDatabase) Open(string) (driver.Conn, error)             { return emptyDatabase{}, nil }
func (emptyDatabase) Prepare(string) (driver.Stmt, error)          { return emptyDatabase{}, nil }
func (emptyDatabase) Close() error                                 { return nil }
func (emptyDatabase) Begin() (driver.Tx, error)                    { return nil, errEmptyDatabaseWrite }
func (emptyDatabase) NumInput() int                                { return -1 }
func (emptyDatabase) Exec([]driver.Value) (driver.Result, error)   { return nil, errEmptyDatabaseWrite }
func (emptyDatabase) Query([]driver.Value) (driver.Rows, error)    { return emptyDatabase{}, nil }
func (emptyDatabase) Columns() []string                            { return nil }
func (emptyDatabase) Next([]driver.Value) error                    { return io.EOF }

// TestRegisterXRPCRoutesServesCommunityViewerModeration proves every
// production registration of a read that serves posts, comments or a
// community hands its handler a community viewer reader built from the
// application's role authorizer: a moderator of the community gets
// viewer.communityModeration on the served item. Which actions apply is owned
// by the moderation rule tests.
func TestRegisterXRPCRoutesServesCommunityViewerModeration(t *testing.T) {
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	unsealer, store, moderatorToken := viewerModerationSessions(communityViewerModeratorDID)
	authority := moderation.NewAllowlistAuthority(nil)
	clientConfig := indigooauth.NewLocalhostConfig("http://127.0.0.1:0/oauth/callback", []string{"atproto"})
	db := sql.OpenDB(emptyDatabase{})
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	app := &application{
		cfg:                     &config.Config{Instance: config.InstanceConfig{DID: viewerModerationInstanceDID}},
		db:                      db,
		oauthClient:             &oauth.OAuthClient{ClientApp: indigooauth.NewClientApp(&clientConfig, nil)},
		authMiddleware:          middleware.NewOAuthAuthMiddleware(unsealer, store),
		instanceAdminAuth:       middleware.NewInstanceAdminMiddleware(unsealer, store, nil, authority),
		moderationAuthority:     authority,
		communityRoleAuthorizer: moderatorOfOneCommunity{},
		mediaPipeline:           pipeline,
		userBlockService:        unreachableUserBlockService{},
		userService:             unreachableUserService{},
		communityStateService:   &moderation.CommunityStateService{},
		communityService:        hostedCommunityService{},
		communityRepo:           unsubscribedCommunityRepo{},
		postService:             communityPostService{},
		commentService:          oneLiveCommentService{},
		feedService:             communityPostFeeds{},
		feedGeneratorService:    communityPostFeeds{},
		feedRegistry:            feedgen.NewRegistry(viewerModerationInstanceDID),
	}
	router := chi.NewRouter()
	registerXRPCRoutes(router, app)

	postActions := []any{"remove", "ban-author"}
	for _, test := range []struct {
		name string
		path string
		item []any // JSON path to the served item's viewer.communityModeration
		want []any
	}{
		{
			name: "post.get", path: "/xrpc/social.coves.community.post.get?uris=" + communityViewerPostURI,
			item: []any{"posts", 0, "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "comment.getComments", path: "/xrpc/social.coves.community.comment.getComments?post=" + communityViewerPostURI,
			item: []any{"comments", 0, "comment", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "communityFeed.getCommunity", path: "/xrpc/social.coves.communityFeed.getCommunity?community=" + viewerModerationCommunityDID,
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "feed.getFeed", path: "/xrpc/social.coves.feed.getFeed?scope=community&community=" + viewerModerationCommunityDID,
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "feed.searchPosts", path: "/xrpc/social.coves.feed.searchPosts?q=community",
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "feed.getTimeline", path: "/xrpc/social.coves.feed.getTimeline",
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "feed.getDiscover", path: "/xrpc/social.coves.feed.getDiscover",
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "actor.getPosts", path: "/xrpc/social.coves.actor.getPosts?actor=" + communityViewerPostAuthor,
			item: []any{"feed", 0, "post", "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "actor.getComments", path: "/xrpc/social.coves.actor.getComments?actor=did:plc:commentauthor",
			item: []any{"comments", 0, "viewer", "communityModeration"}, want: postActions,
		},
		{
			name: "community.get", path: "/xrpc/social.coves.community.get?community=" + viewerModerationCommunityDID,
			item: []any{"viewer", "communityModeration"}, want: []any{"ban-account", "unban-account"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+moderatorToken)
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
			var body any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, test.want, jsonAt(body, test.item), "viewer.communityModeration in %s", response.Body.String())
		})
	}
}

// jsonAt walks decoded JSON by object keys and array indexes, returning nil
// when the path is absent.
func jsonAt(value any, path []any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			value = object[key]
		case int:
			array, ok := value.([]any)
			if !ok || key >= len(array) {
				return nil
			}
			value = array[key]
		}
	}
	return value
}
