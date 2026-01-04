package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// createTestOAuthSession creates a mock OAuth session for testing
func createTestOAuthSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session",
		HostURL:     "http://localhost:3001",
		AccessToken: "test-access-token",
	}
}

// subscribeTestService implements communities.Service for subscribe handler tests
type subscribeTestService struct {
	subscribeFunc   func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error)
	unsubscribeFunc func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error
}

func (m *subscribeTestService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *subscribeTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, nil
}

func (m *subscribeTestService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *subscribeTestService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return nil, nil
}

func (m *subscribeTestService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *subscribeTestService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	if m.subscribeFunc != nil {
		return m.subscribeFunc(ctx, session, communityIdentifier, contentVisibility)
	}
	userDID := ""
	if session != nil {
		userDID = session.AccountDID.String()
	}
	return &communities.Subscription{
		UserDID:      userDID,
		CommunityDID: "did:plc:community123",
		RecordURI:    "at://did:plc:user/social.coves.community.subscription/abc123",
		RecordCID:    "bafytest123",
		SubscribedAt: time.Now(),
	}, nil
}

func (m *subscribeTestService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	if m.unsubscribeFunc != nil {
		return m.unsubscribeFunc(ctx, session, communityIdentifier)
	}
	return nil
}

func (m *subscribeTestService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *subscribeTestService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *subscribeTestService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *subscribeTestService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *subscribeTestService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *subscribeTestService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, nil
}

func (m *subscribeTestService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, nil
}

func (m *subscribeTestService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *subscribeTestService) ValidateHandle(handle string) error {
	return nil
}

func (m *subscribeTestService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (m *subscribeTestService) EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

func (m *subscribeTestService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}

func TestSubscribeHandler_Subscribe_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		contentVisibility int
		expectedCommunity string
	}{
		{
			name:              "subscribe with DID",
			community:         "did:plc:community123",
			contentVisibility: 3,
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "subscribe with canonical handle",
			community:         "c-worldnews.coves.social",
			contentVisibility: 5,
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "subscribe with scoped identifier",
			community:         "!worldnews@coves.social",
			contentVisibility: 1,
			expectedCommunity: "!worldnews@coves.social",
		},
		{
			name:              "subscribe with at-identifier",
			community:         "@c-tech.coves.social",
			contentVisibility: 4,
			expectedCommunity: "@c-tech.coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &subscribeTestService{
				subscribeFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
					receivedIdentifier = communityIdentifier
					userDID := ""
					if session != nil {
						userDID = session.AccountDID.String()
					}
					return &communities.Subscription{
						UserDID:      userDID,
						CommunityDID: "did:plc:resolved",
						RecordURI:    "at://did:plc:user/social.coves.community.subscription/abc123",
						RecordCID:    "bafytest123",
						SubscribedAt: time.Now(),
					}, nil
				},
			}

			handler := NewSubscribeHandler(mockService)

			reqBody := map[string]interface{}{
				"community":         tc.community,
				"contentVisibility": tc.contentVisibility,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session into context
			session := createTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleSubscribe(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Verify the community identifier was passed through correctly
			if receivedIdentifier != tc.expectedCommunity {
				t.Errorf("Expected community %q to be passed to service, got %q", tc.expectedCommunity, receivedIdentifier)
			}

			// Verify response structure
			var resp struct {
				URI      string `json:"uri"`
				CID      string `json:"cid"`
				Existing bool   `json:"existing"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if resp.URI == "" || resp.CID == "" {
				t.Errorf("Expected uri and cid in response, got %+v", resp)
			}
		})
	}
}

func TestSubscribeHandler_Subscribe_RequiresAuth(t *testing.T) {
	mockService := &subscribeTestService{}
	handler := NewSubscribeHandler(mockService)

	reqBody := map[string]interface{}{
		"community":         "did:plc:test",
		"contentVisibility": 3,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No auth context

	w := httptest.NewRecorder()
	handler.HandleSubscribe(w, req)

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

func TestSubscribeHandler_Subscribe_RequiresCommunity(t *testing.T) {
	mockService := &subscribeTestService{}
	handler := NewSubscribeHandler(mockService)

	reqBody := map[string]interface{}{
		"contentVisibility": 3,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestSubscribeHandler_Subscribe_ServiceErrors(t *testing.T) {
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
			name:           "unauthorized",
			serviceErr:     communities.ErrUnauthorized,
			expectedStatus: http.StatusForbidden,
			expectedError:  "Forbidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockService := &subscribeTestService{
				subscribeFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
					return nil, tc.serviceErr
				},
			}

			handler := NewSubscribeHandler(mockService)

			reqBody := map[string]interface{}{
				"community":         "did:plc:test",
				"contentVisibility": 3,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleSubscribe(w, req)

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

func TestSubscribeHandler_Unsubscribe_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		expectedCommunity string
	}{
		{
			name:              "unsubscribe with DID",
			community:         "did:plc:community123",
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "unsubscribe with canonical handle",
			community:         "c-worldnews.coves.social",
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "unsubscribe with scoped identifier",
			community:         "!worldnews@coves.social",
			expectedCommunity: "!worldnews@coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &subscribeTestService{
				unsubscribeFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
					receivedIdentifier = communityIdentifier
					return nil
				},
			}

			handler := NewSubscribeHandler(mockService)

			reqBody := map[string]interface{}{
				"community": tc.community,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unsubscribe", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleUnsubscribe(w, req)

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

func TestSubscribeHandler_Unsubscribe_SubscriptionNotFound(t *testing.T) {
	mockService := &subscribeTestService{
		unsubscribeFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
			return communities.ErrSubscriptionNotFound
		},
	}

	handler := NewSubscribeHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unsubscribe", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleUnsubscribe(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestSubscribeHandler_MethodNotAllowed(t *testing.T) {
	mockService := &subscribeTestService{}
	handler := NewSubscribeHandler(mockService)

	// Test GET on subscribe endpoint
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.subscribe", nil)
	w := httptest.NewRecorder()
	handler.HandleSubscribe(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}

	// Test GET on unsubscribe endpoint
	req = httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.unsubscribe", nil)
	w = httptest.NewRecorder()
	handler.HandleUnsubscribe(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestSubscribeHandler_InvalidJSON(t *testing.T) {
	mockService := &subscribeTestService{}
	handler := NewSubscribeHandler(mockService)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")

	session := createTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestSubscribeHandler_RequiresOAuthSession(t *testing.T) {
	mockService := &subscribeTestService{}
	handler := NewSubscribeHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.subscribe", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// No OAuth session in context

	w := httptest.NewRecorder()
	handler.HandleSubscribe(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}
}

// Ensure unused import is used
var _ = errors.New
