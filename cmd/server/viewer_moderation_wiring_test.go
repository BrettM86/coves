package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"
	"Coves/internal/core/comments"
	"Coves/internal/core/media"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	viewerModerationInstanceDID = "did:web:coves.test"
	viewerModerationAdminDID    = "did:plc:viewermoderationadmin"
	// viewerModerationCommunityDID is the community the served posts and
	// comments belong to.
	viewerModerationCommunityDID = "did:plc:viewermoderationcommunity"
)

// viewerModerationSessions returns the OAuth fakes holding one sealed session
// for did, and that session's bearer token.
func viewerModerationSessions(did string) (wiringSessionUnsealer, *wiringOAuthStore, string) {
	token := "session-for-" + did
	unsealer := wiringSessionUnsealer{token: {DID: did, SessionID: "browser", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	store := &wiringOAuthStore{sessions: map[string]*indigooauth.ClientSessionData{
		did + ":browser": {AccountDID: syntax.DID(did), SessionID: "browser"},
	}}
	return unsealer, store, token
}

// oneLiveCommentService answers every getComments and actor.getComments with a
// single live comment, by an author with no role, in a thread of
// viewerModerationCommunityDID.
type oneLiveCommentService struct{ comments.Service }

func oneLiveComment() *comments.CommentView {
	return &comments.CommentView{
		URI:          "at://did:plc:commentauthor/social.coves.community.comment/3lcommentlive",
		CID:          "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		Author:       &posts.AuthorView{DID: "did:plc:commentauthor", Handle: "commentauthor.test"},
		Record:       map[string]any{"$type": "social.coves.community.comment", "content": "live comment"},
		Post:         &comments.CommentRef{URI: "at://did:plc:postauthor/social.coves.community.post/3lpost", CID: "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"},
		Stats:        &comments.CommentStats{},
		CreatedAt:    "2026-10-04T00:00:00Z",
		IndexedAt:    "2026-10-04T00:00:00Z",
		CommunityDID: viewerModerationCommunityDID,
	}
}

func (oneLiveCommentService) GetComments(context.Context, *comments.GetCommentsRequest) (*comments.GetCommentsResponse, error) {
	return &comments.GetCommentsResponse{Comments: []*comments.ThreadViewComment{{Comment: oneLiveComment()}}}, nil
}

func (oneLiveCommentService) GetActorComments(context.Context, *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
	return &comments.GetActorCommentsResponse{Comments: []*comments.CommentView{oneLiveComment()}}, nil
}

// TestRegisterCommentQueryRouteHydratesViewerModeration proves the getComments
// handler cmd/server builds receives the instance admin authority and the
// instance DID. Which actions apply is owned by the moderation rule tests.
func TestRegisterCommentQueryRouteHydratesViewerModeration(t *testing.T) {
	unsealer, store, adminToken := viewerModerationSessions(viewerModerationAdminDID)
	app := &application{
		cfg:                 &config.Config{Instance: config.InstanceConfig{DID: viewerModerationInstanceDID}},
		commentService:      oneLiveCommentService{},
		authMiddleware:      middleware.NewOAuthAuthMiddleware(unsealer, store),
		moderationAuthority: moderation.NewAllowlistAuthority([]string{viewerModerationAdminDID}),
	}
	router := chi.NewRouter()
	registerCommentQueryRoute(router, app)

	request := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.comment.getComments?post=at://did:plc:postauthor/social.coves.community.post/3lpost", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	var body struct {
		Comments []struct {
			Comment struct {
				Viewer *struct {
					Moderation []string `json:"moderation"`
				} `json:"viewer"`
			} `json:"comment"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Comments, 1)
	if assert.NotNil(t, body.Comments[0].Comment.Viewer, "admin viewer must get viewer state on a live comment") {
		assert.Equal(t, []string{"remove"}, body.Comments[0].Comment.Viewer.Moderation)
	}
}

// unreachableUserBlockService satisfies NewBlockHandler's non-nil guard; no
// request in this file reaches it.
type unreachableUserBlockService struct{ userblocks.Service }

// unreachableUserService satisfies the Bluesky import handlers' non-nil guards;
// no request in this package's route wiring tests reaches it. The zero
// CommunityStateService these partial applications carry plays the same role
// for the getCommunityState route.
type unreachableUserService struct{ users.UserService }

// TestRegisterXRPCRoutesServesGetViewerAuthority proves the production XRPC
// router mounts social.coves.actor.getViewerAuthority with the application's
// authority. Services the route does not use stay nil.
func TestRegisterXRPCRoutesServesGetViewerAuthority(t *testing.T) {
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	unsealer, store, adminToken := viewerModerationSessions(viewerModerationAdminDID)
	authority := moderation.NewAllowlistAuthority([]string{viewerModerationAdminDID})
	clientConfig := indigooauth.NewLocalhostConfig("http://127.0.0.1:0/oauth/callback", []string{"atproto"})
	app := &application{
		cfg:                   &config.Config{Instance: config.InstanceConfig{DID: viewerModerationInstanceDID}},
		oauthClient:           &oauth.OAuthClient{ClientApp: indigooauth.NewClientApp(&clientConfig, nil)},
		authMiddleware:        middleware.NewOAuthAuthMiddleware(unsealer, store),
		instanceAdminAuth:     middleware.NewInstanceAdminMiddleware(unsealer, store, nil, authority),
		moderationAuthority:   authority,
		mediaPipeline:         pipeline,
		userBlockService:      unreachableUserBlockService{},
		userService:           unreachableUserService{},
		communityStateService: &moderation.CommunityStateService{},
	}
	router := chi.NewRouter()
	registerXRPCRoutes(router, app)

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+"/xrpc/social.coves.actor.getViewerAuthority", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, response.StatusCode, "body: %s", body)
	assert.JSONEq(t, `{"instanceAdmin":true}`, string(body))
}

// TestRegisterXRPCRoutesPanicsWithoutViewerModerationWiring proves a server
// whose moderation authority or instance DID was never wired fails while
// building its routes, rather than serving reads that silently omit
// viewer.moderation.
func TestRegisterXRPCRoutesPanicsWithoutViewerModerationWiring(t *testing.T) {
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	for _, test := range []struct {
		name        string
		authority   moderation.Authority
		instanceDID string
	}{
		{name: "nil authority", authority: nil, instanceDID: viewerModerationInstanceDID},
		{name: "empty instance DID", authority: moderation.NewAllowlistAuthority(nil), instanceDID: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			unsealer, store, _ := viewerModerationSessions(viewerModerationAdminDID)
			clientConfig := indigooauth.NewLocalhostConfig("http://127.0.0.1:0/oauth/callback", []string{"atproto"})
			app := &application{
				cfg:                   &config.Config{Instance: config.InstanceConfig{DID: test.instanceDID}},
				oauthClient:           &oauth.OAuthClient{ClientApp: indigooauth.NewClientApp(&clientConfig, nil)},
				authMiddleware:        middleware.NewOAuthAuthMiddleware(unsealer, store),
				instanceAdminAuth:     middleware.NewInstanceAdminMiddleware(unsealer, store, nil, moderation.NewAllowlistAuthority(nil)),
				moderationAuthority:   test.authority,
				mediaPipeline:         pipeline,
				userBlockService:      unreachableUserBlockService{},
				userService:           unreachableUserService{},
				communityStateService: &moderation.CommunityStateService{},
			}
			assert.Panics(t, func() { registerXRPCRoutes(chi.NewRouter(), app) })
		})
	}
}
