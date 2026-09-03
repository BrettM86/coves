//go:build integration

// Viewer state is the one part of a community response that the handler cannot
// answer from the service layer: social.coves.community.get and
// social.coves.community.list both promise that "viewer state will be included
// if authenticated", and both satisfy that promise by going back to the
// communities repository with the caller's DID and asking which of the
// communities in the response the caller is subscribed to.
//
// That second query is what these tests cover, and it is why they need a real
// database. The in-package unit tests in get_test.go and list_test.go drive the
// same handlers against a hand-written repository, which proves the handler
// calls the seam but cannot prove the SQL behind it answers correctly — a
// subscription lookup that silently returned the empty set would pass every one
// of them. Here the subscriptions are real rows, so a wrong join, a wrong
// column or a missing WHERE shows up as viewer.subscribed being wrong.
//
// The file is in the external test package because it imports
// internal/db/postgres, which pulls in the domain; the established form for
// every relocated integration test in this tree is package foo_test.
package community_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/community"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// repositoryBackedService is a communities.Service that answers only the reads
// the two endpoints under test make, and answers them from the real repository.
//
// The point of the fake is to take the SERVICE out of the picture without
// taking the DATABASE out of it: the handler asks the service for the
// communities and asks the repository for the viewer's subscriptions, and it is
// the second call this file is about. Every method the endpoints never reach
// returns an error rather than a zero value, so a handler that starts calling
// one fails loudly instead of quietly seeing an empty result.
type repositoryBackedService struct {
	repo communities.Repository
}

func (s *repositoryBackedService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return s.repo.List(ctx, req)
}

func (s *repositoryBackedService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return s.repo.GetByDID(ctx, identifier)
}

func (s *repositoryBackedService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return s.repo.GetByDID(ctx, did)
}

func (s *repositoryBackedService) CreateCommunity(context.Context, communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, fmt.Errorf("repositoryBackedService: CreateCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) UpdateCommunity(context.Context, communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, fmt.Errorf("repositoryBackedService: UpdateCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) SearchCommunities(context.Context, communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, fmt.Errorf("repositoryBackedService: SearchCommunities is not part of the viewer-state seam")
}

func (s *repositoryBackedService) SubscribeToCommunity(context.Context, *oauth.ClientSessionData, string, int) (*communities.Subscription, error) {
	return nil, fmt.Errorf("repositoryBackedService: SubscribeToCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) UnsubscribeFromCommunity(context.Context, *oauth.ClientSessionData, string) error {
	return fmt.Errorf("repositoryBackedService: UnsubscribeFromCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) GetUserSubscriptions(context.Context, string, int, int) ([]*communities.Subscription, error) {
	return nil, fmt.Errorf("repositoryBackedService: GetUserSubscriptions is not part of the viewer-state seam")
}

func (s *repositoryBackedService) GetCommunitySubscribers(context.Context, string, int, int) ([]*communities.Subscription, error) {
	return nil, fmt.Errorf("repositoryBackedService: GetCommunitySubscribers is not part of the viewer-state seam")
}

func (s *repositoryBackedService) BlockCommunity(context.Context, *oauth.ClientSessionData, string) (*communities.CommunityBlock, error) {
	return nil, fmt.Errorf("repositoryBackedService: BlockCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) UnblockCommunity(context.Context, *oauth.ClientSessionData, string) error {
	return fmt.Errorf("repositoryBackedService: UnblockCommunity is not part of the viewer-state seam")
}

func (s *repositoryBackedService) GetBlockedCommunities(context.Context, string, int, int) ([]*communities.CommunityBlock, error) {
	return nil, fmt.Errorf("repositoryBackedService: GetBlockedCommunities is not part of the viewer-state seam")
}

func (s *repositoryBackedService) IsBlocked(context.Context, string, string) (bool, error) {
	return false, fmt.Errorf("repositoryBackedService: IsBlocked is not part of the viewer-state seam")
}

func (s *repositoryBackedService) GetMembership(context.Context, string, string) (*communities.Membership, error) {
	return nil, fmt.Errorf("repositoryBackedService: GetMembership is not part of the viewer-state seam")
}

func (s *repositoryBackedService) ListCommunityMembers(context.Context, string, int, int) ([]*communities.Membership, error) {
	return nil, fmt.Errorf("repositoryBackedService: ListCommunityMembers is not part of the viewer-state seam")
}

func (s *repositoryBackedService) ValidateHandle(string) error { return nil }

func (s *repositoryBackedService) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (s *repositoryBackedService) EnsureFreshToken(_ context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

// viewerEnvelope is the slice of the lexicon response these tests read. Both
// endpoints use the same optional shape, and the two levels of pointer are
// load bearing: a nil Viewer means "the response omitted viewer state
// entirely", which is what an unauthenticated caller must see, while a
// non-nil Viewer with a nil Subscribed would mean the field was present but
// never filled in. Decoding into plain bools would collapse both mistakes into
// "false" and the unauthenticated cases below would pass no matter what.
type viewerEnvelope struct {
	Subscribed *bool `json:"subscribed"`
}

// authenticatedAs builds a router that runs the handler behind middleware which
// puts userDID in the request context, the same key the real OAuth middleware
// writes. Passing an empty DID gives the unauthenticated router.
func authenticatedAs(userDID, pattern string, handler http.HandlerFunc) chi.Router {
	router := chi.NewRouter()
	if userDID != "" {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(middleware.SetTestUserDID(req.Context(), userDID)))
			})
		})
	}
	router.Get(pattern, handler)
	return router
}

