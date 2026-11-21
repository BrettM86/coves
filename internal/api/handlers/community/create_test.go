package community

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

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

func (m *mockCommunityService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int, favorite *bool) (*communities.Subscription, error) {
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

// adminSet is an InstanceAdminChecker that recognizes exactly the DIDs it holds.
type adminSet map[string]bool

func (a adminSet) IsInstanceAdmin(did string) bool { return a[did] }

// TestCreateHandler_CreationPolicy pins who the COMMUNITY_CREATION policy lets
// through: anyone lets an ordinary account create, instance-admins refuses it
// with CommunityCreationRestricted before the service is reached, and a zero
// policy (nothing configured) fails closed rather than open.
func TestCreateHandler_CreationPolicy(t *testing.T) {
	t.Parallel()

	const (
		adminDID  = "did:plc:creationadmin"
		memberDID = "did:plc:creationmember"
	)

	tests := []struct {
		name               string
		policy             communities.CreationPolicy
		callerDID          string
		wantStatus         int
		wantError          string
		wantServiceReached bool
	}{
		{name: "anyone lets a non-admin create", policy: communities.CreationAnyone, callerDID: memberDID, wantStatus: http.StatusOK, wantServiceReached: true},
		{name: "instance-admins refuses a non-admin", policy: communities.CreationInstanceAdmins, callerDID: memberDID, wantStatus: http.StatusForbidden, wantError: "CommunityCreationRestricted", wantServiceReached: false},
		{name: "instance-admins lets an admin create", policy: communities.CreationInstanceAdmins, callerDID: adminDID, wantStatus: http.StatusOK, wantServiceReached: true},
		{name: "unset policy fails closed for a non-admin", policy: "", callerDID: memberDID, wantStatus: http.StatusForbidden, wantError: "CommunityCreationRestricted", wantServiceReached: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reached := false
			handler := NewCreateHandler(&mockCommunityService{
				createFunc: func(context.Context, communities.CreateCommunityRequest) (*communities.Community, error) {
					reached = true
					return &communities.Community{DID: "did:plc:created", Handle: "c-gardening.coves.social"}, nil
				},
			}, tc.policy, adminSet{adminDID: true})

			bodyBytes, err := json.Marshal(map[string]any{"name": "gardening", "visibility": "public"})
			if err != nil {
				t.Fatalf("encoding the request body: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, tc.callerDID))

			w := httptest.NewRecorder()
			handler.HandleCreate(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if reached != tc.wantServiceReached {
				t.Fatalf("service reached = %v, want %v", reached, tc.wantServiceReached)
			}
			if tc.wantError != "" {
				var errResp struct {
					Error string `json:"error"`
				}
				if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
					t.Fatalf("decoding the error response: %v", err)
				}
				if errResp.Error != tc.wantError {
					t.Fatalf("expected error %q, got %q", tc.wantError, errResp.Error)
				}
			}
		})
	}
}

func TestCreateHandler_RequiresAuth(t *testing.T) {
	mockService := &mockCommunityService{}
	handler := NewCreateHandler(mockService, communities.CreationAnyone, adminSet{})

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
	}, communities.CreationAnyone, adminSet{})

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
	}, communities.CreationAnyone, adminSet{})

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

// A visibility the lexicon does not define is the client's mistake, so it must
// answer 400. The handler is driven with the REAL service, so this fails for
// the real reason if the service's error is one the mapper does not recognise.
// The service's PDS and provisioner are aimed at "://", which net/url refuses
// before any socket is opened: a request that got past validation fails there,
// and validation is never what answers. The repository is nil because
// validation runs before any lookup.
func TestCreateHandler_InvalidVisibilityIsABadRequest(t *testing.T) {
	t.Parallel()

	const unparseablePDSURL = "://"
	service := communities.NewCommunityServiceWithPDSFactory(
		nil, unparseablePDSURL, "did:web:coves.example", "coves.example",
		communities.NewPDSAccountProvisioner("coves.example", unparseablePDSURL, communities.PrivateHostOptions(true)...),
		nil, nil, nil, append(communities.PrivateHostOptions(true),
			communities.WithRoleAuthorization(unusedRoleIndex{}, viewerRoles{}))...)
	handler := NewCreateHandler(service, communities.CreationAnyone, adminSet{})

	encoded, err := json.Marshal(map[string]any{
		"name":        "gardening",
		"displayName": "Gardening",
		"visibility":  "secret",
	})
	if err != nil {
		t.Fatalf("encoding the request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.create", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, "did:plc:author"))

	w := httptest.NewRecorder()
	handler.HandleCreate(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for visibility \"secret\", got %d: %s", w.Code, w.Body.String())
	}
}

// unusedRoleIndex is a role index for a create that validation refuses before
// any role is written.
type unusedRoleIndex struct{}

func (unusedRoleIndex) ListCommunityRoles(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, nil
}

func (unusedRoleIndex) ListRolesBySubject(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, nil
}

func (unusedRoleIndex) UpsertCommunityRole(context.Context, communities.CommunityRole) error {
	return nil
}

func (unusedRoleIndex) DeleteCommunityRole(context.Context, string, string) error { return nil }

func (unusedRoleIndex) UpsertCommunityRoleAtRev(context.Context, communities.CommunityRole, string) (bool, error) {
	return false, nil
}

func (unusedRoleIndex) ApplyRoleRecordEvent(context.Context, communities.RoleRecordEvent) (bool, error) {
	return false, nil
}
