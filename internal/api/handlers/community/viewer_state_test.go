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
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// repositoryBackedService is a communities.Service that answers only the reads
// the endpoints under test make (get and list here, plus search in
// community_origin_view_test.go), and answers them from the real repository.
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

func (s *repositoryBackedService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return s.repo.Search(ctx, req)
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
	Blocked    *bool `json:"blocked"`
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

// block records userDID's aggregate-feed preference for communityDID.
func block(t *testing.T, repo communities.Repository, userDID, communityDID string) {
	t.Helper()

	id := testkit.UniqueID(t)
	if _, err := repo.BlockCommunity(context.Background(), &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: communityDID,
		RecordURI:    "at://" + userDID + "/social.coves.community.block/" + id,
		RecordCID:    "bafy" + id,
		BlockedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("blocking community %s for %s: %v", communityDID, userDID, err)
	}
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
// carries viewer.subscribed and viewer.blocked for an authenticated caller,
// and carries no viewer object at all for an anonymous one.
func TestCommunityGet_ViewerState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	// Three communities keep subscription and block state independent: one is
	// subscribed only, one is blocked only, and one has neither relationship.
	// A handler deriving blocked from the subscription row fails this matrix.
	communityDIDs := seedCommunities(t, repo, "getviewer", 3)

	viewerDID := fixtures.DID("getviewer" + testkit.UniqueID(t))
	subscribe(t, repo, viewerDID, communityDIDs[0])
	block(t, repo, viewerDID, communityDIDs[1])

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
		if viewer.Blocked == nil || *viewer.Blocked {
			t.Errorf("expected viewer.blocked=false for subscribed-only community %s, got %+v", communityDIDs[0], viewer)
		}
	})

	t.Run("authenticated blocked non-subscriber sees independent viewer state", func(t *testing.T) {
		router := authenticatedAs(viewerDID, "/xrpc/social.coves.community.get", handler.HandleGet)

		viewer := get(t, router, communityDIDs[1])
		if viewer == nil || viewer.Subscribed == nil {
			t.Fatalf("expected populated viewer state, got %+v", viewer)
		}
		if *viewer.Subscribed {
			t.Errorf("expected viewer.subscribed=false for unsubscribed community %s", communityDIDs[1])
		}
		if viewer.Blocked == nil || !*viewer.Blocked {
			t.Errorf("expected viewer.blocked=true for blocked-but-unsubscribed community %s, got %+v", communityDIDs[1], viewer)
		}
	})

	t.Run("authenticated viewer sees false for both absent relationships", func(t *testing.T) {
		router := authenticatedAs(viewerDID, "/xrpc/social.coves.community.get", handler.HandleGet)

		viewer := get(t, router, communityDIDs[2])
		if viewer == nil || viewer.Subscribed == nil || viewer.Blocked == nil {
			t.Fatalf("expected populated viewer state, got %+v", viewer)
		}
		if *viewer.Subscribed || *viewer.Blocked {
			t.Errorf("expected subscribed=false and blocked=false for community %s, got %+v", communityDIDs[2], viewer)
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

// TestCommunityGet_ViewerBanState covers the ban half of community.get's viewer
// state, read from the real ban index. The response is decoded as a map so an
// absent key is told apart from a present null or false: a banned flag, URI,
// reason or expiry the viewer is not subject to must not be served at all.
func TestCommunityGet_ViewerBanState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	bans := postgres.NewCommunityBanRepository(db)

	// Fixed DIDs, so the served ban URI is a literal. The rkey is the unpadded
	// lowercase base32 SHA-256 of the viewer DID, computed outside the code:
	// printf '%s' did:plc:banviewera | openssl dgst -sha256 -binary | base32 | tr -d '=' | tr A-Z a-z
	const (
		viewerDID       = "did:plc:banviewera"
		viewerBanRKey   = "pionjobpr3fgp6blueso6t5f4onskrd6zfmfkb7owv64acoexbtq"
		temporaryDID    = "did:plc:banviewtemporary"
		permanentDID    = "did:plc:banviewpermanent"
		lapsedDID       = "did:plc:banviewlapsed"
		unrelatedDID    = "did:plc:banviewnoban"
		temporaryExpiry = "2099-01-02T03:04:05Z"
		lapsedExpiry    = "2001-01-02T03:04:05Z"
	)
	for _, did := range []string{temporaryDID, permanentDID, lapsedDID, unrelatedDID} {
		name := did[len("did:plc:"):]
		if _, err := repo.Create(ctx, &communities.Community{
			DID:          did,
			Handle:       "c-" + name + ".coves.local",
			Name:         name,
			DisplayName:  name,
			OwnerDID:     fixtures.InstanceDID(),
			CreatedByDID: fixtures.DID("creator" + name),
			HostedByDID:  fixtures.InstanceDID(),
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("creating community %s: %v", did, err)
		}
	}

	seedBan := func(communityDID, expiresAt string) {
		t.Helper()
		ban := communities.CommunityBan{
			CommunityDID: communityDID,
			SubjectDID:   viewerDID,
			RecordKey:    communities.BanRecordKey(viewerDID),
			RecordCID:    "bafybanviewrecord",
			Reason:       "spam",
		}
		if expiresAt != "" {
			expiry, err := time.Parse(time.RFC3339, expiresAt)
			if err != nil {
				t.Fatalf("parsing expiry literal %q: %v", expiresAt, err)
			}
			ban.ExpiresAt = &expiry
		}
		if err := bans.UpsertCommunityBan(ctx, ban); err != nil {
			t.Fatalf("seeding ban in %s: %v", communityDID, err)
		}
	}
	seedBan(temporaryDID, temporaryExpiry)
	seedBan(permanentDID, "")
	seedBan(lapsedDID, lapsedExpiry)

	handler := community.NewGetHandler(&repositoryBackedService{repo: repo}, repo, community.WithCommunityBans(bans))

	// get returns the served viewer object, or nil when the response has none.
	get := func(t *testing.T, callerDID, communityDID string) map[string]any {
		t.Helper()

		router := authenticatedAs(callerDID, "/xrpc/social.coves.community.get", handler.HandleGet)
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET community %s: expected 200, got %d: %s", communityDID, rec.Code, rec.Body.String())
		}
		var response map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decoding get response: %v", err)
		}
		raw, present := response["viewer"]
		if !present {
			return nil
		}
		viewer, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("viewer must be an object, got %#v", raw)
		}
		return viewer
	}

	banKeys := []string{"banned", "banUri", "banReason", "banExpiresAt"}

	t.Run("active temporary ban serves banned, uri, reason and expiry", func(t *testing.T) {
		viewer := get(t, viewerDID, temporaryDID)
		if viewer == nil {
			t.Fatalf("expected a viewer object for an authenticated caller")
		}
		want := map[string]any{
			"banned":       true,
			"banUri":       "at://did:plc:banviewtemporary/social.coves.moderation.ban/" + viewerBanRKey,
			"banReason":    "spam",
			"banExpiresAt": temporaryExpiry,
		}
		for key, value := range want {
			if got, present := viewer[key]; !present || got != value {
				t.Errorf("viewer.%s: expected %#v, got %#v (present=%v)", key, value, got, present)
			}
		}
	})

	t.Run("permanent ban serves banned, uri and reason with no expiry key", func(t *testing.T) {
		viewer := get(t, viewerDID, permanentDID)
		if viewer == nil {
			t.Fatalf("expected a viewer object for an authenticated caller")
		}
		want := map[string]any{
			"banned":    true,
			"banUri":    "at://did:plc:banviewpermanent/social.coves.moderation.ban/" + viewerBanRKey,
			"banReason": "spam",
		}
		for key, value := range want {
			if got, present := viewer[key]; !present || got != value {
				t.Errorf("viewer.%s: expected %#v, got %#v (present=%v)", key, value, got, present)
			}
		}
		if got, present := viewer["banExpiresAt"]; present {
			t.Errorf("viewer.banExpiresAt must be absent for a permanent ban, got %#v", got)
		}
	})

	for _, row := range []struct {
		name, callerDID, communityDID string
	}{
		{"lapsed ban serves no ban keys", viewerDID, lapsedDID},
		{"viewer with no ban in this community serves no ban keys", viewerDID, unrelatedDID},
		{"anonymous caller serves no ban keys", "", temporaryDID},
	} {
		t.Run(row.name, func(t *testing.T) {
			viewer := get(t, row.callerDID, row.communityDID)
			for _, key := range banKeys {
				if got, present := viewer[key]; present {
					t.Errorf("viewer.%s must be absent, got %#v", key, got)
				}
			}
		})
	}
}
