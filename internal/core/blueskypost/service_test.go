package blueskypost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// alicePostURL is the one bsky.app permalink these tests parse. IsBlueskyURL
// and ParseBlueskyURL are regex-and-split over the string — no request is made
// to derive the AT-URI — and every test here that does issue a request goes
// through newStubbedService, which points the fetcher at an httptest server.
const alicePostURL = "https://bsky.app/profile/alice.bsky.social/post/abc123" // coves:allow-public-host: parser input; the only host this file dials is the local stub server.

// mockRepository implements Repository for testing
type mockRepository struct {
	storage map[string]*BlueskyPostResult
	getErr  error
	setErr  error
}

func newMockRepository() *mockRepository {
	return &mockRepository{
		storage: make(map[string]*BlueskyPostResult),
	}
}

func (m *mockRepository) Get(ctx context.Context, atURI string) (*BlueskyPostResult, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	result, ok := m.storage[atURI]
	if !ok {
		return nil, ErrCacheMiss
	}
	return result, nil
}

func (m *mockRepository) Set(ctx context.Context, atURI string, result *BlueskyPostResult, ttl time.Duration) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.storage[atURI] = result
	return nil
}

// newStubbedService returns a service whose Bluesky API calls go to an
// httptest server instead of public.api.bsky.app. Without it these tests hit
// the live Bluesky API — which made them fail the moment CI blocked egress,
// and meant they were asserting Bluesky's uptime rather than our caching and
// circuit-breaker behaviour.
func newStubbedService(t *testing.T, repo Repository, handler http.HandlerFunc) *service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	svc := NewService(repo, &mockIdentityResolver{}).(*service)
	svc.api = blueskyAPI{baseURL: server.URL, allowPrivateHost: true}
	return svc
}

