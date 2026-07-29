//go:build integration

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

	"github.com/go-chi/chi/v5"
)

// getViewerMockService reuses mockCommunityService but implements GetCommunity
// against the real repository, since the get endpoint is what's under test.
type getViewerMockService struct {
	mockCommunityService
}

func (m *getViewerMockService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return m.repo.GetByDID(ctx, identifier)
}

// TestCommunityGet_ViewerState tests that the get community endpoint
// populates viewer.subscribed for authenticated users, matching the
// social.coves.community.get lexicon promise ("viewer state will be
// included if authenticated").
func TestCommunityGet_ViewerState(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	// Create two communities: the user subscribes to the first only
	baseSuffix := time.Now().UnixNano()
	communityDIDs := make([]string, 2)
	for i := 0; i < 2; i++ {
		communityDID := generateTestDID(fmt.Sprintf("%d%d", baseSuffix, i))
		communityDIDs[i] = communityDID
		comm := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("c-getviewer-%d-%d.coves.local", baseSuffix, i),
			Name:         fmt.Sprintf("getviewer-test-%d", i),
			DisplayName:  fmt.Sprintf("Get Viewer Test Community %d", i),
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

	testUserDID := fmt.Sprintf("did:plc:getviewertestuser%d", baseSuffix)
	sub := &communities.Subscription{
		UserDID:           testUserDID,
		CommunityDID:      communityDIDs[0],
		ContentVisibility: 3,
		SubscribedAt:      time.Now(),
	}
	if _, err := repo.Subscribe(ctx, sub); err != nil {
		t.Fatalf("Failed to subscribe to community 0: %v", err)
	}

	mockService := &getViewerMockService{mockCommunityService{repo: repo}}
	getHandler := community.NewGetHandler(mockService, repo)

	type getResponse struct {
		DID    string `json:"did"`
		Viewer *struct {
			Subscribed *bool `json:"subscribed"`
		} `json:"viewer"`
	}

	doGet := func(t *testing.T, r chi.Router, communityDID string) getResponse {
		t.Helper()
		req := httptest.NewRequest("GET", "/xrpc/social.coves.community.get?community="+communityDID, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp getResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}
		return resp
	}

	t.Run("authenticated subscriber sees viewer.subscribed=true", func(t *testing.T) {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := middleware.SetTestUserDID(req.Context(), testUserDID)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

		resp := doGet(t, r, communityDIDs[0])
		if resp.Viewer == nil || resp.Viewer.Subscribed == nil {
			t.Fatalf("Expected populated viewer.subscribed, got viewer=%+v", resp.Viewer)
		}
		if !*resp.Viewer.Subscribed {
			t.Errorf("Expected viewer.subscribed=true for subscribed community %s", communityDIDs[0])
		}
	})

	t.Run("authenticated non-subscriber sees viewer.subscribed=false", func(t *testing.T) {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := middleware.SetTestUserDID(req.Context(), testUserDID)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

		resp := doGet(t, r, communityDIDs[1])
		if resp.Viewer == nil || resp.Viewer.Subscribed == nil {
			t.Fatalf("Expected populated viewer.subscribed, got viewer=%+v", resp.Viewer)
		}
		if *resp.Viewer.Subscribed {
			t.Errorf("Expected viewer.subscribed=false for unsubscribed community %s", communityDIDs[1])
		}
	})

	t.Run("unauthenticated request has nil viewer state", func(t *testing.T) {
		r := chi.NewRouter()
		r.Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

		resp := doGet(t, r, communityDIDs[0])
		if resp.Viewer != nil {
			t.Errorf("Expected nil viewer for unauthenticated request, got %+v", resp.Viewer)
		}
	})
}
