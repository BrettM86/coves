package actor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// mockPostService implements posts.Service for testing
type mockPostService struct {
	getAuthorPostsFunc func(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error)
}

func (m *mockPostService) GetAuthorPosts(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
	if m.getAuthorPostsFunc != nil {
		return m.getAuthorPostsFunc(ctx, req)
	}
	return &posts.GetAuthorPostsResponse{
		Feed:   []*posts.FeedViewPost{},
		Cursor: nil,
	}, nil
}

func (m *mockPostService) CreatePost(ctx context.Context, req posts.CreatePostRequest) (*posts.CreatePostResponse, error) {
	return nil, nil
}

// mockUserService implements users.UserService for testing
type mockUserService struct {
	resolveHandleToDIDFunc func(ctx context.Context, handle string) (string, error)
}

func (m *mockUserService) CreateUser(ctx context.Context, req users.CreateUserRequest) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) GetUserByDID(ctx context.Context, did string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) GetUserByHandle(ctx context.Context, handle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	if m.resolveHandleToDIDFunc != nil {
		return m.resolveHandleToDIDFunc(ctx, handle)
	}
	return "did:plc:testuser", nil
}

func (m *mockUserService) RegisterAccount(ctx context.Context, req users.RegisterAccountRequest) (*users.RegisterAccountResponse, error) {
	return nil, nil
}

func (m *mockUserService) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	return nil
}

func (m *mockUserService) GetProfile(ctx context.Context, did string) (*users.ProfileViewDetailed, error) {
	return nil, nil
}

func (m *mockUserService) DeleteAccount(ctx context.Context, did string) error {
	return nil
}

func (m *mockUserService) UpdateProfile(ctx context.Context, did string, displayName, bio, avatarCID, bannerCID *string) (*users.User, error) {
	return nil, nil
}

// mockVoteService implements votes.Service for testing
type mockVoteService struct{}

func (m *mockVoteService) CreateVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.CreateVoteRequest) (*votes.CreateVoteResponse, error) {
	return nil, nil
}

func (m *mockVoteService) DeleteVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.DeleteVoteRequest) error {
	return nil
}

func (m *mockVoteService) EnsureCachePopulated(ctx context.Context, session *oauthlib.ClientSessionData) error {
	return nil
}

func (m *mockVoteService) GetViewerVote(userDID, subjectURI string) *votes.CachedVote {
	return nil
}

func (m *mockVoteService) GetViewerVotesForSubjects(userDID string, subjectURIs []string) map[string]*votes.CachedVote {
	return nil
}

// mockBlueskyService implements blueskypost.Service for testing
type mockBlueskyService struct{}

func (m *mockBlueskyService) ResolvePost(ctx context.Context, atURI string) (*blueskypost.BlueskyPostResult, error) {
	return nil, nil
}

func (m *mockBlueskyService) ParseBlueskyURL(ctx context.Context, url string) (string, error) {
	return "", nil
}

func (m *mockBlueskyService) IsBlueskyURL(url string) bool {
	return false
}

func TestGetPostsHandler_Success(t *testing.T) {
	mockPosts := &mockPostService{
		getAuthorPostsFunc: func(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
			return &posts.GetAuthorPostsResponse{
				Feed: []*posts.FeedViewPost{
					{
						Post: &posts.PostView{
							URI: "at://did:plc:testuser/social.coves.community.post/abc123",
							CID: "bafytest123",
						},
					},
				},
			}, nil
		},
	}
	mockUsers := &mockUserService{}
	mockVotes := &mockVoteService{}
	mockBluesky := &mockBlueskyService{}

	handler := NewGetPostsHandler(mockPosts, mockUsers, mockVotes, mockBluesky)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=did:plc:testuser", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	var response posts.GetAuthorPostsResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(response.Feed) != 1 {
		t.Errorf("Expected 1 post in feed, got %d", len(response.Feed))
	}
}

func TestGetPostsHandler_MissingActorParameter(t *testing.T) {
	handler := NewGetPostsHandler(&mockPostService{}, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "InvalidRequest" {
		t.Errorf("Expected error 'InvalidRequest', got '%s'", response.Error)
	}
}

func TestGetPostsHandler_InvalidLimitParameter(t *testing.T) {
	handler := NewGetPostsHandler(&mockPostService{}, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=did:plc:test&limit=abc", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "InvalidRequest" {
		t.Errorf("Expected error 'InvalidRequest', got '%s'", response.Error)
	}
}

func TestGetPostsHandler_ActorNotFound(t *testing.T) {
	mockUsers := &mockUserService{
		resolveHandleToDIDFunc: func(ctx context.Context, handle string) (string, error) {
			return "", posts.ErrActorNotFound
		},
	}

	handler := NewGetPostsHandler(&mockPostService{}, mockUsers, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=nonexistent.user", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", rec.Code)
	}
}

func TestGetPostsHandler_ActorLengthExceedsMax(t *testing.T) {
	handler := NewGetPostsHandler(&mockPostService{}, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	// Create an actor parameter that exceeds 2048 characters using valid URL characters
	longActorBytes := make([]byte, 2100)
	for i := range longActorBytes {
		longActorBytes[i] = 'a'
	}
	longActor := "did:plc:" + string(longActorBytes)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor="+longActor, nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rec.Code)
	}
}

func TestGetPostsHandler_InvalidCursor(t *testing.T) {
	mockPosts := &mockPostService{
		getAuthorPostsFunc: func(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
			return nil, posts.ErrInvalidCursor
		},
	}

	handler := NewGetPostsHandler(mockPosts, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=did:plc:test&cursor=invalid", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "InvalidCursor" {
		t.Errorf("Expected error 'InvalidCursor', got '%s'", response.Error)
	}
}

func TestGetPostsHandler_MethodNotAllowed(t *testing.T) {
	handler := NewGetPostsHandler(&mockPostService{}, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.getPosts", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", rec.Code)
	}
}

func TestGetPostsHandler_HandleResolution(t *testing.T) {
	resolvedDID := ""
	mockPosts := &mockPostService{
		getAuthorPostsFunc: func(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
			resolvedDID = req.ActorDID
			return &posts.GetAuthorPostsResponse{Feed: []*posts.FeedViewPost{}}, nil
		},
	}
	mockUsers := &mockUserService{
		resolveHandleToDIDFunc: func(ctx context.Context, handle string) (string, error) {
			if handle == "test.user" {
				return "did:plc:resolveduser123", nil
			}
			return "", posts.ErrActorNotFound
		},
	}

	handler := NewGetPostsHandler(mockPosts, mockUsers, &mockVoteService{}, &mockBlueskyService{})

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=test.user", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if resolvedDID != "did:plc:resolveduser123" {
		t.Errorf("Expected resolved DID 'did:plc:resolveduser123', got '%s'", resolvedDID)
	}
}

func TestGetPostsHandler_DirectDIDPassthrough(t *testing.T) {
	receivedDID := ""
	mockPosts := &mockPostService{
		getAuthorPostsFunc: func(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
			receivedDID = req.ActorDID
			return &posts.GetAuthorPostsResponse{Feed: []*posts.FeedViewPost{}}, nil
		},
	}

	handler := NewGetPostsHandler(mockPosts, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{})

	// When actor is already a DID, it should pass through without resolution
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getPosts?actor=did:plc:directuser", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetPosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if receivedDID != "did:plc:directuser" {
		t.Errorf("Expected DID 'did:plc:directuser', got '%s'", receivedDID)
	}
}
