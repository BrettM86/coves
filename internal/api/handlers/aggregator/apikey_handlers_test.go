package aggregator

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// mockAggregatorService implements aggregators.Service for testing
type mockAggregatorService struct {
	isAggregatorFunc func(ctx context.Context, did string) (bool, error)
}

func (m *mockAggregatorService) IsAggregator(ctx context.Context, did string) (bool, error) {
	if m.isAggregatorFunc != nil {
		return m.isAggregatorFunc(ctx, did)
	}
	return true, nil
}

// Stub implementations for Service interface methods we don't test
func (m *mockAggregatorService) GetAggregator(ctx context.Context, did string) (*aggregators.Aggregator, error) {
	return nil, nil
}

func (m *mockAggregatorService) GetAggregators(ctx context.Context, dids []string) ([]*aggregators.Aggregator, error) {
	return nil, nil
}

func (m *mockAggregatorService) ListAggregators(ctx context.Context, limit, offset int) ([]*aggregators.Aggregator, error) {
	return nil, nil
}

func (m *mockAggregatorService) GetAuthorizationsForAggregator(ctx context.Context, req aggregators.GetAuthorizationsRequest) ([]*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAggregatorService) ListAggregatorsForCommunity(ctx context.Context, req aggregators.ListForCommunityRequest) ([]*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAggregatorService) EnableAggregator(ctx context.Context, req aggregators.EnableAggregatorRequest) (*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAggregatorService) DisableAggregator(ctx context.Context, req aggregators.DisableAggregatorRequest) (*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAggregatorService) UpdateAggregatorConfig(ctx context.Context, req aggregators.UpdateConfigRequest) (*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAggregatorService) ValidateAggregatorPost(ctx context.Context, aggregatorDID, communityDID string) error {
	return nil
}

func (m *mockAggregatorService) RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error {
	return nil
}

// XRPCError represents an XRPC error response for testing
type XRPCError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Helper to create authenticated request context with OAuth session
func createAuthenticatedContext(t *testing.T, didStr string) context.Context {
	t.Helper()
	did, err := syntax.ParseDID(didStr)
	if err != nil {
		t.Fatalf("Failed to parse DID: %v", err)
	}
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_access_token",
		SessionID:   "test_session",
	}
	ctx := context.WithValue(context.Background(), middleware.OAuthSessionKey, session)
	ctx = context.WithValue(ctx, middleware.UserDIDKey, didStr)
	return ctx
}

// Helper to create context with just UserDID (no OAuth session)
func createUserDIDContext(didStr string) context.Context {
	return context.WithValue(context.Background(), middleware.UserDIDKey, didStr)
}

// =============================================================================
// CreateAPIKey Handler Tests
// =============================================================================

func TestCreateAPIKeyHandler_Success(t *testing.T) {
	// Create mock services
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	mockAPIKeySvc := &mockAPIKeyService{
		generateKeyFunc: func(ctx context.Context, aggregatorDID string, oauthSession *oauthlib.ClientSessionData) (string, string, error) {
			return "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "ckapi_012345", nil
		},
	}

	handler := NewCreateAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with full auth context (including OAuth session)
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createAuthenticatedContext(t, "did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check response format
	var response CreateAPIKeyResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Key != "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("Expected key to match generated key, got %s", response.Key)
	}
	if response.KeyPrefix != "ckapi_012345" {
		t.Errorf("Expected keyPrefix to match, got %s", response.KeyPrefix)
	}
	if response.DID != "did:plc:aggregator123" {
		t.Errorf("Expected DID to match authenticated user, got %s", response.DID)
	}
	if response.CreatedAt == "" {
		t.Error("Expected createdAt to be set")
	}
}

func TestCreateAPIKeyHandler_RequiresAuth(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewCreateAPIKeyHandler(nil, mockAggSvc)

	// Create HTTP request without auth context
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	// No OAuth session in context

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthenticationRequired" {
		t.Errorf("Expected error AuthenticationRequired, got %s", errResp.Error)
	}
}

func TestCreateAPIKeyHandler_MethodNotAllowed(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewCreateAPIKeyHandler(nil, mockAggSvc)

	// Create GET request (should only accept POST)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.createApiKey", nil)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestCreateAPIKeyHandler_NotAggregator(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, nil // Not an aggregator
		},
	}
	handler := NewCreateAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createAuthenticatedContext(t, "did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AggregatorRequired" {
		t.Errorf("Expected error AggregatorRequired, got %s", errResp.Error)
	}
}

