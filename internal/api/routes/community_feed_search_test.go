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

	"github.com/go-chi/chi/v5"
)

type fakeCommunityFeedService struct {
	searchCalls int
	getCalls    int
}

func (f *fakeCommunityFeedService) GetCommunityFeed(context.Context, communityFeeds.GetCommunityFeedRequest) (*communityFeeds.FeedResponse, error) {
	f.getCalls++
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}, nil
}

func (f *fakeCommunityFeedService) SearchPosts(context.Context, communityFeeds.SearchPostsRequest) (*communityFeeds.FeedResponse, error) {
	f.searchCalls++
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}, nil
}

func communityFeedRouter(service communityFeeds.Service) http.Handler {
	router := chi.NewRouter()
	RegisterCommunityFeedRoutes(router, service, nil, nil, middleware.NewOAuthAuthMiddleware(nil, nil))
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

	communityFeed := communityFeedRequest(t, router, "/xrpc/social.coves.communityFeed.getCommunity?community=x", firstClient)
	if communityFeed.Code == http.StatusTooManyRequests {
		t.Errorf("getCommunity from search-limited client status = 429: the postSearch cap must be route-specific because only search is the most expensive read")
	}
	if communityFeed.Code != http.StatusOK {
		t.Errorf("getCommunity from search-limited client status = %d, want 200: exhausting the per-route search budget must not throttle other reads. Body: %s", communityFeed.Code, communityFeed.Body.String())
	}
}
