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
)

// mockCommunityService implements communities.Service for testing
type mockCommunityService struct {
	createFunc func(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error)
}

func (m *mockCommunityService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	if m.createFunc != nil {
		return m.createFunc(ctx, req)
	}
	return &communities.Community{
		DID:         "did:plc:test123",
		Handle:      "c-test.coves.social",
		RecordURI:   "at://did:plc:test123/social.coves.community.profile/self",
		RecordCID:   "bafytest123",
		DisplayName: req.DisplayName,
		Description: req.Description,
		Visibility:  req.Visibility,
		CreatedAt:   time.Now(),
	}, nil
}

func (m *mockCommunityService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, nil
}

func (m *mockCommunityService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *mockCommunityService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return nil, nil
}

func (m *mockCommunityService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *mockCommunityService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *mockCommunityService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *mockCommunityService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *mockCommunityService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *mockCommunityService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, nil
}

func (m *mockCommunityService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *mockCommunityService) ValidateHandle(handle string) error {
	return nil
}

func (m *mockCommunityService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (m *mockCommunityService) EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

func (m *mockCommunityService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}

func TestCreateHandler_AllowlistRestriction(t *testing.T) {
	mockService := &mockCommunityService{}

	tests := []struct {
		name           string
		requestDID     string
		expectedError  string
		allowedDIDs    []string
		expectedStatus int
	}{
		{
			name:           "allowed DID can create community",
			allowedDIDs:    []string{"did:plc:allowed123"},
			requestDID:     "did:plc:allowed123",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "non-allowed DID is forbidden",
			allowedDIDs:    []string{"did:plc:allowed123"},
			requestDID:     "did:plc:notallowed456",
			expectedStatus: http.StatusForbidden,
			expectedError:  "CommunityCreationRestricted",
		},
		{
			name:           "empty allowlist allows anyone",
			allowedDIDs:    nil,
			requestDID:     "did:plc:anyuser789",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple allowed DIDs - first DID",
			allowedDIDs:    []string{"did:plc:admin1", "did:plc:admin2", "did:plc:admin3"},
			requestDID:     "did:plc:admin1",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple allowed DIDs - last DID",
			allowedDIDs:    []string{"did:plc:admin1", "did:plc:admin2", "did:plc:admin3"},
			requestDID:     "did:plc:admin3",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple allowed DIDs - not in list",
			allowedDIDs:    []string{"did:plc:admin1", "did:plc:admin2"},
			requestDID:     "did:plc:randomuser",
			expectedStatus: http.StatusForbidden,
			expectedError:  "CommunityCreationRestricted",
		},
		{
			name:           "allowlist with empty strings filtered - valid DID works",
			allowedDIDs:    []string{"did:plc:admin1", "", "did:plc:admin2"},
			requestDID:     "did:plc:admin1",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "allowlist with empty strings filtered - invalid DID blocked",
			allowedDIDs:    []string{"did:plc:admin1", "", "did:plc:admin2"},
			requestDID:     "did:plc:notallowed",
			expectedStatus: http.StatusForbidden,
			expectedError:  "CommunityCreationRestricted",
		},
		{
			name:           "all empty strings allows anyone",
			allowedDIDs:    []string{"", "", ""},
			requestDID:     "did:plc:anyuser",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewCreateHandler(mockService, tc.allowedDIDs)

			// Create request body
			reqBody := map[string]interface{}{
				"name":                   "testcommunity",
				"displayName":            "Test Community",
				"description":            "Test description",
				"visibility":             "public",
				"allowExternalDiscovery": true,
			}
			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject user DID into context (simulates auth middleware)
			ctx := context.WithValue(req.Context(), middleware.UserDIDKey, tc.requestDID)
			req = req.WithContext(ctx)

			// Execute handler
			w := httptest.NewRecorder()
			handler.HandleCreate(w, req)

			// Check status code
			if w.Code != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d. Body: %s", tc.expectedStatus, w.Code, w.Body.String())
			}

			// Check error response if expected
			if tc.expectedError != "" {
				var errResp struct {
					Error   string `json:"error"`
					Message string `json:"message"`
				}
				if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
					t.Fatalf("Failed to decode error response: %v", err)
				}
				if errResp.Error != tc.expectedError {
					t.Errorf("Expected error %s, got %s", tc.expectedError, errResp.Error)
				}
			}
		})
	}
}

func TestCreateHandler_RequiresAuth(t *testing.T) {
	mockService := &mockCommunityService{}
	handler := NewCreateHandler(mockService, nil)

	// Create request without auth context
	reqBody := map[string]interface{}{
		"name":        "testcommunity",
		"displayName": "Test Community",
		"description": "Test description",
		"visibility":  "public",
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No user DID in context

	w := httptest.NewRecorder()
	handler.HandleCreate(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
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
