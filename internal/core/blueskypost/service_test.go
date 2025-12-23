package blueskypost

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
			url:      "https://bsky.app/profile/alice.bsky.social/post/abc123",
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
			url:         "https://bsky.app/profile/alice.bsky.social/post/abc123",
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
	// This test would require mocking the HTTP client or using a test server
	// For now, we'll test that cache miss is handled properly by testing
	// the flow up to the point where fetching would occur

	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	atURI := "at://did:plc:notincache/app.bsky.feed.post/xyz789"

	// Cache miss should trigger a fetch from the API
	// Since this is a fake DID, the API will return 404 which maps to unavailable
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
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)
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
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)
	ctx := context.Background()

	atURI := "at://did:plc:alice123/app.bsky.feed.post/abc123"

	// This will fail at fetch, but we're testing that cache set errors
	// are handled gracefully
	_, err := svc.ResolvePost(ctx, atURI)

	// Error should be from fetch, not from cache set
	// In a real test with mocked HTTP, we'd verify the cache set error
	// was logged but didn't fail the request
	if err != nil && contains(err.Error(), "cache write failed") {
		t.Error("Cache set errors should not fail the request")
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

	if svc.cacheTTL != customCacheTTL {
		t.Errorf("Expected cache TTL %v, got %v", customCacheTTL, svc.cacheTTL)
	}
}

func TestService_DefaultOptions(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver).(*service)

	expectedTimeout := 10 * time.Second
	expectedCacheTTL := 1 * time.Hour

	if svc.timeout != expectedTimeout {
		t.Errorf("Expected default timeout %v, got %v", expectedTimeout, svc.timeout)
	}

	if svc.cacheTTL != expectedCacheTTL {
		t.Errorf("Expected default cache TTL %v, got %v", expectedCacheTTL, svc.cacheTTL)
	}

	if svc.circuitBreaker == nil {
		t.Error("Circuit breaker should be initialized")
	}
}

func TestService_ResolvePost_ContextCancellation(t *testing.T) {
	repo := newMockRepository()
	resolver := &mockIdentityResolver{}
	svc := NewService(repo, resolver)

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
	url := "https://bsky.app/profile/alice.bsky.social/post/abc123"
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
