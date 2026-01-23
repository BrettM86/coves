package actor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// mockCommentService implements a comment service interface for testing
type mockCommentService struct {
	getActorCommentsFunc func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error)
}

func (m *mockCommentService) GetActorComments(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
	if m.getActorCommentsFunc != nil {
		return m.getActorCommentsFunc(ctx, req)
	}
	return &comments.GetActorCommentsResponse{
		Comments: []*comments.CommentView{},
		Cursor:   nil,
	}, nil
}

// Implement other Service methods as no-ops
func (m *mockCommentService) GetComments(ctx context.Context, req *comments.GetCommentsRequest) (*comments.GetCommentsResponse, error) {
	return nil, nil
}

func (m *mockCommentService) CreateComment(ctx context.Context, session *oauthlib.ClientSessionData, req comments.CreateCommentRequest) (*comments.CreateCommentResponse, error) {
	return nil, nil
}

func (m *mockCommentService) UpdateComment(ctx context.Context, session *oauthlib.ClientSessionData, req comments.UpdateCommentRequest) (*comments.UpdateCommentResponse, error) {
	return nil, nil
}

func (m *mockCommentService) DeleteComment(ctx context.Context, session *oauthlib.ClientSessionData, req comments.DeleteCommentRequest) error {
	return nil
}

// mockUserServiceForComments implements users.UserService for testing getComments
type mockUserServiceForComments struct {
	resolveHandleToDIDFunc func(ctx context.Context, handle string) (string, error)
}

func (m *mockUserServiceForComments) CreateUser(ctx context.Context, req users.CreateUserRequest) (*users.User, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) GetUserByDID(ctx context.Context, did string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) GetUserByHandle(ctx context.Context, handle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	if m.resolveHandleToDIDFunc != nil {
		return m.resolveHandleToDIDFunc(ctx, handle)
	}
	return "did:plc:testuser", nil
}

func (m *mockUserServiceForComments) RegisterAccount(ctx context.Context, req users.RegisterAccountRequest) (*users.RegisterAccountResponse, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	return nil
}

func (m *mockUserServiceForComments) GetProfile(ctx context.Context, did string) (*users.ProfileViewDetailed, error) {
	return nil, nil
}

func (m *mockUserServiceForComments) DeleteAccount(ctx context.Context, did string) error {
	return nil
}

func (m *mockUserServiceForComments) UpdateProfile(ctx context.Context, did string, input users.UpdateProfileInput) (*users.User, error) {
	return nil, nil
}

// mockVoteServiceForComments implements votes.Service for testing getComments
type mockVoteServiceForComments struct{}

func (m *mockVoteServiceForComments) CreateVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.CreateVoteRequest) (*votes.CreateVoteResponse, error) {
	return nil, nil
}

func (m *mockVoteServiceForComments) DeleteVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.DeleteVoteRequest) error {
	return nil
}

func (m *mockVoteServiceForComments) EnsureCachePopulated(ctx context.Context, session *oauthlib.ClientSessionData) error {
	return nil
}

func (m *mockVoteServiceForComments) GetViewerVote(userDID, subjectURI string) *votes.CachedVote {
	return nil
}

func (m *mockVoteServiceForComments) GetViewerVotesForSubjects(userDID string, subjectURIs []string) map[string]*votes.CachedVote {
	return nil
}

func TestGetCommentsHandler_Success(t *testing.T) {
	createdAt := time.Now().Format(time.RFC3339)
	indexedAt := time.Now().Format(time.RFC3339)

	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			return &comments.GetActorCommentsResponse{
				Comments: []*comments.CommentView{
					{
						URI:       "at://did:plc:testuser/social.coves.community.comment/abc123",
						CID:       "bafytest123",
						Content:   "Test comment content",
						CreatedAt: createdAt,
						IndexedAt: indexedAt,
						Author: &posts.AuthorView{
							DID:    "did:plc:testuser",
							Handle: "test.user",
						},
						Stats: &comments.CommentStats{
							Upvotes:    5,
							Downvotes:  1,
							Score:      4,
							ReplyCount: 2,
						},
					},
				},
			}, nil
		},
	}
	mockUsers := &mockUserServiceForComments{}
	mockVotes := &mockVoteServiceForComments{}

	handler := NewGetCommentsHandler(mockComments, mockUsers, mockVotes)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:testuser", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	var response comments.GetActorCommentsResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(response.Comments) != 1 {
		t.Errorf("Expected 1 comment in response, got %d", len(response.Comments))
	}

	if response.Comments[0].URI != "at://did:plc:testuser/social.coves.community.comment/abc123" {
		t.Errorf("Expected correct comment URI, got '%s'", response.Comments[0].URI)
	}

	if response.Comments[0].Content != "Test comment content" {
		t.Errorf("Expected correct comment content, got '%s'", response.Comments[0].Content)
	}
}