// seedCommunities inserts count communities and returns their DIDs.
func seedCommunities(t *testing.T, repo communities.Repository, label string, count int) []string {
	t.Helper()

	ctx := context.Background()
	dids := make([]string, count)
	for i := 0; i < count; i++ {
		id := testkit.UniqueID(t)
		dids[i] = fixtures.DID(id)
		community := &communities.Community{
			DID:          dids[i],
			Handle:       fmt.Sprintf("c-%s%d.coves.local", id, i),
			Name:         fmt.Sprintf("%s-%s-%d", label, id, i),
			DisplayName:  fmt.Sprintf("%s community %d", label, i),
			OwnerDID:     fixtures.InstanceDID(),
			CreatedByDID: fixtures.DID("creator" + id),
			HostedByDID:  fixtures.InstanceDID(),
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("creating %s community %d: %v", label, i, err)
		}
	}
	return dids
}

// subscribe records userDID as a subscriber of communityDID.
func subscribe(t *testing.T, repo communities.Repository, userDID, communityDID string) {
	t.Helper()

	if _, err := repo.Subscribe(context.Background(), &communities.Subscription{
		UserDID:      userDID,
		CommunityDID: communityDID,
		// 3 is the default content-visibility level; the endpoints under test
		// only care that a subscription row exists.
		ContentVisibility: 3,
		SubscribedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("subscribing %s to %s: %v", userDID, communityDID, err)
	}
}

// TestCommunityGet_ViewerState covers social.coves.community.get: the response
// carries viewer.subscribed for an authenticated caller, and carries no viewer
// object at all for an anonymous one.
func TestCommunityGet_ViewerState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	// Two communities so that "subscribed" and "not subscribed" are answered
	// from the same database state: a handler that hardcoded either answer
	// would fail one of the two subtests.
	communityDIDs := seedCommunities(t, repo, "getviewer", 2)

	viewerDID := fixtures.DID("getviewer" + testkit.UniqueID(t))
	subscribe(t, repo, viewerDID, communityDIDs[0])

	handler := community.NewGetHandler(&repositoryBackedService{repo: repo}, repo)

	get := func(t *testing.T, router chi.Router, communityDID string) *viewerEnvelope {
		t.Helper()

		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET community %s: expected 200, got %d: %s", communityDID, rec.Code, rec.Body.String())
		}
		var response struct {
			DID    string          `json:"did"`
			Viewer *viewerEnvelope `json:"viewer"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decoding get response: %v", err)
		}
		return response.Viewer
	}

	t.Run("authenticated subscriber sees viewer.subscribed=true", func(t *testing.T) {
		router := authenticatedAs(viewerDID, "/xrpc/social.coves.community.get", handler.HandleGet)

		viewer := get(t, router, communityDIDs[0])
		if viewer == nil || viewer.Subscribed == nil {
			t.Fatalf("expected populated viewer state, got %+v", viewer)
		}
		if !*viewer.Subscribed {
			t.Errorf("expected viewer.subscribed=true for subscribed community %s", communityDIDs[0])
		}
	})

	t.Run("authenticated non-subscriber sees viewer.subscribed=false", func(t *testing.T) {
		router := authenticatedAs(viewerDID, "/xrpc/social.coves.community.get", handler.HandleGet)

		viewer := get(t, router, communityDIDs[1])
		if viewer == nil || viewer.Subscribed == nil {
			t.Fatalf("expected populated viewer state, got %+v", viewer)
		}
		if *viewer.Subscribed {
			t.Errorf("expected viewer.subscribed=false for unsubscribed community %s", communityDIDs[1])
		}
	})

	t.Run("unauthenticated request has no viewer state", func(t *testing.T) {
		router := authenticatedAs("", "/xrpc/social.coves.community.get", handler.HandleGet)

		if viewer := get(t, router, communityDIDs[0]); viewer != nil {
			t.Errorf("expected the viewer object to be omitted for an anonymous caller, got %+v", viewer)
		}
	})
}

// TestCommunityList_ViewerState covers social.coves.community.list, where the
// viewer's subscriptions have to be matched up against a whole page of
// communities rather than a single one — the case where an off-by-one in the
// join would show up as the right count of subscriptions attached to the wrong
// communities.
func TestCommunityList_ViewerState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	communityDIDs := seedCommunities(t, repo, "listviewer", 3)

	viewerDID := fixtures.DID("listviewer" + testkit.UniqueID(t))
	// Subscribed to the first and the last, deliberately skipping the middle:
	// a handler that attached viewer state positionally rather than by DID
	// would mislabel the second and third entries.
	subscribe(t, repo, viewerDID, communityDIDs[0])
	subscribe(t, repo, viewerDID, communityDIDs[2])

	handler := community.NewListHandler(&repositoryBackedService{repo: repo}, repo)

	list := func(t *testing.T, router chi.Router) map[string]*viewerEnvelope {
		t.Helper()

		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.list?limit=50", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("LIST communities: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var response struct {
			Communities []struct {
				DID    string          `json:"did"`
				Viewer *viewerEnvelope `json:"viewer"`
			} `json:"communities"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decoding list response: %v", err)
		}

		byDID := make(map[string]*viewerEnvelope, len(response.Communities))
		for _, entry := range response.Communities {
			byDID[entry.DID] = entry.Viewer
		}
		return byDID
	}

	t.Run("authenticated user sees viewer.subscribed per community", func(t *testing.T) {
		router := authenticatedAs(viewerDID, "/xrpc/social.coves.community.list", handler.HandleList)
		viewers := list(t, router)

		expected := map[string]bool{
			communityDIDs[0]: true,
			communityDIDs[1]: false,
			communityDIDs[2]: true,
		}
		for communityDID, wantSubscribed := range expected {
			viewer, present := viewers[communityDID]
			if !present {
				t.Errorf("community %s missing from the listing", communityDID)
				continue
			}
			if viewer == nil || viewer.Subscribed == nil {
				t.Errorf("community %s: expected populated viewer state, got %+v", communityDID, viewer)
				continue
			}
			if *viewer.Subscribed != wantSubscribed {
				t.Errorf("community %s: expected subscribed=%v, got %v",
					communityDID, wantSubscribed, *viewer.Subscribed)
			}
		}
	})

	t.Run("unauthenticated request has no viewer state", func(t *testing.T) {
		router := authenticatedAs("", "/xrpc/social.coves.community.list", handler.HandleList)

		for communityDID, viewer := range list(t, router) {
			if viewer != nil {
				t.Errorf("community %s carried viewer state for an anonymous caller: %+v", communityDID, viewer)
			}
		}
	})
}
