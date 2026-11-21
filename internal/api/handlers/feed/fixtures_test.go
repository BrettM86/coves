package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
)

// No infrastructure: the real feedgen service, the real HMAC cursor signer,
// and in-test generator/hydrator/resolver fakes. T0.

const (
	testInstanceDID = "did:web:instance.test"
	hotFeedURI      = "at://did:web:instance.test/social.coves.feed.generator/hot"
	otherFeedURI    = "at://did:web:instance.test/social.coves.feed.generator/other"
	newFeedURI      = "at://did:web:instance.test/social.coves.feed.generator/new"
	topFeedURI      = "at://did:web:instance.test/social.coves.feed.generator/top"
	unknownFeedURI  = "at://did:web:instance.test/social.coves.feed.generator/nope"

	communityOneDID = "did:plc:communityone"
	communityTwoDID = "did:plc:communitytwo"
	viewerOneDID    = "did:plc:viewerone"
	viewerTwoDID    = "did:plc:viewertwo"

	// communityOneHandle and communityOneScopedIdentifier resolve to
	// communityOneDID.
	communityOneHandle           = "one.coves.social"
	communityOneScopedIdentifier = "!one@coves.social"

	homeCommunityDID = "did:plc:homecommunity"
)

// fakeGenerator offers a fixed list of post URIs. It honors req.Limit, returns
// pageCursor as its inner cursor, and records every request it received.
type fakeGenerator struct {
	mu         sync.Mutex
	uris       []string
	pageCursor string
	requests   []feedgen.SkeletonRequest
}

func (g *fakeGenerator) Skeleton(ctx context.Context, req feedgen.SkeletonRequest) (feedgen.SkeletonPage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, req)
	items := make([]feedgen.SkeletonItem, 0, len(g.uris))
	for _, uri := range g.uris {
		if len(items) == req.Limit {
			break
		}
		items = append(items, feedgen.SkeletonItem{URI: uri})
	}
	return feedgen.SkeletonPage{Items: items, Cursor: g.pageCursor}, nil
}

func (g *fakeGenerator) received() []feedgen.SkeletonRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]feedgen.SkeletonRequest(nil), g.requests...)
}

// discoverHotItemURI is the one post the fixture's discover-hot fake serves.
const discoverHotItemURI = "at://did:plc:author/social.coves.community.post/discover1"

// fakeHydratedGenerator serves one pre-hydrated discover-hot page, or err when
// set, and records every request it received.
type fakeHydratedGenerator struct {
	mu       sync.Mutex
	err      error
	requests []feedgen.SkeletonRequest
}

func (g *fakeHydratedGenerator) HydratedPage(ctx context.Context, req feedgen.SkeletonRequest) (feedgen.HydratedPostPage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, req)
	if g.err != nil {
		return feedgen.HydratedPostPage{}, g.err
	}
	return feedgen.HydratedPostPage{Feed: []*feedgen.FeedViewPost{{Post: &posts.PostView{
		URI:       discoverHotItemURI,
		Author:    &posts.AuthorView{DID: "did:plc:author"},
		Community: &posts.CommunityRef{DID: homeCommunityDID, Handle: "c.test", Name: "c"},
	}}}}, nil
}

func (g *fakeHydratedGenerator) received() []feedgen.SkeletonRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]feedgen.SkeletonRequest(nil), g.requests...)
}

// fakeHydrator makes every URI visible, in input order, in the requested
// community (scope community) or in homeCommunityDID (scope home).
type fakeHydrator struct{}

func (fakeHydrator) HydratePosts(ctx context.Context, req feedgen.HydrateRequest) ([]*posts.PostView, error) {
	communityDID := homeCommunityDID
	if req.Scope.Name == feedgen.ScopeCommunity {
		communityDID = req.Scope.CommunityDID
	}
	views := make([]*posts.PostView, 0, len(req.URIs))
	for _, uri := range req.URIs {
		views = append(views, &posts.PostView{
			URI:       uri,
			Author:    &posts.AuthorView{DID: "did:plc:author"},
			Community: &posts.CommunityRef{DID: communityDID, Handle: "c.test", Name: "c"},
		})
	}
	return views, nil
}

// ambiguousCommunityIdentifier is a name@origin the fake resolver reports as
// matching more than one community.
const ambiguousCommunityIdentifier = "shared@two.example"

// malformedCommunityIdentifier is an identifier the fake resolver rejects as
// not well formed.
const malformedCommunityIdentifier = "not a community identifier"