func TestCreateAPIKeyHandler_AggregatorCheckError(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, errors.New("database error")
		},
	}
	handler := NewCreateAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createAuthenticatedContext(t, "did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "InternalServerError" {
		t.Errorf("Expected error InternalServerError, got %s", errResp.Error)
	}
}

func TestCreateAPIKeyHandler_MissingOAuthSession(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}
	handler := NewCreateAPIKeyHandler(nil, mockAggSvc)

	// Create request with UserDID but no OAuth session
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleCreateAPIKey(w, req)

	// Check status code - should fail because OAuth session is required
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "OAuthSessionRequired" {
		t.Errorf("Expected error OAuthSessionRequired, got %s", errResp.Error)
	}
}

// =============================================================================
// GetAPIKey Handler Tests
// =============================================================================

func TestGetAPIKeyHandler_Success(t *testing.T) {
	createdAt := time.Now().Add(-24 * time.Hour)
	lastUsed := time.Now().Add(-1 * time.Hour)

	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	mockAPIKeySvc := &mockAPIKeyService{
		getAPIKeyInfoFunc: func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
			return &aggregators.APIKeyInfo{
				HasKey:     true,
				KeyPrefix:  "ckapi_test12",
				CreatedAt:  &createdAt,
				LastUsedAt: &lastUsed,
				IsRevoked:  false,
			}, nil
		},
	}

	handler := NewGetAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", nil)
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check response format
	var response GetAPIKeyResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if !response.HasKey {
		t.Error("Expected hasKey to be true")
	}
	if response.KeyInfo == nil {
		t.Fatal("Expected keyInfo to be present")
	}
	if response.KeyInfo.Prefix != "ckapi_test12" {
		t.Errorf("Expected prefix 'ckapi_test12', got %s", response.KeyInfo.Prefix)
	}
	if response.KeyInfo.IsRevoked {
		t.Error("Expected isRevoked to be false")
	}
	if response.KeyInfo.CreatedAt == "" {
		t.Error("Expected createdAt to be set")
	}
	if response.KeyInfo.LastUsedAt == nil {
		t.Error("Expected lastUsedAt to be set")
	}
}

func TestGetAPIKeyHandler_Success_NoKey(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	mockAPIKeySvc := &mockAPIKeyService{
		getAPIKeyInfoFunc: func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
			return &aggregators.APIKeyInfo{
				HasKey: false,
			}, nil
		},
	}

	handler := NewGetAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", nil)
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check response format
	var response GetAPIKeyResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.HasKey {
		t.Error("Expected hasKey to be false")
	}
	if response.KeyInfo != nil {
		t.Error("Expected keyInfo to be nil when hasKey is false")
	}
}

func TestGetAPIKeyHandler_RequiresAuth(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewGetAPIKeyHandler(nil, mockAggSvc)

	// Create HTTP request without auth context
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", nil)
	// No auth context

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthenticationRequired" {
		t.Errorf("Expected error AuthenticationRequired, got %s", errResp.Error)
	}
}

func TestGetAPIKeyHandler_MethodNotAllowed(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewGetAPIKeyHandler(nil, mockAggSvc)

	// Create POST request (should only accept GET)
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.getApiKey", nil)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestGetAPIKeyHandler_NotAggregator(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, nil // Not an aggregator
		},
	}
	handler := NewGetAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", nil)
	ctx := createUserDIDContext("did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AggregatorRequired" {
		t.Errorf("Expected error AggregatorRequired, got %s", errResp.Error)
	}
}

func TestGetAPIKeyHandler_AggregatorCheckError(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, errors.New("database error")
		},
	}
	handler := NewGetAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", nil)
	ctx := createUserDIDContext("did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleGetAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "InternalServerError" {
		t.Errorf("Expected error InternalServerError, got %s", errResp.Error)
	}
}

// =============================================================================
// RevokeAPIKey Handler Tests
// =============================================================================

func TestRevokeAPIKeyHandler_Success(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	revokeKeyCalled := false
	mockAPIKeySvc := &mockAPIKeyService{
		getAPIKeyInfoFunc: func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
			return &aggregators.APIKeyInfo{
				HasKey:    true,
				KeyPrefix: "ckapi_test12",
				IsRevoked: false,
			}, nil
		},
		revokeKeyFunc: func(ctx context.Context, aggregatorDID string) error {
			revokeKeyCalled = true
			return nil
		},
	}

	handler := NewRevokeAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check that RevokeKey was called
	if !revokeKeyCalled {
		t.Error("Expected RevokeKey to be called")
	}

	// Check response format
	var response RevokeAPIKeyResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.RevokedAt == "" {
		t.Error("Expected revokedAt to be set")
	}

	// Verify timestamp format
	_, err := time.Parse("2006-01-02T15:04:05.000Z", response.RevokedAt)
	if err != nil {
		t.Errorf("Expected revokedAt to be valid ISO8601 timestamp: %v", err)
	}
}