func TestGetCommentsHandler_MissingActor(t *testing.T) {
	handler := NewGetCommentsHandler(
		&mockCommentService{},
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

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

func TestGetCommentsHandler_InvalidLimit(t *testing.T) {
	handler := NewGetCommentsHandler(
		&mockCommentService{},
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test&limit=abc", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

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

func TestGetCommentsHandler_ActorNotFound(t *testing.T) {
	mockUsers := &mockUserServiceForComments{
		resolveHandleToDIDFunc: func(ctx context.Context, handle string) (string, error) {
			return "", posts.ErrActorNotFound
		},
	}

	handler := NewGetCommentsHandler(
		&mockCommentService{},
		mockUsers,
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=nonexistent.user", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "ActorNotFound" {
		t.Errorf("Expected error 'ActorNotFound', got '%s'", response.Error)
	}
}

func TestGetCommentsHandler_ActorLengthExceedsMax(t *testing.T) {
	handler := NewGetCommentsHandler(
		&mockCommentService{},
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	// Create an actor parameter that exceeds 2048 characters using valid URL characters
	longActorBytes := make([]byte, 2100)
	for i := range longActorBytes {
		longActorBytes[i] = 'a'
	}
	longActor := "did:plc:" + string(longActorBytes)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor="+longActor, nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rec.Code)
	}
}

func TestGetCommentsHandler_InvalidCursor(t *testing.T) {
	// The handleCommentServiceError function checks for "invalid request" in error message
	// to return a BadRequest. An invalid cursor error falls under this category.
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			return nil, errors.New("invalid request: invalid cursor format")
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test&cursor=invalid", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

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

func TestGetCommentsHandler_MethodNotAllowed(t *testing.T) {
	handler := NewGetCommentsHandler(
		&mockCommentService{},
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.getComments", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", rec.Code)
	}
}

func TestGetCommentsHandler_HandleResolution(t *testing.T) {
	resolvedDID := ""
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			resolvedDID = req.ActorDID
			return &comments.GetActorCommentsResponse{Comments: []*comments.CommentView{}}, nil
		},
	}
	mockUsers := &mockUserServiceForComments{
		resolveHandleToDIDFunc: func(ctx context.Context, handle string) (string, error) {
			if handle == "test.user" {
				return "did:plc:resolveduser123", nil
			}
			return "", posts.ErrActorNotFound
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		mockUsers,
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=test.user", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if resolvedDID != "did:plc:resolveduser123" {
		t.Errorf("Expected resolved DID 'did:plc:resolveduser123', got '%s'", resolvedDID)
	}
}

func TestGetCommentsHandler_DIDPassThrough(t *testing.T) {
	receivedDID := ""
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			receivedDID = req.ActorDID
			return &comments.GetActorCommentsResponse{Comments: []*comments.CommentView{}}, nil
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	// When actor is already a DID, it should pass through without resolution
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:directuser", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if receivedDID != "did:plc:directuser" {
		t.Errorf("Expected DID 'did:plc:directuser', got '%s'", receivedDID)
	}
}

func TestGetCommentsHandler_EmptyCommentsArray(t *testing.T) {
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			return &comments.GetActorCommentsResponse{
				Comments: []*comments.CommentView{},
			}, nil
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:newuser", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	var response comments.GetActorCommentsResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Comments == nil {
		t.Error("Expected comments array to be non-nil (empty array), got nil")
	}

	if len(response.Comments) != 0 {
		t.Errorf("Expected 0 comments for new user, got %d", len(response.Comments))
	}
}

func TestGetCommentsHandler_WithCursor(t *testing.T) {
	receivedCursor := ""
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			if req.Cursor != nil {
				receivedCursor = *req.Cursor
			}
			nextCursor := "page2cursor"
			return &comments.GetActorCommentsResponse{
				Comments: []*comments.CommentView{},
				Cursor:   &nextCursor,
			}, nil
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test&cursor=testcursor123", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if receivedCursor != "testcursor123" {
		t.Errorf("Expected cursor 'testcursor123', got '%s'", receivedCursor)
	}

	var response comments.GetActorCommentsResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Cursor == nil || *response.Cursor != "page2cursor" {
		t.Error("Expected response to include next cursor")
	}
}

func TestGetCommentsHandler_WithLimit(t *testing.T) {
	receivedLimit := 0
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			receivedLimit = req.Limit
			return &comments.GetActorCommentsResponse{
				Comments: []*comments.CommentView{},
			}, nil
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test&limit=25", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if receivedLimit != 25 {
		t.Errorf("Expected limit 25, got %d", receivedLimit)
	}
}

func TestGetCommentsHandler_WithCommunityFilter(t *testing.T) {
	receivedCommunity := ""
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			receivedCommunity = req.Community
			return &comments.GetActorCommentsResponse{
				Comments: []*comments.CommentView{},
			}, nil
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test&community=did:plc:community123", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	if receivedCommunity != "did:plc:community123" {
		t.Errorf("Expected community 'did:plc:community123', got '%s'", receivedCommunity)
	}
}

func TestGetCommentsHandler_ServiceError_Returns500(t *testing.T) {
	// Test that generic service errors (database failures, etc.) return 500
	mockComments := &mockCommentService{
		getActorCommentsFunc: func(ctx context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
			return nil, errors.New("database connection failed")
		},
	}

	handler := NewGetCommentsHandler(
		mockComments,
		&mockUserServiceForComments{},
		&mockVoteServiceForComments{},
	)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=did:plc:test", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "InternalServerError" {
		t.Errorf("Expected error 'InternalServerError', got '%s'", response.Error)
	}

	// Verify error message doesn't leak internal details
	if response.Message == "database connection failed" {
		t.Error("Error message should not leak internal error details")
	}
}

func TestGetCommentsHandler_ResolutionFailedError_Returns500(t *testing.T) {
	// Test that infrastructure failures during handle resolution return 500, not 400
	mockUsers := &mockUserServiceForComments{
		resolveHandleToDIDFunc: func(ctx context.Context, handle string) (string, error) {
			// Simulate a database failure during resolution
			return "", errors.New("connection refused")
		},
	}

	handler := NewGetCommentsHandler(
		&mockCommentService{},
		mockUsers,
		&mockVoteServiceForComments{},
	)

	// Use a handle (not a DID) to trigger resolution
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getComments?actor=test.user", nil)
	rec := httptest.NewRecorder()

	handler.HandleGetComments(rec, req)

	// Infrastructure failures should return 500, not 400 or 404
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for infrastructure failure, got %d", rec.Code)
	}

	var response ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Error != "InternalServerError" {
		t.Errorf("Expected error 'InternalServerError', got '%s'", response.Error)
	}
}
