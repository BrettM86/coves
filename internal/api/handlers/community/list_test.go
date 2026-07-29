package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// listTestService implements communities.Service for list handler tests
type listTestService struct {
	listFunc func(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error)
}

func (m *listTestService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *listTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, nil
}

func (m *listTestService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *listTestService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, req)
	}
	return []*communities.Community{}, nil
}

func (m *listTestService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *listTestService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	return nil, nil
}

func (m *listTestService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *listTestService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *listTestService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *listTestService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *listTestService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *listTestService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}

func (m *listTestService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, nil
}

func (m *listTestService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, nil
}

func (m *listTestService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *listTestService) ValidateHandle(handle string) error {
	return nil
}

func (m *listTestService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (m *listTestService) EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

func (m *listTestService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}

// listTestRepo implements communities.Repository for list handler tests
type listTestRepo struct{}

func (r *listTestRepo) Create(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return nil, nil
}
func (r *listTestRepo) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}
func (r *listTestRepo) GetByHandle(ctx context.Context, handle string) (*communities.Community, error) {
	return nil, nil
}
func (r *listTestRepo) Update(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return nil, nil
}
func (r *listTestRepo) Delete(ctx context.Context, did string) error { return nil }
func (r *listTestRepo) UpdateCredentials(ctx context.Context, did, accessToken, refreshToken string) error {
	return nil
}
func (r *listTestRepo) List(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return nil, nil
}
func (r *listTestRepo) Search(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}
func (r *listTestRepo) Subscribe(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) SubscribeWithCount(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) Unsubscribe(ctx context.Context, userDID, communityDID string) error {
	return nil
}
func (r *listTestRepo) UnsubscribeWithCount(ctx context.Context, userDID, communityDID string) error {
	return nil
}
func (r *listTestRepo) GetSubscription(ctx context.Context, userDID, communityDID string) (*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) GetSubscriptionByURI(ctx context.Context, recordURI string) (*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) ListSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) ListSubscribers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}
func (r *listTestRepo) GetSubscribedCommunityDIDs(ctx context.Context, userDID string, communityDIDs []string) (map[string]bool, error) {
	return nil, nil
}
func (r *listTestRepo) BlockCommunity(ctx context.Context, block *communities.CommunityBlock) (*communities.CommunityBlock, error) {
	return nil, nil
}
func (r *listTestRepo) UnblockCommunity(ctx context.Context, userDID, communityDID string) error {
	return nil
}
func (r *listTestRepo) GetBlock(ctx context.Context, userDID, communityDID string) (*communities.CommunityBlock, error) {
	return nil, nil
}
func (r *listTestRepo) GetBlockByURI(ctx context.Context, recordURI string) (*communities.CommunityBlock, error) {
	return nil, nil
}
func (r *listTestRepo) ListBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, nil
}
func (r *listTestRepo) IsBlocked(ctx context.Context, userDID, communityDID string) (bool, error) {
	return false, nil
}
func (r *listTestRepo) CreateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	return nil, nil
}
func (r *listTestRepo) GetMembership(ctx context.Context, userDID, communityDID string) (*communities.Membership, error) {
	return nil, nil
}
func (r *listTestRepo) UpdateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	return nil, nil
}
func (r *listTestRepo) ListMembers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}
func (r *listTestRepo) CreateModerationAction(ctx context.Context, action *communities.ModerationAction) (*communities.ModerationAction, error) {
	return nil, nil
}
func (r *listTestRepo) ListModerationActions(ctx context.Context, communityDID string, limit, offset int) ([]*communities.ModerationAction, error) {
	return nil, nil
}
func (r *listTestRepo) IncrementMemberCount(ctx context.Context, communityDID string) error {
	return nil
}
func (r *listTestRepo) DecrementMemberCount(ctx context.Context, communityDID string) error {
	return nil
}
func (r *listTestRepo) IncrementSubscriberCount(ctx context.Context, communityDID string) error {
	return nil
}
func (r *listTestRepo) DecrementSubscriberCount(ctx context.Context, communityDID string) error {
	return nil
}
func (r *listTestRepo) IncrementPostCount(ctx context.Context, communityDID string) error {
	return nil
}

