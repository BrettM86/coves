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

func TestDeleteVoteHandler_Success(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	// Create request body
	reqBody := DeleteVoteInput{
		Subject: struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}{
			URI: "at://did:plc:author123/social.coves.post/xyz789",
			CID: "bafypost123",
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBuffer(bodyBytes))
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
	handler.HandleDeleteVote(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check response is empty object per lexicon
	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(response) != 0 {
		t.Errorf("Expected empty object per lexicon, got %v", response)
	}
}

func TestDeleteVoteHandler_RequiresAuth(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	// Create request body
	reqBody := DeleteVoteInput{
		Subject: struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}{
			URI: "at://did:plc:author123/social.coves.post/xyz789",
			CID: "bafypost123",
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Create HTTP request without auth context
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No OAuth session in context

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleDeleteVote(w, req)

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

func TestDeleteVoteHandler_MissingFields(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	tests := []struct {
		name          string
		subjectURI    string
		subjectCID    string
		expectedError string
	}{
		{
			name:          "missing subject URI",
			subjectURI:    "",
			subjectCID:    "bafypost123",
			expectedError: "subject.uri is required",
		},
		{
			name:          "missing subject CID",
			subjectURI:    "at://did:plc:author123/social.coves.post/xyz789",
			subjectCID:    "",
			expectedError: "subject.cid is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create request body
			reqBody := DeleteVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: tc.subjectURI,
					CID: tc.subjectCID,
				},
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBuffer(bodyBytes))
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
			handler.HandleDeleteVote(w, req)

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

func TestDeleteVoteHandler_InvalidJSON(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	// Create invalid JSON
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBufferString("{invalid json"))
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
	handler.HandleDeleteVote(w, req)

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

func TestDeleteVoteHandler_MethodNotAllowed(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	// Create GET request (should only accept POST)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.vote.delete", nil)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleDeleteVote(w, req)

	// Check status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestDeleteVoteHandler_ServiceError(t *testing.T) {
	tests := []struct {
		serviceError   error
		name           string
		expectedError  string
		expectedStatus int
	}{
		{
			name:           "vote not found",
			serviceError:   votes.ErrVoteNotFound,
			expectedStatus: http.StatusNotFound,
			expectedError:  "VoteNotFound", // Per lexicon: social.coves.feed.vote.delete#VoteNotFound
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
			expectedError:  "NotAuthorized", // Per lexicon: social.coves.feed.vote.delete#NotAuthorized
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
				deleteFunc: func(ctx context.Context, session *oauthlib.ClientSessionData, req votes.DeleteVoteRequest) error {
					return tc.serviceError
				},
			}
			handler := NewDeleteVoteHandler(mockService)

			// Create request body
			reqBody := DeleteVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: "at://did:plc:author123/social.coves.post/xyz789",
					CID: "bafypost123",
				},
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBuffer(bodyBytes))
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
			handler.HandleDeleteVote(w, req)

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

func TestDeleteVoteHandler_MultipleSubjects(t *testing.T) {
	mockService := &mockVoteService{}
	handler := NewDeleteVoteHandler(mockService)

	subjects := []struct {
		uri string
		cid string
	}{
		{"at://did:plc:author1/social.coves.post/post1", "bafypost1"},
		{"at://did:plc:author2/social.coves.post/post2", "bafypost2"},
		{"at://did:plc:author3/social.coves.comment/comment1", "bafycomment1"},
	}

	for _, subject := range subjects {
		t.Run("subject_"+subject.uri, func(t *testing.T) {
			// Create request body
			reqBody := DeleteVoteInput{
				Subject: struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}{
					URI: subject.uri,
					CID: subject.cid,
				},
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.vote.delete", bytes.NewBuffer(bodyBytes))
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
			handler.HandleDeleteVote(w, req)

			// Check status code
			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200 for subject '%s', got %d. Body: %s", subject.uri, w.Code, w.Body.String())
			}

			// Check response is empty object per lexicon
			var response map[string]interface{}
			if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}

			if len(response) != 0 {
				t.Errorf("Expected empty object per lexicon, got %v", response)
			}
		})
	}
}
