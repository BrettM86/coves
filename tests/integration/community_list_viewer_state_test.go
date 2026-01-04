package integration

import (
	"Coves/internal/api/handlers/community"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// TestCommunityList_ViewerState tests that the list communities endpoint
// correctly populates viewer.subscribed field for authenticated users
func TestCommunityList_ViewerState(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	// Create test communities
	baseSuffix := time.Now().UnixNano()
	communityDIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		uniqueSuffix := fmt.Sprintf("%d%d", baseSuffix, i)
		communityDID := generateTestDID(uniqueSuffix)
		communityDIDs[i] = communityDID
		comm := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("c-viewer-test-%d-%d.coves.local", baseSuffix, i),
			Name:         fmt.Sprintf("viewer-test-%d", i),
			DisplayName:  fmt.Sprintf("Viewer Test Community %d", i),
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:testcreator",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := repo.Create(ctx, comm); err != nil {
			t.Fatalf("Failed to create community %d: %v", i, err)
		}
	}

	// Create a test user and subscribe them to community 0 and 2
	testUserDID := fmt.Sprintf("did:plc:viewertestuser%d", baseSuffix)

	sub1 := &communities.Subscription{
		UserDID:           testUserDID,
		CommunityDID:      communityDIDs[0],
		ContentVisibility: 3,
		SubscribedAt:      time.Now(),
	}
	if _, err := repo.Subscribe(ctx, sub1); err != nil {
		t.Fatalf("Failed to subscribe to community 0: %v", err)
	}

	sub2 := &communities.Subscription{
		UserDID:           testUserDID,
		CommunityDID:      communityDIDs[2],
		ContentVisibility: 3,
		SubscribedAt:      time.Now(),
	}
	if _, err := repo.Subscribe(ctx, sub2); err != nil {
		t.Fatalf("Failed to subscribe to community 2: %v", err)
	}

	// Create mock service that returns our communities
	mockService := &mockCommunityService{
		repo: repo,
	}

	// Create handler with real repo for viewer state population
	listHandler := community.NewListHandler(mockService, repo)

	t.Run("authenticated user sees viewer.subscribed correctly", func(t *testing.T) {
		// Setup router with middleware that injects user DID
		r := chi.NewRouter()

		// Use test middleware that sets user DID in context
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := middleware.SetTestUserDID(req.Context(), testUserDID)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Get("/xrpc/social.coves.community.list", listHandler.HandleList)

		req := httptest.NewRequest("GET", "/xrpc/social.coves.community.list?limit=50", nil)
		rec := httptest.NewRecorder()

		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var response struct {
			Communities []struct {
				DID    string `json:"did"`
				Viewer *struct {
					Subscribed *bool `json:"subscribed"`
				} `json:"viewer"`
			} `json:"communities"`
		}

		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// Check that viewer state is populated correctly
		subscriptionMap := map[string]bool{
			communityDIDs[0]: true,
			communityDIDs[1]: false,
			communityDIDs[2]: true,
		}

		for _, comm := range response.Communities {
			expectedSubscribed, inTestSet := subscriptionMap[comm.DID]
			if !inTestSet {
				continue // Skip communities not in our test set
			}

			if comm.Viewer == nil {
				t.Errorf("Community %s has nil Viewer, expected populated", comm.DID)
				continue
			}

			if comm.Viewer.Subscribed == nil {
				t.Errorf("Community %s has nil Viewer.Subscribed, expected populated", comm.DID)
				continue
			}

			if *comm.Viewer.Subscribed != expectedSubscribed {
				t.Errorf("Community %s: expected subscribed=%v, got %v",
					comm.DID, expectedSubscribed, *comm.Viewer.Subscribed)
			}
		}
	})

	t.Run("unauthenticated request has nil viewer state", func(t *testing.T) {
		// Setup router WITHOUT middleware that sets user DID
		r := chi.NewRouter()
		r.Get("/xrpc/social.coves.community.list", listHandler.HandleList)

		req := httptest.NewRequest("GET", "/xrpc/social.coves.community.list?limit=50", nil)
		rec := httptest.NewRecorder()

		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var response struct {
			Communities []struct {
				DID    string `json:"did"`
				Viewer *struct {
					Subscribed *bool `json:"subscribed"`
				} `json:"viewer"`
			} `json:"communities"`
		}

		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// For unauthenticated requests, viewer should be nil for all communities
		for _, comm := range response.Communities {
			if comm.Viewer != nil {
				t.Errorf("Community %s has non-nil Viewer for unauthenticated request", comm.DID)
			}
		}
	})
}

// mockCommunityService implements communities.Service for testing
type mockCommunityService struct {
	repo communities.Repository
}

func (m *mockCommunityService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return m.repo.List(ctx, req)
}

func (m *mockCommunityService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return fmt.Errorf("not implemented")
}

func (m *mockCommunityService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return fmt.Errorf("not implemented")
}

func (m *mockCommunityService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCommunityService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, fmt.Errorf("not implemented")
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
	return m.repo.GetByDID(ctx, did)
}