// respondPostNotFound is what the Bluesky API returns for a post that does not
// exist: a 404, which the fetcher maps to an "unavailable" result rather than
// an error.
func respondPostNotFound(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func TestService_IsBlueskyURL(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)

	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "valid bsky.app URL",
			url:      alicePostURL,
			expected: true,
		},
		{
			name:     "invalid URL",
			url:      "https://twitter.com/alice/status/123",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.IsBlueskyURL(tt.url)
			if result != tt.expected {
				t.Errorf("IsBlueskyURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

func TestService_ParseBlueskyURL(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{
		handleToDID: map[string]string{
			"alice.bsky.social": "did:plc:alice123",
		},
	}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	tests := []struct {
		name        string
		url         string
		expectedURI string
		wantErr     bool
	}{
		{
			name:        "valid URL",
			url:         alicePostURL,
			expectedURI: "at://did:plc:alice123/app.bsky.feed.post/abc123",
			wantErr:     false,
		},
		{
			name:    "invalid URL",
			url:     "https://twitter.com/alice/status/123",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := svc.ParseBlueskyURL(ctx, tt.url)

			if tt.wantErr {
				if err == nil {
					t.Error("ParseBlueskyURL() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("ParseBlueskyURL() unexpected error: %v", err)
				return
			}

			if result != tt.expectedURI {
				t.Errorf("ParseBlueskyURL() = %q, want %q", result, tt.expectedURI)
			}
		})
	}
}

func TestService_ResolvePost_CacheHit(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	expectedResult := &BlueskyPostResult{
		URI:  atURI,
		CID:  "cid123",
		Text: "Hello from cache",
		Author: &Author{
			DID:    "did:plc:alice123",
			Handle: "alice.bsky.social",
		},
	}

	// Pre-populate cache
	err := repo.Set(ctx, atURI, expectedResult, 1*time.Hour)
	if err != nil {
		t.Fatalf("Failed to set up cache: %v", err)
	}

	// Resolve should return cached result
	result, err := svc.ResolvePost(ctx, atURI)
	if err != nil {
		t.Fatalf("ResolvePost() unexpected error: %v", err)
	}

	if result.URI != expectedResult.URI {
		t.Errorf("Expected URI %q, got %q", expectedResult.URI, result.URI)
	}
	if result.Text != expectedResult.Text {
		t.Errorf("Expected text %q, got %q", expectedResult.Text, result.Text)
	}
}

func TestService_ResolvePost_CacheMiss(t *testing.T) {
	repo := newMockRepository()
	svc := newStubbedService(t, repo, respondPostNotFound)
	ctx := context.Background()

	atURI := "at://did:plc:notincache/app.bsky.feed.post/xyz789"

	// Cache miss should trigger a fetch from the API, whose 404 for an unknown
	// post maps to unavailable rather than to an error.
	result, err := svc.ResolvePost(ctx, atURI)
	// The request should succeed (404 is not an error, it's unavailable)
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
	if result == nil {
		t.Fatal("Expected result, got nil")
	}
	if !result.Unavailable {
		t.Error("Expected result to be unavailable for fake DID")
	}
}

func TestService_ResolvePost_CacheError(t *testing.T) {
	repo := newMockRepository()
	repo.getErr = errors.New("database connection failed")
	svc := newStubbedService(t, repo, respondPostNotFound)
	ctx := context.Background()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"

	// Cache error should be logged but not fail the request
	// It should proceed to fetch from the API
	result, err := svc.ResolvePost(ctx, atURI)
	// The request should succeed (cache errors are logged but not fatal)
	// The fetch will likely return an unavailable result for this fake DID
	if err != nil {
		t.Errorf("Expected no error despite cache failure, got: %v", err)
	}
	if result == nil {
		t.Error("Expected result despite cache failure, got nil")
	}
}

func TestService_ResolvePost_CircuitBreakerOpen(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver).(*service)
	ctx := context.Background()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	provider := "bluesky"

	// Manually open the circuit breaker
	testErr := errors.New("test error")
	for i := 0; i < svc.circuitBreaker.failureThreshold; i++ {
		svc.circuitBreaker.recordFailure(provider, testErr)
	}

	// Attempt to resolve should be blocked by circuit breaker
	_, err := svc.ResolvePost(ctx, atURI)
	if err == nil {
		t.Error("ResolvePost() should fail when circuit breaker is open")
	}

	if !contains(err.Error(), "circuit breaker open") {
		t.Errorf("Expected circuit breaker error, got: %v", err)
	}
}

func TestService_ResolvePost_SetCacheError(t *testing.T) {
	// Test that cache set errors don't fail the request
	repo := newMockRepository()
	repo.setErr = errors.New("cache write failed")
	svc := newStubbedService(t, repo, respondPostNotFound)
	ctx := context.Background()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"

	result, err := svc.ResolvePost(ctx, atURI)

	// The cache write fails, and the request still succeeds: caching is
	// best-effort, so its failure is logged rather than surfaced.
	if err != nil {
		t.Errorf("Cache set errors should not fail the request, got: %v", err)
	}
	if result == nil {
		t.Fatal("Expected a result despite the cache write failing")
	}
}

func TestService_WithOptions(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}

	customTimeout := 30 * time.Second
	customCacheTTL := 2 * time.Hour

	svc := NewService(
		repo,
		resolver,
		WithTimeout(customTimeout),
		WithCacheTTL(customCacheTTL),
	).(*service)

	if svc.timeout != customTimeout {
		t.Errorf("Expected timeout %v, got %v", customTimeout, svc.timeout)
	}

	if svc.maxCacheTTL != customCacheTTL {
		t.Errorf("Expected max cache TTL %v, got %v", customCacheTTL, svc.maxCacheTTL)
	}
}

func TestService_DefaultOptions(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver).(*service)

	expectedTimeout := 10 * time.Second
	expectedMaxCacheTTL := 24 * time.Hour // maxCacheTTL defaults to ttlOldPost (fallback for unknown age)

	if svc.timeout != expectedTimeout {
		t.Errorf("Expected default timeout %v, got %v", expectedTimeout, svc.timeout)
	}

	if svc.maxCacheTTL != expectedMaxCacheTTL {
		t.Errorf("Expected default max cache TTL %v, got %v", expectedMaxCacheTTL, svc.maxCacheTTL)
	}

	if svc.circuitBreaker == nil {
		t.Error("Circuit breaker should be initialized")
	}
}

// TestService_DefaultAPITarget pins the production default. The api field
// exists so tests can redirect the fetcher at a loopback server with the SSRF
// guard off; if a future option or refactor let either of those leak into the
// default, the service would be willing to fetch posts from an
// attacker-nominated host. That is worth a test of its own rather than a
// comment.
func TestService_DefaultAPITarget(t *testing.T) {
	svc := NewService(newMockRepository(), &mockIdentityResolver{}).(*service)

	if svc.api.baseURL != blueskyAPIBaseURL {
		t.Errorf("api.baseURL = %q, want the public Bluesky AppView %q", svc.api.baseURL, blueskyAPIBaseURL)
	}
	if svc.api.allowPrivateHost {
		t.Error("api.allowPrivateHost must be false by default: the SSRF guard is not optional in production")
	}
}

// TestService_ResolvePost_ParsesAPIResponse covers the happy path end to end —
// a 200 from the Bluesky API through blueskyAPIResponse decoding and into the
// cache. Every other stubbed test here answers 404, so without this one the
// response parsing has no merge-path coverage at all: it was previously only
// exercised by the live-tier tests.
func TestService_ResolvePost_ParsesAPIResponse(t *testing.T) {
	// The avatar is spliced in rather than written inline because it is the one
	// public hostname in the fixture, and a JSON line cannot carry a Go comment
	// saying so.
	const goldenAvatar = "https://cdn.bsky.app/img/avatar/alice.jpg" // coves:allow-public-host: a field in a canned API response body; decoded by the stub handler's client, never fetched.
	goldenResponse := fmt.Sprintf(`{
	  "posts": [
	    {
	      "uri": "at://did:plc:alice123/app.bsky.feed.post/abc123",
	      "cid": "bafyreigoldencid",
	      "author": {
	        "did": "did:plc:alice123",
	        "handle": "alice.bsky.social",
	        "displayName": "Alice",
	        "avatar": %q
	      },
	      "record": {
	        "text": "hello from the golden fixture",
	        "createdAt": "2026-07-01T12:00:00Z"
	      },
	      "replyCount": 3,
	      "repostCount": 5,
	      "likeCount": 7,
	      "indexedAt": "2026-07-01T12:00:05Z"
	    }
	  ]
	}`, goldenAvatar)

	repo := newMockRepository()
	var requestedURI string
	svc := newStubbedService(t, repo, func(w http.ResponseWriter, r *http.Request) {
		requestedURI = r.URL.Query().Get("uris")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(goldenResponse))
	})

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	result, err := svc.ResolvePost(context.Background(), atURI)
	if err != nil {
		t.Fatalf("ResolvePost() unexpected error: %v", err)
	}

	if requestedURI != atURI {
		t.Errorf("fetcher requested uris=%q, want %q", requestedURI, atURI)
	}
	if result.Unavailable {
		t.Error("A 200 with a post must not be marked unavailable")
	}
	if result.URI != atURI {
		t.Errorf("URI = %q, want %q", result.URI, atURI)
	}
	if result.CID != "bafyreigoldencid" {
		t.Errorf("CID = %q, want bafyreigoldencid", result.CID)
	}
	if result.Text != "hello from the golden fixture" {
		t.Errorf("Text = %q, want the record text", result.Text)
	}
	if result.Author == nil {
		t.Fatal("Author must be populated from the response")
	}
	if result.Author.Handle != "alice.bsky.social" {
		t.Errorf("Author.Handle = %q, want alice.bsky.social", result.Author.Handle)
	}
	if result.LikeCount != 7 || result.RepostCount != 5 || result.ReplyCount != 3 {
		t.Errorf("engagement counts = like %d/repost %d/reply %d, want 7/5/3",
			result.LikeCount, result.RepostCount, result.ReplyCount)
	}
	if result.CreatedAt.IsZero() {
		t.Error("CreatedAt should be parsed from record.createdAt")
	}

	// A successful fetch is cached, which is what makes the second read a hit.
	cached, cacheErr := repo.Get(context.Background(), atURI)
	if cacheErr != nil {
		t.Fatalf("result should have been cached, got: %v", cacheErr)
	}
	if cached.CID != "bafyreigoldencid" {
		t.Errorf("cached CID = %q, want bafyreigoldencid", cached.CID)
	}
}

