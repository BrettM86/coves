package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// blockTestService implements communities.Service for block handler tests
type blockTestService struct {
	blockFunc   func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error)
	unblockFunc func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error
}

func (m *blockTestService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *blockTestService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *blockTestService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	if m.blockFunc != nil {
		return m.blockFunc(ctx, session, communityIdentifier)
	}
	userDID := ""
	if session != nil {
		userDID = session.AccountDID.String()
	}
	return &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: "did:plc:community123",
		RecordURI:    "at://did:plc:user/social.coves.community.block/abc123",
		RecordCID:    "bafytest123",
		BlockedAt:    time.Now(),
	}, nil
}

func (m *blockTestService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	if m.unblockFunc != nil {
		return m.unblockFunc(ctx, session, communityIdentifier)
	}
	return nil
}

func (m *blockTestService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *blockTestService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, nil
}

func (m *blockTestService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, nil
}

func (m *blockTestService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *blockTestService) ValidateHandle(handle string) error {
	return nil
}

func (m *blockTestService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (m *blockTestService) EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

func (m *blockTestService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}

// createBlockTestOAuthSession creates a mock OAuth session for block handler tests
func createBlockTestOAuthSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session",
		HostURL:     "http://localhost:3001",
		AccessToken: "test-access-token",
	}
}

func TestBlockHandler_Block_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		expectedCommunity string
	}{
		{
			name:              "block with DID",
			community:         "did:plc:community123",
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "block with canonical handle",
			community:         "c-worldnews.coves.social",
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "block with scoped identifier",
			community:         "!worldnews@coves.social",
			expectedCommunity: "!worldnews@coves.social",
		},
		{
			name:              "block with at-identifier",
			community:         "@c-tech.coves.social",
			expectedCommunity: "@c-tech.coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &blockTestService{
				blockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
					receivedIdentifier = communityIdentifier
					userDID := ""
					if session != nil {
						userDID = session.AccountDID.String()
					}
					return &communities.CommunityBlock{
						UserDID:      userDID,
						CommunityDID: "did:plc:resolved",
						RecordURI:    "at://did:plc:user/social.coves.community.block/abc123",
						RecordCID:    "bafytest123",
						BlockedAt:    time.Now(),
					}, nil
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": tc.community,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session into context
			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleBlock(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Verify the community identifier was passed through correctly
			if receivedIdentifier != tc.expectedCommunity {
				t.Errorf("Expected community %q to be passed to service, got %q", tc.expectedCommunity, receivedIdentifier)
			}

			// Verify response structure
			var resp struct {
				Block struct {
					RecordURI string `json:"recordUri"`
					RecordCID string `json:"recordCid"`
				} `json:"block"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if resp.Block.RecordURI == "" || resp.Block.RecordCID == "" {
				t.Errorf("Expected recordUri and recordCid in response, got %+v", resp)
			}
		})
	}
}

func TestBlockHandler_Block_RequiresOAuthSession(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// No OAuth session in context

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}

	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
}

func TestBlockHandler_Block_RequiresCommunity(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestBlockHandler_Block_ServiceErrors(t *testing.T) {
	tests := []struct {
		name           string
		serviceErr     error
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "community not found",
			serviceErr:     communities.ErrCommunityNotFound,
			expectedStatus: http.StatusNotFound,
			expectedError:  "NotFound",
		},
		{
			name:           "validation error",
			serviceErr:     communities.NewValidationError("community", "invalid format"),
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidRequest",
		},
		{
			name:           "already blocked",
			serviceErr:     communities.ErrBlockAlreadyExists,
			expectedStatus: http.StatusConflict,
			expectedError:  "AlreadyExists",
		},
		{
			name:           "unauthorized",
			serviceErr:     communities.ErrUnauthorized,
			expectedStatus: http.StatusForbidden,
			expectedError:  "Forbidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockService := &blockTestService{
				blockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
					return nil, tc.serviceErr
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": "did:plc:test",
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleBlock(w, req)

			if w.Code != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d. Body: %s", tc.expectedStatus, w.Code, w.Body.String())
			}

			var errResp struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Error != tc.expectedError {
				t.Errorf("Expected error %s, got %s", tc.expectedError, errResp.Error)
			}
		})
	}
}

func TestBlockHandler_Unblock_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		expectedCommunity string
	}{
		{
			name:              "unblock with DID",
			community:         "did:plc:community123",
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "unblock with canonical handle",
			community:         "c-worldnews.coves.social",
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "unblock with scoped identifier",
			community:         "!worldnews@coves.social",
			expectedCommunity: "!worldnews@coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &blockTestService{
				unblockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
					receivedIdentifier = communityIdentifier
					return nil
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": tc.community,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleUnblock(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			if receivedIdentifier != tc.expectedCommunity {
				t.Errorf("Expected community %q to be passed to service, got %q", tc.expectedCommunity, receivedIdentifier)
			}

			var resp struct {
				Success bool `json:"success"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if !resp.Success {
				t.Errorf("Expected success: true in response")
			}
		})
	}
}

func TestBlockHandler_Unblock_RequiresOAuthSession(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// No OAuth session in context

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}

	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
}

func TestBlockHandler_Unblock_BlockNotFound(t *testing.T) {
	mockService := &blockTestService{
		unblockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
			return communities.ErrBlockNotFound
		},
	}

	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestBlockHandler_MethodNotAllowed(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	// Test GET on block endpoint
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.blockCommunity", nil)
	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}

	// Test GET on unblock endpoint
	req = httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.unblockCommunity", nil)
	w = httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestBlockHandler_InvalidJSON(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}