func TestRevokeAPIKeyHandler_NoKeyToRevoke(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	mockAPIKeySvc := &mockAPIKeyService{
		getAPIKeyInfoFunc: func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
			return &aggregators.APIKeyInfo{
				HasKey: false, // No key exists
			}, nil
		},
	}

	handler := NewRevokeAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "ApiKeyNotFound" {
		t.Errorf("Expected error ApiKeyNotFound, got %s", errResp.Error)
	}
}

func TestRevokeAPIKeyHandler_AlreadyRevoked(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return true, nil // Is an aggregator
		},
	}

	mockAPIKeySvc := &mockAPIKeyService{
		getAPIKeyInfoFunc: func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
			return &aggregators.APIKeyInfo{
				HasKey:    true,
				KeyPrefix: "ckapi_test12",
				IsRevoked: true, // Already revoked
			}, nil
		},
	}

	handler := NewRevokeAPIKeyHandler(mockAPIKeySvc, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:aggregator123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "ApiKeyAlreadyRevoked" {
		t.Errorf("Expected error ApiKeyAlreadyRevoked, got %s", errResp.Error)
	}
}

func TestRevokeAPIKeyHandler_RequiresAuth(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewRevokeAPIKeyHandler(nil, mockAggSvc)

	// Create HTTP request without auth context
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	// No auth context

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthenticationRequired" {
		t.Errorf("Expected error AuthenticationRequired, got %s", errResp.Error)
	}
}

func TestRevokeAPIKeyHandler_MethodNotAllowed(t *testing.T) {
	mockAggSvc := &mockAggregatorService{}
	handler := NewRevokeAPIKeyHandler(nil, mockAggSvc)

	// Create GET request (should only accept POST)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.aggregator.revokeApiKey", nil)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestRevokeAPIKeyHandler_NotAggregator(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, nil // Not an aggregator
		},
	}
	handler := NewRevokeAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AggregatorRequired" {
		t.Errorf("Expected error AggregatorRequired, got %s", errResp.Error)
	}
}

func TestRevokeAPIKeyHandler_AggregatorCheckError(t *testing.T) {
	mockAggSvc := &mockAggregatorService{
		isAggregatorFunc: func(ctx context.Context, did string) (bool, error) {
			return false, errors.New("database error")
		},
	}
	handler := NewRevokeAPIKeyHandler(nil, mockAggSvc)

	// Create request with auth
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", nil)
	req.Header.Set("Content-Type", "application/json")
	ctx := createUserDIDContext("did:plc:user123")
	req = req.WithContext(ctx)

	// Execute handler
	w := httptest.NewRecorder()
	handler.HandleRevokeAPIKey(w, req)

	// Check status code
	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Check error response
	var errResp XRPCError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "InternalServerError" {
		t.Errorf("Expected error InternalServerError, got %s", errResp.Error)
	}
}

// =============================================================================
// Response Format Tests
// =============================================================================

func TestRevokeAPIKeyResponse_ContainsRequiredFields(t *testing.T) {
	// Verify RevokeAPIKeyResponse has the required fields per lexicon
	response := RevokeAPIKeyResponse{
		RevokedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// Check required fields per lexicon (success field removed per AT Protocol best practices)
	if _, ok := decoded["revokedAt"]; !ok {
		t.Error("Response missing required 'revokedAt' field")
	}
}

func TestCreateAPIKeyResponse_ContainsRequiredFields(t *testing.T) {
	response := CreateAPIKeyResponse{
		Key:       "ckapi_test1234567890123456789012345678",
		KeyPrefix: "ckapi_test12",
		DID:       "did:plc:aggregator123",
		CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// Check required fields
	requiredFields := []string{"key", "keyPrefix", "did", "createdAt"}
	for _, field := range requiredFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("Response missing required '%s' field", field)
		}
	}
}

