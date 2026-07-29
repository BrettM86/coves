package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The create handler's contract with a client: which fields it refuses to take
// from the request, and what a success answers.
//
// The refusals came down a tier from tests/integration/community_e2e_test.go's
// "Create via XRPC endpoint" subtest, which stated them only as a comment
// ("NOTE: Both createdByDid and hostedByDid are derived server-side") while
// asserting neither. They are the community domain's authorship boundary — a
// client that could set createdByDid would create communities in someone else's
// name, and one that could set hostedByDid would claim this instance hosts a
// community for a domain it does not own — so they are worth an actual test.

// createdBy runs one create request as userDID and returns the service request
// it produced alongside the recorder.
func createdBy(t *testing.T, userDID string, body map[string]any) (communities.CreateCommunityRequest, *httptest.ResponseRecorder) {
	t.Helper()

	var forwarded communities.CreateCommunityRequest
	handler := NewCreateHandler(&mockCommunityService{
		createFunc: func(_ context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
			forwarded = req
			return &communities.Community{
				DID:       "did:plc:created",
				Handle:    "c-" + req.Name + ".coves.social",
				RecordURI: "at://did:plc:created/social.coves.community.profile/self",
				RecordCID: "bafycreated",
				// Seeded so the response assertion can prove the handler serves
				// a hand-built envelope rather than the entity — see
				// assertRecordWriteEnvelope in update_test.go. A community
				// created through the service really does carry these.
				PDSPassword:     "hunter2",
				PDSAccessToken:  "access-jwt",
				PDSRefreshToken: "refresh-jwt",
			}, nil
		},
	}, nil)

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if userDID != "" {
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, userDID))
	}

	w := httptest.NewRecorder()
	handler.HandleCreate(w, req)
	return forwarded, w
}

func TestCreateHandler_DerivesTheCreatorFromTheSession(t *testing.T) {
	t.Parallel()

	forwarded, w := createdBy(t, "did:plc:author", map[string]any{
		"name":        "gaming",
		"displayName": "Gaming",
		"visibility":  "public",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if forwarded.CreatedByDID != "did:plc:author" {
		t.Errorf("expected the authenticated DID as the creator, service saw %q", forwarded.CreatedByDID)
	}
	if forwarded.HostedByDID != "" {
		t.Errorf("the handler must leave hostedByDid empty for the service to stamp, forwarded %q", forwarded.HostedByDID)
	}
}

func TestCreateHandler_RefusesClientSuppliedAuthorship(t *testing.T) {
	t.Parallel()

	// Refused outright rather than overwritten: silently replacing a supplied
	// createdByDid would let a client believe it had created a community on
	// another user's behalf, and the 400 is what tells it otherwise.
	for _, field := range []string{"createdByDid", "hostedByDid"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			forwarded, w := createdBy(t, "did:plc:author", map[string]any{
				"name":       "gaming",
				"visibility": "public",
				field:        "did:plc:someoneelse",
			})

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when a client supplies %s, got %d: %s", field, w.Code, w.Body.String())
			}
			if forwarded.Name != "" {
				t.Errorf("the handler forwarded the request to the service instead of rejecting it")
			}

			var errResp struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("decoding the error response: %v (body %q)", err, w.Body.String())
			}
			if errResp.Error != "InvalidRequest" {
				t.Errorf("expected error InvalidRequest, got %q", errResp.Error)
			}
			if !strings.Contains(errResp.Message, field) {
				t.Errorf("the message should name the offending field %q, got %q", field, errResp.Message)
			}
		})
	}
}

func TestCreateHandler_SuccessResponse(t *testing.T) {
	t.Parallel()

	// The four fields social.coves.community.create's lexicon promises, and only
	// those four. A client writes the community's first post against this uri
	// and addresses it by this did, so a renamed key is a broken client rather
	// than a cosmetic change — and an EXTRA key is a credential leak, which is
	// why the assertion is on the exact key set.
	_, w := createdBy(t, "did:plc:author", map[string]any{
		"name":       "gaming",
		"visibility": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	assertRecordWriteEnvelope(t, w, map[string]string{
		"uri":    "at://did:plc:created/social.coves.community.profile/self",
		"cid":    "bafycreated",
		"did":    "did:plc:created",
		"handle": "c-gaming.coves.social",
	})
}

func TestCreateHandler_MalformedBody(t *testing.T) {
	t.Parallel()

	handler := NewCreateHandler(&mockCommunityService{
		createFunc: func(context.Context, communities.CreateCommunityRequest) (*communities.Community, error) {
			t.Error("the service was called with an undecodable request body")
			return nil, nil
		},
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, "did:plc:author"))

	w := httptest.NewRecorder()
	handler.HandleCreate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed body, got %d: %s", w.Code, w.Body.String())
	}
}