// unresolvableCommunityIdentifier is an identifier the fake resolver fails to
// answer for (a datastore outage), with resolverFailureDetail in the error.
const (
	unresolvableCommunityIdentifier = "outage.coves.social"
	resolverFailureDetail           = "community lookup connection pool exhausted"
)

// fakeResolver knows a fixed set of community DIDs, plus communityOneHandle and
// communityOneScopedIdentifier, which resolve to communityOneDID; anything else
// is not found.
type fakeResolver struct{ known map[string]bool }

func (r fakeResolver) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	if r.known[identifier] {
		return identifier, nil
	}
	if identifier == communityOneHandle || identifier == communityOneScopedIdentifier {
		return communityOneDID, nil
	}
	if identifier == ambiguousCommunityIdentifier {
		return "", fmt.Errorf("resolve %q: %w", identifier, communities.ErrAmbiguousCommunity)
	}
	if identifier == malformedCommunityIdentifier {
		return "", communities.NewValidationError("community", "not a handle, DID or scoped identifier")
	}
	if identifier == unresolvableCommunityIdentifier {
		return "", fmt.Errorf("resolve %q: %s", identifier, resolverFailureDetail)
	}
	return "", fmt.Errorf("resolve %q: %w", identifier, communities.ErrCommunityNotFound)
}

type feedFixture struct {
	hot         *fakeGenerator
	other       *fakeGenerator
	newFeed     *fakeGenerator
	top         *fakeGenerator
	discoverHot *fakeHydratedGenerator
	registry    *feedgen.Registry
	handler     *GetFeedHandler
}

// newFeedFixture builds the handler over the real service and signer.
// extraCommunities are additional community DIDs the resolver knows.
func newFeedFixture(t *testing.T, hot *fakeGenerator, authority moderation.Authority, extraCommunities ...string) *feedFixture {
	t.Helper()
	other := &fakeGenerator{uris: []string{"at://did:plc:author/social.coves.community.post/other1"}}
	newFeed := &fakeGenerator{uris: []string{"at://did:plc:author/social.coves.community.post/new1"}}
	top := &fakeGenerator{uris: []string{"at://did:plc:author/social.coves.community.post/top1"}}
	discoverHot := &fakeHydratedGenerator{}
	registry := feedgen.NewRegistry(testInstanceDID,
		feedgen.HotBuiltin(hot),
		feedgen.NewBuiltin(newFeed),
		feedgen.TopBuiltin(top),
		feedgen.DiscoverHotBuiltin(discoverHot),
		feedgen.Builtin{
			RKey:        "other",
			DisplayName: "Other",
			Description: "A second test feed.",
			Kind:        feedgen.KindPosts,
			Scopes:      []string{feedgen.ScopeHome, feedgen.ScopeCommunity},
			Generator:   other,
		},
	)
	known := map[string]bool{communityOneDID: true, communityTwoDID: true}
	for _, did := range extraCommunities {
		known[did] = true
	}
	service := feedgen.NewService(registry, fakeHydrator{}, postgres.NewCursorSigner("test-secret"))
	handler := NewGetFeedHandler(service, fakeResolver{known: known}, nil, nil)
	if authority != nil {
		handler = handler.WithViewerModeration(authority, testInstanceDID)
	}
	return &feedFixture{hot: hot, other: other, newFeed: newFeed, top: top, discoverHot: discoverHot, registry: registry, handler: handler}
}

// get issues GET getFeed with the given query, as viewerDID ("" = anonymous).
func (f *feedFixture) get(t *testing.T, query url.Values, viewerDID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getFeed?"+query.Encode(), nil)
	if viewerDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), viewerDID))
	}
	f.handler.HandleGetFeed(rec, req)
	return rec
}

type feedResponseBody struct {
	Feed []struct {
		Post struct {
			URI    string `json:"uri"`
			Viewer *struct {
				Moderation []string `json:"moderation"`
			} `json:"viewer"`
		} `json:"post"`
	} `json:"feed"`
	Cursor string `json:"cursor"`
}

// decodeFeed requires a 200 with a JSON feed body.
func decodeFeed(t *testing.T, rec *httptest.ResponseRecorder) feedResponseBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body feedResponseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not a getFeed JSON object: %v (body: %q)", err, rec.Body.String())
	}
	return body
}

// requireXRPCError requires the given status and XRPC error name.
func requireXRPCError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantError string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (body: %q)", err, rec.Body.String())
	}
	if body.Error != wantError {
		t.Fatalf("error = %q, want %q (body: %s)", body.Error, wantError, rec.Body.String())
	}
}

func values(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}
