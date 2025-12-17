package vote

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// mockVoteService implements votes.Service for testing
type mockVoteService struct {
	createFunc func(ctx context.Context, session *oauthlib.ClientSessionData, req votes.CreateVoteRequest) (*votes.CreateVoteResponse, error)
	deleteFunc func(ctx context.Context, session *oauthlib.ClientSessionData, req votes.DeleteVoteRequest) error
}

func (m *mockVoteService) CreateVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.CreateVoteRequest) (*votes.CreateVoteResponse, error) {
	if m.createFunc != nil {
		return m.createFunc(ctx, session, req)
	}
	return &votes.CreateVoteResponse{
		URI: "at://did:plc:test123/social.coves.vote/abc123",
		CID: "bafyvote123",
	}, nil
}

func (m *mockVoteService) DeleteVote(ctx context.Context, session *oauthlib.ClientSessionData, req votes.DeleteVoteRequest) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, session, req)
	}
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

func TestCreateVoteHandler_Success(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	// Create request body
	reqBody := CreateVoteInput{
		Subject: struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}{
			URI: "at://did:plc:author123/social.coves.post/xyz789",
			CID: "bafypost123",
		},
		Direction: "up",
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// Inject OAuth session into context (simulates auth middleware)
	did, _ := syntax.ParseDID("did:plc:test123")
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateVote(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check response
	var response CreateVoteOutput
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.URI != "at://did:plc:test123/social.coves.vote/abc123" {
		t.Errorf("Expected URI at://did:plc:test123/social.coves.vote/abc123, got %s", response.URI)
	}
	if response.CID != "bafyvote123" {
		t.Errorf("Expected CID bafyvote123, got %s", response.CID)
	}
}

func TestCreateVoteHandler_RequiresAuth(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	// Create request body
	reqBody := CreateVoteInput{
		Subject: struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}{
			URI: "at://did:plc:author123/social.coves.post/xyz789",
			CID: "bafypost123",
		},
		Direction: "up",
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Create HTTP request without auth context
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No OAuth session in context

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateVote(w, req)

	// Check status code
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
}

func TestCreateVoteHandler_InvalidDirection(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	tests := []struct {
		name      string
		direction string
	}{
		{"empty direction", ""},
		{"invalid direction", "sideways"},
		{"wrong case", "UP"},
		{"typo", "upp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create request body
			reqBody := CreateVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: "at://did:plc:author123/social.coves.post/xyz789",
					CID: "bafypost123",
				},
				Direction: tc.direction,
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session
			did, _ := syntax.ParseDID("did:plc:test123")
			session := &oauthlib.ClientSessionData{
				AccountDID:  did,
				AccessToken: "test_token",
			}
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			// Execute handler
			w := httptest.NewRecorder()
			handler.HandleCreateVote(w, req)

			// Check status code
			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Check error response
			var errResp XRPCError
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Error != "InvalidRequest" {
				t.Errorf("Expected error InvalidRequest, got %s", errResp.Error)
			}
		})
	}
}

func TestCreateVoteHandler_MissingFields(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	tests := []struct {
		name          string
		subjectURI    string
		subjectCID    string
		direction     string
		expectedError string
	}{
		{
			name:          "missing subject URI",
			subjectURI:    "",
			subjectCID:    "bafypost123",
			direction:     "up",
			expectedError: "subject.uri is required",
		},
		{
			name:          "missing subject CID",
			subjectURI:    "at://did:plc:author123/social.coves.post/xyz789",
			subjectCID:    "",
			direction:     "up",
			expectedError: "subject.cid is required",
		},
		{
			name:          "missing direction",
			subjectURI:    "at://did:plc:author123/social.coves.post/xyz789",
			subjectCID:    "bafypost123",
			direction:     "",
			expectedError: "direction is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create request body
			reqBody := CreateVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: tc.subjectURI,
					CID: tc.subjectCID,
				},
				Direction: tc.direction,
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session
			did, _ := syntax.ParseDID("did:plc:test123")
			session := &oauthlib.ClientSessionData{
				AccountDID:  did,
				AccessToken: "test_token",
			}
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			// Execute handler
			w := httptest.NewRecorder()
			handler.HandleCreateVote(w, req)

			// Check status code
			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Check error response
			var errResp XRPCError
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Message != tc.expectedError {
				t.Errorf("Expected message '%s', got '%s'", tc.expectedError, errResp.Message)
			}
		})
	}
}