func TestService_ResolvePost_ContextCancellation(t *testing.T) {
	repo := newMockRepository()
	svc := newStubbedService(t, repo, respondPostNotFound)

	// Create a context that's already cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"

	_, err := svc.ResolvePost(ctx, atURI)
	if err == nil {
		t.Error("ResolvePost() should fail with cancelled context")
	}

	if !errors.Is(err, context.Canceled) && !contains(err.Error(), "context canceled") {
		t.Errorf("Expected context cancelled error, got: %v", err)
	}
}

func TestService_ResolvePost_MultipleProviders(t *testing.T) {
	// Test that circuit breaker tracks providers independently
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver).(*service)

	// The blueskypost service only uses one provider ("bluesky")
	// but we can verify the circuit breaker works independently
	// by testing with different URIs

	ctx := context.Background()
	atURI1 := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	atURI2 := "at://did:plc:bob456/app.bsky.feed.post/xyz789"

	// Both should use the same circuit breaker
	// Open the circuit
	provider := "bluesky"
	testErr := errors.New("test error")
	for i := 0; i < svc.circuitBreaker.failureThreshold; i++ {
		svc.circuitBreaker.recordFailure(provider, testErr)
	}

	// Both URIs should be blocked
	_, err1 := svc.ResolvePost(ctx, atURI1)
	if err1 == nil || !contains(err1.Error(), "circuit breaker open") {
		t.Error("First URI should be blocked by circuit breaker")
	}

	_, err2 := svc.ResolvePost(ctx, atURI2)
	if err2 == nil || !contains(err2.Error(), "circuit breaker open") {
		t.Error("Second URI should be blocked by circuit breaker")
	}
}