// createListTestOAuthSession creates a mock OAuth session for testing
func createListTestOAuthSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session",
		HostURL:     "http://localhost:3001",
		AccessToken: "test-access-token",
	}
}

func TestListHandler_SubscribedWithoutAuth_Returns401(t *testing.T) {
	mockService := &listTestService{}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	// Request subscribed filter without authentication
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?subscribed=true", nil)

	w := httptest.NewRecorder()
	handler.HandleList(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify JSON error response format
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
	if errResp.Message == "" {
		t.Error("Expected non-empty error message")
	}
}

func TestListHandler_SubscribedWithAuth_FiltersCorrectly(t *testing.T) {
	userDID := "did:plc:testuser123"

	// Create communities - some subscribed, some not
	allCommunities := []*communities.Community{
		{
			DID:       "did:plc:community1",
			Handle:    "c-subscribed1.coves.social",
			Name:      "subscribed1",
			CreatedAt: time.Now(),
		},
		{
			DID:       "did:plc:community2",
			Handle:    "c-subscribed2.coves.social",
			Name:      "subscribed2",
			CreatedAt: time.Now(),
		},
	}

	var receivedRequest communities.ListCommunitiesRequest
	mockService := &listTestService{
		listFunc: func(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
			receivedRequest = req
			// Service should receive the SubscriberDID and filter accordingly
			if req.SubscriberDID != "" {
				// Return only subscribed communities
				return allCommunities, nil
			}
			// Return all communities if no filter
			return allCommunities, nil
		},
	}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?subscribed=true", nil)

	// Add authentication using the test helper
	ctx := middleware.SetTestUserDID(req.Context(), userDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify that the service received the SubscriberDID
	if receivedRequest.SubscriberDID != userDID {
		t.Errorf("Expected SubscriberDID %q to be passed to service, got %q", userDID, receivedRequest.SubscriberDID)
	}

	// Verify response contains communities
	var resp struct {
		Communities []map[string]interface{} `json:"communities"`
		Cursor      string                   `json:"cursor"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if len(resp.Communities) != 2 {
		t.Errorf("Expected 2 communities in response, got %d", len(resp.Communities))
	}
}

func TestListHandler_SubscribedFalse_NoFilter(t *testing.T) {
	var receivedRequest communities.ListCommunitiesRequest
	mockService := &listTestService{
		listFunc: func(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
			receivedRequest = req
			return []*communities.Community{}, nil
		},
	}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	// Request with subscribed=false should not require auth and should not filter
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?subscribed=false", nil)

	w := httptest.NewRecorder()
	handler.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify that SubscriberDID is empty (no filter)
	if receivedRequest.SubscriberDID != "" {
		t.Errorf("Expected empty SubscriberDID, got %q", receivedRequest.SubscriberDID)
	}
}

func TestListHandler_InvalidLimit_Returns400(t *testing.T) {
	mockService := &listTestService{}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?limit=abc", nil)

	w := httptest.NewRecorder()
	handler.HandleList(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// Verify JSON error response format
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "InvalidRequest" {
		t.Errorf("Expected error InvalidRequest, got %s", errResp.Error)
	}
}

func TestListHandler_InvalidCursor_Returns400(t *testing.T) {
	mockService := &listTestService{}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	tests := []struct {
		name   string
		cursor string
	}{
		{
			name:   "non-numeric cursor",
			cursor: "abc",
		},
		{
			name:   "negative cursor",
			cursor: "-5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?cursor="+tc.cursor, nil)

			w := httptest.NewRecorder()
			handler.HandleList(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Verify JSON error response format
			var errResp struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Error != "InvalidRequest" {
				t.Errorf("Expected error InvalidRequest, got %s", errResp.Error)
			}
		})
	}
}

func TestListHandler_ValidLimitBoundaries(t *testing.T) {
	tests := []struct {
		name          string
		limitParam    string
		expectedLimit int
	}{
		{
			name:          "limit below minimum clamped to 1",
			limitParam:    "0",
			expectedLimit: 1,
		},
		{
			name:          "limit above maximum clamped to 100",
			limitParam:    "150",
			expectedLimit: 100,
		},
		{
			name:          "valid limit in range",
			limitParam:    "25",
			expectedLimit: 25,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedRequest communities.ListCommunitiesRequest
			mockService := &listTestService{
				listFunc: func(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
					receivedRequest = req
					return []*communities.Community{}, nil
				},
			}
			mockRepo := &listTestRepo{}
			handler := NewListHandler(mockService, mockRepo)

			req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?limit="+tc.limitParam, nil)

			w := httptest.NewRecorder()
			handler.HandleList(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			if receivedRequest.Limit != tc.expectedLimit {
				t.Errorf("Expected limit %d, got %d", tc.expectedLimit, receivedRequest.Limit)
			}
		})
	}
}

func TestListHandler_MethodNotAllowed(t *testing.T) {
	mockService := &listTestService{}
	mockRepo := &listTestRepo{}
	handler := NewListHandler(mockService, mockRepo)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.list", nil)

	w := httptest.NewRecorder()
	handler.HandleList(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

// The list handler's query surface: which sorts and filters it accepts, what it
// refuses, and the response envelope.
//
// These came down a tier from tests/integration/community_e2e_test.go, whose
// "List with sort=…" subtests spent a real PDS, a real Jetstream and a
// provisioned community each to assert that a sort parameter produced HTTP 200
// (docs/TEST_ARCHITECTURE.md §3.4 rule 3: behavioural breadth is a T1/T0
// concern). Down here every case is covered instead of four of them, the
// parameter's onward journey into the service request is visible, and the
// ORDERING those sorts actually produce is asserted where it is implemented —
// internal/db/postgres's community repository, against real SQL.

// listedBy runs one list request and returns the service request it produced
// alongside the recorder, so a test can assert on both halves of the handler's
// job: what it forwarded, and what it answered.
func listedBy(t *testing.T, query string) (communities.ListCommunitiesRequest, *httptest.ResponseRecorder) {
	t.Helper()

	var forwarded communities.ListCommunitiesRequest
	handler := NewListHandler(&listTestService{
		listFunc: func(_ context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
			forwarded = req
			return []*communities.Community{}, nil
		},
	}, &listTestRepo{})

	w := httptest.NewRecorder()
	handler.HandleList(w, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?"+query, nil))
	return forwarded, w
}

func TestListHandler_SortParameter(t *testing.T) {
	t.Parallel()

	for _, sort := range []string{"popular", "active", "new", "alphabetical"} {
		t.Run(sort, func(t *testing.T) {
			t.Parallel()
			forwarded, w := listedBy(t, "sort="+sort)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for sort=%s, got %d: %s", sort, w.Code, w.Body.String())
			}
			if forwarded.Sort != sort {
				t.Errorf("expected the handler to forward sort=%q, forwarded %q", sort, forwarded.Sort)
			}
		})
	}
}

func TestListHandler_SortDefaultsToPopular(t *testing.T) {
	t.Parallel()

	// The default is applied in the handler rather than left empty for the
	// repository to interpret, so an omitted sort and sort=popular must produce
	// the identical service request.
	forwarded, w := listedBy(t, "limit=10")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if forwarded.Sort != "popular" {
		t.Errorf("expected an absent sort to default to popular, got %q", forwarded.Sort)
	}
}

func TestListHandler_InvalidSortIsRejected(t *testing.T) {
	t.Parallel()

	// KNOWN INCONSISTENCY, asserted as it is rather than as it should be: this
	// rejection is http.Error, so the body is plain text and carries no XRPC
	// error name — unlike the limit and cursor rejections a few lines above it
	// in the same handler. A client matching on the error name sees nothing to
	// match. Changing it is a client-visible API change and belongs in its own
	// commit; pinning it here means that commit cannot happen by accident.
	forwarded, w := listedBy(t, "sort=trending")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown sort, got %d: %s", w.Code, w.Body.String())
	}
	if forwarded.Sort != "" {
		t.Error("the handler queried the service with an invalid sort instead of rejecting the request")
	}
	if body := w.Body.String(); !strings.Contains(body, "popular, active, new, or alphabetical") {
		t.Errorf("the rejection should name the accepted sorts, got %q", body)
	}
}

func TestListHandler_VisibilityFilter(t *testing.T) {
	t.Parallel()

	for _, visibility := range []string{"public", "unlisted", "private"} {
		t.Run(visibility, func(t *testing.T) {
			t.Parallel()
			forwarded, w := listedBy(t, "visibility="+visibility)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for visibility=%s, got %d: %s", visibility, w.Code, w.Body.String())
			}
			if forwarded.Visibility != visibility {
				t.Errorf("expected the handler to forward visibility=%q, forwarded %q", visibility, forwarded.Visibility)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		// No default: an empty visibility is what tells the repository not to
		// filter at all, so substituting "public" here would silently hide
		// unlisted communities from every unfiltered listing.
		forwarded, _ := listedBy(t, "limit=10")
		if forwarded.Visibility != "" {
			t.Errorf("expected an absent visibility to stay empty, got %q", forwarded.Visibility)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		forwarded, w := listedBy(t, "visibility=secret")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an unknown visibility, got %d: %s", w.Code, w.Body.String())
		}
		if forwarded.Visibility != "" {
			t.Error("the handler queried the service with an invalid visibility instead of rejecting the request")
		}
	})
}

func TestListHandler_ResponseEnvelope(t *testing.T) {
	t.Parallel()

	// The cursor is the pagination contract: offset-based, and present only when
	// the page was full. A cursor returned on a short page would make a client
	// page forever.
	tests := []struct {
		name           string
		query          string
		returned       int
		expectedCursor string
	}{
		{name: "a short page ends the listing", query: "limit=10", returned: 3, expectedCursor: ""},
		{name: "a full page offers the next offset", query: "limit=3", returned: 3, expectedCursor: "3"},
		{name: "a full page continues from the cursor", query: "limit=3&cursor=6", returned: 3, expectedCursor: "9"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			results := make([]*communities.Community, tc.returned)
			for i := range results {
				results[i] = &communities.Community{
					DID:             "did:plc:listed" + strconv.Itoa(i),
					Handle:          "c-listed" + strconv.Itoa(i) + ".coves.social",
					Name:            "listed" + strconv.Itoa(i),
					Visibility:      "public",
					PDSPassword:     "hunter2",
					PDSAccessToken:  "access-jwt",
					PDSRefreshToken: "refresh-jwt",
				}
			}

			handler := NewListHandler(&listTestService{
				listFunc: func(_ context.Context, _ communities.ListCommunitiesRequest) ([]*communities.Community, error) {
					return results, nil
				},
			}, &listTestRepo{})

			w := httptest.NewRecorder()
			handler.HandleList(w, httptest.NewRequest(http.MethodGet,
				"/xrpc/social.coves.community.list?"+tc.query, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}

			var response struct {
				Communities []communities.CommunityView `json:"communities"`
				Cursor      *string                     `json:"cursor"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("decoding the list response: %v (body %q)", err, w.Body.String())
			}
			if len(response.Communities) != tc.returned {
				t.Errorf("expected %d communities, got %d", tc.returned, len(response.Communities))
			}
			// Always present, even when empty: a client that reads a missing key
			// as "no more pages" and an empty string as "page 0" would loop.
			if response.Cursor == nil {
				t.Fatalf("the response omitted the cursor key entirely: %s", w.Body.String())
			}
			if *response.Cursor != tc.expectedCursor {
				t.Errorf("expected cursor %q, got %q", tc.expectedCursor, *response.Cursor)
			}
			if strings.Contains(w.Body.String(), "hunter2") ||
				strings.Contains(w.Body.String(), "-jwt") {
				t.Error("a community's PDS credentials reached a client through the list view")
			}
		})
	}
}