func TestCreateVoteHandler_InvalidJSON(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	// Create invalid JSON
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBufferString("{invalid json"))
	req.Header.Set("Content-Type", "application/json")

	// Inject OAuth session
	did, _ := syntax.ParseDID("did:plc:test123")
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateVote(w, req)

	// Check status code
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "InvalidRequest" {
		t.Errorf("Expected error InvalidRequest, got %s", errResp.Error)
	}
}

func TestCreateVoteHandler_MethodNotAllowed(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	// Create GET request (should only accept POST)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.vote.create", nil)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateVote(w, req)

	// Check status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestCreateVoteHandler_ServiceError(t *testing.T) {
	tests := []struct {
		serviceError   error
		name           string
		expectedError  string
		expectedStatus int
	}{
		{
			name:           "invalid direction",
			serviceError:   votes.ErrInvalidDirection,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidRequest",
		},
		{
			name:           "invalid subject",
			serviceError:   votes.ErrInvalidSubject,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidSubject", // Per lexicon: social.coves.feed.vote.create#InvalidSubject
		},
		{
			name:           "not authorized",
			serviceError:   votes.ErrNotAuthorized,
			expectedStatus: http.StatusForbidden,
			expectedError:  "NotAuthorized", // Per lexicon: social.coves.feed.vote.create#NotAuthorized
		},
		{
			name:           "banned",
			serviceError:   votes.ErrBanned,
			expectedStatus: http.StatusForbidden,
			expectedError:  "NotAuthorized", // Banned maps to NotAuthorized per lexicon
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockService := &mockVoteService{
				createFunc: func(ctx context.Context, session *oauthlib.ClientSessionData, req votes.CreateVoteRequest) (*votes.CreateVoteResponse, error) {
					return nil, tc.serviceError
				},
			}
			handler := NewCreateVoteHandler(mockService)

			// Create request body
			reqBody := CreateVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: "at://did:plc:author123/social.coves.post/xyz789",
					CID: "bafypost123",
				},
				Direction: "up",
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session
			did, _ := syntax.ParseDID("did:plc:test123")
			session := &oauthlib.ClientSessionData{
				AccountDID:  did,
				AccessToken: "test_token",
			}
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			// Execute handler
			w := httptest.NewRecorder()
			handler.HandleCreateVote(w, req)

			// Check status code
			if w.Code != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d. Body: %s", tc.expectedStatus, w.Code, w.Body.String())
			}

			// Check error response
			var errResp XRPCError
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Error != tc.expectedError {
				t.Errorf("Expected error %s, got %s", tc.expectedError, errResp.Error)
			}
		})
	}
}

func TestCreateVoteHandler_ValidDirections(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewCreateVoteHandler(mockService)

	directions := []string{"up", "down"}

	for _, direction := range directions {
		t.Run("direction_"+direction, func(t *testing.T) {
			// Create request body
			reqBody := CreateVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: "at://did:plc:author123/social.coves.post/xyz789",
					CID: "bafypost123",
				},
				Direction: direction,
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.create", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session
			did, _ := syntax.ParseDID("did:plc:test123")
			session := &oauthlib.ClientSessionData{
				AccountDID:  did,
				AccessToken: "test_token",
			}
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			// Execute handler
			w := httptest.NewRecorder()
			handler.HandleCreateVote(w, req)

			// Check status code
			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200 for direction '%s', got %d. Body: %s", direction, w.Code, w.Body.String())
			}
		})
	}
}