func TestService_ResolvePost_CacheBypass(t *testing.T) {
	// Test that even if cache returns a result, it's the correct one
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	atURI1 := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	atURI2 := "at://did:plc:bob456/app.bsky.feed.post/xyz789"

	result1 := &BlueskyPostResult{
		URI:  atURI1,
		Text: "Post 1",
	}
	result2 := &BlueskyPostResult{
		URI:  atURI2,
		Text: "Post 2",
	}

	// Cache both
	_ = repo.Set(ctx, atURI1, result1, 1*time.Hour)
	_ = repo.Set(ctx, atURI2, result2, 1*time.Hour)

	// Retrieve each and verify correct result
	got1, err := svc.ResolvePost(ctx, atURI1)
	if err != nil {
		t.Fatalf("ResolvePost(%q) error: %v", atURI1, err)
	}
	if got1.Text != "Post 1" {
		t.Errorf("Expected Post 1, got %q", got1.Text)
	}

	got2, err := svc.ResolvePost(ctx, atURI2)
	if err != nil {
		t.Fatalf("ResolvePost(%q) error: %v", atURI2, err)
	}
	if got2.Text != "Post 2" {
		t.Errorf("Expected Post 2, got %q", got2.Text)
	}
}

func TestCalculateCacheTTL(t *testing.T) {
	maxTTL := 24 * time.Hour

	tests := []struct {
		name     string
		result   *BlueskyPostResult
		expected time.Duration
	}{
		{
			name:     "nil result returns unavailable TTL",
			result:   nil,
			expected: 15 * time.Minute,
		},
		{
			name: "unavailable post returns unavailable TTL",
			result: &BlueskyPostResult{
				URI:         "at://did:plc:test/app.bsky.feed.post/abc",
				Unavailable: true,
			},
			expected: 15 * time.Minute,
		},
		{
			name: "zero CreatedAt returns maxTTL",
			result: &BlueskyPostResult{
				URI:  "at://did:plc:test/app.bsky.feed.post/abc",
				Text: "Test post",
				// CreatedAt is zero value
			},
			expected: maxTTL,
		},
		{
			name: "fresh post (< 24h) returns 15 min TTL",
			result: &BlueskyPostResult{
				URI:       "at://did:plc:test/app.bsky.feed.post/abc",
				Text:      "Fresh post",
				CreatedAt: time.Now().Add(-1 * time.Hour), // 1 hour ago
			},
			expected: 15 * time.Minute,
		},
		{
			name: "recent post (1-7 days) returns 1 hour TTL",
			result: &BlueskyPostResult{
				URI:       "at://did:plc:test/app.bsky.feed.post/abc",
				Text:      "Recent post",
				CreatedAt: time.Now().Add(-3 * 24 * time.Hour), // 3 days ago
			},
			expected: 1 * time.Hour,
		},
		{
			name: "old post (7+ days) returns 24 hour TTL",
			result: &BlueskyPostResult{
				URI:       "at://did:plc:test/app.bsky.feed.post/abc",
				Text:      "Old post",
				CreatedAt: time.Now().Add(-14 * 24 * time.Hour), // 14 days ago
			},
			expected: 24 * time.Hour,
		},
		{
			name: "post at exactly 24h boundary uses recent TTL",
			result: &BlueskyPostResult{
				URI:       "at://did:plc:test/app.bsky.feed.post/abc",
				Text:      "Boundary post",
				CreatedAt: time.Now().Add(-24*time.Hour - 1*time.Minute), // just over 24h
			},
			expected: 1 * time.Hour,
		},
		{
			name: "post at exactly 7 day boundary uses old TTL",
			result: &BlueskyPostResult{
				URI:       "at://did:plc:test/app.bsky.feed.post/abc",
				Text:      "7 day boundary post",
				CreatedAt: time.Now().Add(-7*24*time.Hour - 1*time.Minute), // just over 7 days
			},
			expected: 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCacheTTL(tt.result, maxTTL)
			if got != tt.expected {
				t.Errorf("CalculateCacheTTL() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestService_IntegrationFlow(t *testing.T) {
	// Integration test simulating the full flow
	repo := newMockRepository()
	resolver := &mockIdentityResolver{
		handleToDID: map[string]string{
			"alice.bsky.social": "did:plc:alice123",
		},
	}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	// Step 1: Check URL
	url := alicePostURL
	if !svc.IsBlueskyURL(url) {
		t.Fatalf("IsBlueskyURL(%q) should return true", url)
	}

	// Step 2: Parse URL
	atURI, err := svc.ParseBlueskyURL(ctx, url)
	if err != nil {
		t.Fatalf("ParseBlueskyURL() error: %v", err)
	}

	expectedURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"
	if atURI != expectedURI {
		t.Errorf("Expected URI %q, got %q", expectedURI, atURI)
	}

	// Step 3: Pre-populate cache with result
	cachedResult := &BlueskyPostResult{
		URI:  atURI,
		Text: "Integration test post",
		Author: &Author{
			DID:    "did:plc:alice123",
			Handle: "alice.bsky.social",
		},
	}
	_ = repo.Set(ctx, atURI, cachedResult, 1*time.Hour)

	// Step 4: Resolve post (should hit cache)
	result, err := svc.ResolvePost(ctx, atURI)
	if err != nil {
		t.Fatalf("ResolvePost() error: %v", err)
	}

	if result.Text != "Integration test post" {
		t.Errorf("Expected cached text, got %q", result.Text)
	}
}
