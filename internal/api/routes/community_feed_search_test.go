package routes

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
)

// fakeCommunityFeedService serves SearchPosts; the embedded nil Service
// satisfies the rest of the interface and panics if reached.
type fakeCommunityFeedService struct {
	communityFeeds.Service
	searchCalls int
}

func (f *fakeCommunityFeedService) SearchPosts(context.Context, communityFeeds.SearchPostsRequest) (*communityFeeds.FeedResponse, error) {
	f.searchCalls++
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}, nil
}

// emptyFeedService answers every getFeed request with an empty page.
type emptyFeedService struct{}

func (emptyFeedService) GetFeed(context.Context, feedgen.GetFeedRequest) (*feedgen.GetFeedResponse, error) {
	return &feedgen.GetFeedResponse{Feed: []*feedgen.FeedViewPost{}}, nil
}

// communityFeedRouter serves searchPosts and, alongside it, getFeed (another
// feed read) on one router.
func communityFeedRouter(service communityFeeds.Service) http.Handler {
	router := chi.NewRouter()
	auth := middleware.NewOAuthAuthMiddleware(nil, nil)
	RegisterCommunityFeedRoutes(router, service, nil, nil, auth, moderation.NewAllowlistAuthority(nil), "did:plc:testinstance")
	RegisterFeedRoutes(router, emptyFeedService{}, feedgen.NewRegistry("did:plc:testinstance"), nil, nil, nil, auth, moderation.NewAllowlistAuthority(nil), "did:plc:testinstance")
	return router
}

func communityFeedRequest(t *testing.T, router http.Handler, path, clientIP string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if clientIP != "" {
		req.Header.Set("X-Real-IP", clientIP)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestCommunityFeedRoutes_SearchUsesExactFeedNSIDAndOptionalAuth(t *testing.T) {
	service := &fakeCommunityFeedService{}
	router := communityFeedRouter(service)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.searchPosts?q=bridge", nil)
	// OptionalAuth must treat an unusable credential as an anonymous request.
	req.Header.Set("Authorization", "not-a-bearer-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET social.coves.feed.searchPosts status = %d, want 200: post search is a public read and must be registered with OptionalAuth. Body: %s", rec.Code, rec.Body.String())
	}
	if service.searchCalls != 1 {
		t.Errorf("SearchPosts calls = %d, want 1: the lexicon NSID social.coves.feed.searchPosts must reach the search service", service.searchCalls)
	}

	wrongNSID := communityFeedRequest(t, router, "/xrpc/social.coves.communityFeed.searchPosts?q=bridge", "")
	if wrongNSID.Code != http.StatusNotFound {
		t.Errorf("GET rejected communityFeed.searchPosts NSID status = %d, want 404: search belongs to the feed namespace even when a community filter is used", wrongNSID.Code)
	}
	if service.searchCalls != 1 {
		t.Errorf("SearchPosts calls after rejected NSID = %d, want 1: only the exact social.coves.feed.searchPosts route may invoke search", service.searchCalls)
	}
}

func TestCommunityFeedRoutes_SearchRateLimitIsNamedPerRouteAndClient(t *testing.T) {
	service := &fakeCommunityFeedService{}
	router := communityFeedRouter(service)

	var rateLimitLogs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&rateLimitLogs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	const firstClient = "10.0.0.1"
	for requestNumber := 1; requestNumber <= 30; requestNumber++ {
		rec := communityFeedRequest(t, router, "/xrpc/social.coves.feed.searchPosts?q=needle", firstClient)
		if rec.Code != http.StatusOK {
			t.Fatalf("search request %d from %s status = %d, want 200: the most expensive read gets exactly 30 requests per minute for each client. Body: %s", requestNumber, firstClient, rec.Code, rec.Body.String())
		}
	}

	limited := communityFeedRequest(t, router, "/xrpc/social.coves.feed.searchPosts?q=needle", firstClient)
	if limited.Code != http.StatusTooManyRequests {
		t.Errorf("search request 31 from %s status = %d, want 429: the most expensive read must be capped at 30 requests per minute per client", firstClient, limited.Code)
	}
	if !strings.Contains(rateLimitLogs.String(), `"limiter":"postSearch"`) {
		t.Errorf("rate-limit log = %q, want limiter identity postSearch: operators must be able to attribute throttling of the most expensive read", rateLimitLogs.String())
	}
	if service.searchCalls != 30 {
		t.Errorf("SearchPosts calls after the rejected request = %d, want 30: the per-route cap must reject request 31 before it reaches the expensive search service", service.searchCalls)
	}

	const secondClient = "10.0.0.2"
	otherClient := communityFeedRequest(t, router, "/xrpc/social.coves.feed.searchPosts?q=needle", secondClient)
	if otherClient.Code != http.StatusOK {
		t.Errorf("first search from %s status = %d, want 200: the 30/minute search cap is per client, not shared across clients. Body: %s", secondClient, otherClient.Code, otherClient.Body.String())
	}

	otherFeed := communityFeedRequest(t, router, "/xrpc/social.coves.feed.getFeed?scope=all", firstClient)
	if otherFeed.Code == http.StatusTooManyRequests {
		t.Errorf("getFeed from search-limited client status = 429: the postSearch cap must be route-specific because only search is the most expensive read")
	}
	if otherFeed.Code != http.StatusOK {
		t.Errorf("getFeed from search-limited client status = %d, want 200: exhausting the per-route search budget must not throttle other reads. Body: %s", otherFeed.Code, otherFeed.Body.String())
	}
}