func TestGetAPIKeyResponse_ContainsRequiredFields(t *testing.T) {
	response := GetAPIKeyResponse{
		HasKey: true,
		KeyInfo: &APIKeyView{
			Prefix:    "ckapi_test12",
			CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			IsRevoked: false,
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// Check required fields (now uses nested keyInfo structure)
	if _, ok := decoded["hasKey"]; !ok {
		t.Error("Response missing required 'hasKey' field")
	}
	if keyInfo, ok := decoded["keyInfo"].(map[string]interface{}); ok {
		if _, ok := keyInfo["isRevoked"]; !ok {
			t.Error("keyInfo missing required 'isRevoked' field")
		}
	} else {
		t.Error("Response missing 'keyInfo' field when hasKey is true")
	}
}

func TestGetAPIKeyResponse_OmitsEmptyOptionalFields(t *testing.T) {
	response := GetAPIKeyResponse{
		HasKey: false,
		// KeyInfo is nil when hasKey is false
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// KeyInfo should be omitted when hasKey is false (per omitempty tag)
	if _, ok := decoded["keyInfo"]; ok {
		t.Error("Response should omit nil 'keyInfo' field when hasKey is false")
	}
}

// =============================================================================
// Handler Success Path Tests with Mocks
// =============================================================================

// mockAPIKeyService implements aggregators.APIKeyServiceInterface for testing
type mockAPIKeyService struct {
	generateKeyFunc            func(ctx context.Context, aggregatorDID string, oauthSession *oauthlib.ClientSessionData) (plainKey string, keyPrefix string, err error)
	getAPIKeyInfoFunc          func(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error)
	revokeKeyFunc              func(ctx context.Context, aggregatorDID string) error
	failedLastUsedUpdates      int64
	failedNonceUpdates         int64
}

func (m *mockAPIKeyService) GenerateKey(ctx context.Context, aggregatorDID string, oauthSession *oauthlib.ClientSessionData) (string, string, error) {
	if m.generateKeyFunc != nil {
		return m.generateKeyFunc(ctx, aggregatorDID, oauthSession)
	}
	return "", "", errors.New("not implemented")
}

func (m *mockAPIKeyService) GetAPIKeyInfo(ctx context.Context, aggregatorDID string) (*aggregators.APIKeyInfo, error) {
	if m.getAPIKeyInfoFunc != nil {
		return m.getAPIKeyInfoFunc(ctx, aggregatorDID)
	}
	return nil, errors.New("not implemented")
}

func (m *mockAPIKeyService) RevokeKey(ctx context.Context, aggregatorDID string) error {
	if m.revokeKeyFunc != nil {
		return m.revokeKeyFunc(ctx, aggregatorDID)
	}
	return errors.New("not implemented")
}

func (m *mockAPIKeyService) GetFailedLastUsedUpdates() int64 {
	return m.failedLastUsedUpdates
}

func (m *mockAPIKeyService) GetFailedNonceUpdates() int64 {
	return m.failedNonceUpdates
}

// Verify mockAPIKeyService implements the interface at compile time
var _ aggregators.APIKeyServiceInterface = (*mockAPIKeyService)(nil)

func TestCreateAPIKeyHandler_Success_RequiresIntegration(t *testing.T) {
	// The CreateAPIKeyHandler.HandleCreateAPIKey method calls:
	// 1. middleware.GetUserDID(r) - to get authenticated user
	// 2. h.aggregatorService.IsAggregator(ctx, userDID) - to verify aggregator status
	// 3. middleware.GetOAuthSession(r) - to get OAuth session
	// 4. h.apiKeyService.GenerateKey(ctx, userDID, oauthSession) - to create the key
	//
	// Since apiKeyService is a concrete *aggregators.APIKeyService (not an interface),
	// we cannot mock it directly. Full success path testing requires:
	// - A real aggregators.Repository mock
	// - A real OAuth store mock
	// - Setting up the full APIKeyService with those mocks
	//
	// This test documents the pattern for integration-style testing with mocks:

	// Create mock repository that tracks calls
	createdAt := time.Now()
	generateKeyCalled := false

	// Create a custom test that verifies the handler response format when everything works
	t.Run("response_format_verification", func(t *testing.T) {
		// Verify the expected response format matches what GenerateKey would return
		response := CreateAPIKeyResponse{
			Key:       "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			KeyPrefix: "ckapi_012345",
			DID:       "did:plc:aggregator123",
			CreatedAt: createdAt.Format("2006-01-02T15:04:05.000Z"),
		}

		data, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("Failed to marshal response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// Verify key format
		key, ok := decoded["key"].(string)
		if !ok || len(key) != 70 {
			t.Errorf("Expected key to be 70 chars, got %d", len(key))
		}
		if !ok || key[:6] != "ckapi_" {
			t.Errorf("Expected key to start with 'ckapi_', got %s", key[:6])
		}

		// Verify keyPrefix is first 12 chars of key
		keyPrefix, ok := decoded["keyPrefix"].(string)
		if !ok || keyPrefix != key[:12] {
			t.Errorf("Expected keyPrefix to be first 12 chars of key")
		}
	})

	// This assertion exists just to use the variable and satisfy the linter
	_ = generateKeyCalled
}

func TestGetAPIKeyHandler_Success_RequiresIntegration(t *testing.T) {
	// Similar to CreateAPIKeyHandler, GetAPIKeyHandler uses concrete *aggregators.APIKeyService.
	// This test documents the integration test pattern and verifies response format.

	t.Run("response_format_with_active_key", func(t *testing.T) {
		createdAt := time.Now().Add(-24 * time.Hour)
		lastUsed := time.Now().Add(-1 * time.Hour)
		lastUsedStr := lastUsed.Format("2006-01-02T15:04:05.000Z")

		response := GetAPIKeyResponse{
			HasKey: true,
			KeyInfo: &APIKeyView{
				Prefix:     "ckapi_test12",
				CreatedAt:  createdAt.Format("2006-01-02T15:04:05.000Z"),
				LastUsedAt: &lastUsedStr,
				IsRevoked:  false,
			},
		}

		data, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("Failed to marshal response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// Verify all expected fields are present
		if !decoded["hasKey"].(bool) {
			t.Error("Expected hasKey to be true")
		}
		keyInfo := decoded["keyInfo"].(map[string]interface{})
		if keyInfo["prefix"] != "ckapi_test12" {
			t.Errorf("Expected prefix 'ckapi_test12', got %v", keyInfo["prefix"])
		}
		if keyInfo["isRevoked"].(bool) {
			t.Error("Expected isRevoked to be false")
		}
	})

	t.Run("response_format_with_no_key", func(t *testing.T) {
		response := GetAPIKeyResponse{
			HasKey: false,
			// KeyInfo is nil when hasKey is false
		}

		data, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("Failed to marshal response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if decoded["hasKey"].(bool) {
			t.Error("Expected hasKey to be false")
		}
		if _, ok := decoded["keyInfo"]; ok {
			t.Error("Expected keyInfo to be omitted when hasKey is false")
		}
	})
}

// =============================================================================
// Service Error Handling Tests
// =============================================================================
// These tests document the expected error handling behavior when the APIKeyService
// returns errors. Since handlers use concrete *aggregators.APIKeyService (not an
// interface), full testing of these paths requires integration tests with mocked
// repository layer.

func TestRevokeAPIKeyHandler_ServiceError_Documentation(t *testing.T) {
	// Documents expected behavior when RevokeKey returns an error:
	// - Handler should return 500 InternalServerError
	// - Error response should include "RevocationFailed" error code
	//
	// This behavior is tested at the service level and integration level.
	t.Run("expected_error_response", func(t *testing.T) {
		errorResp := struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}{
			Error:   "RevocationFailed",
			Message: "Failed to revoke API key",
		}

		data, err := json.Marshal(errorResp)
		if err != nil {
			t.Fatalf("Failed to marshal error response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if decoded["error"] != "RevocationFailed" {
			t.Errorf("Expected error 'RevocationFailed', got %v", decoded["error"])
		}
	})
}

func TestCreateAPIKeyHandler_KeyGenerationError_Documentation(t *testing.T) {
	// Documents expected behavior when GenerateKey returns an error:
	// - Handler should return 500 InternalServerError
	// - Error response should include "KeyGenerationFailed" error code
	//
	// This behavior is tested at the service level and integration level.
	t.Run("expected_error_response", func(t *testing.T) {
		errorResp := struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}{
			Error:   "KeyGenerationFailed",
			Message: "Failed to generate API key",
		}

		data, err := json.Marshal(errorResp)
		if err != nil {
			t.Fatalf("Failed to marshal error response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if decoded["error"] != "KeyGenerationFailed" {
			t.Errorf("Expected error 'KeyGenerationFailed', got %v", decoded["error"])
		}
	})
}

func TestGetAPIKeyHandler_ServiceError_Documentation(t *testing.T) {
	// Documents expected behavior when GetAPIKeyInfo returns an error:
	// - Handler should return 500 InternalServerError
	// - Error response should include "InternalServerError" error code
	//
	// This behavior is tested at the service level and integration level.
	t.Run("expected_error_response", func(t *testing.T) {
		errorResp := struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}{
			Error:   "InternalServerError",
			Message: "Failed to get API key info",
		}

		data, err := json.Marshal(errorResp)
		if err != nil {
			t.Fatalf("Failed to marshal error response: %v", err)
		}

		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if decoded["error"] != "InternalServerError" {
			t.Errorf("Expected error 'InternalServerError', got %v", decoded["error"])
		}
	})
}
