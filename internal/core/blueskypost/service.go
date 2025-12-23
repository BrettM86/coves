package blueskypost

import (
	"Coves/internal/atproto/identity"
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Cache TTL constants for age-based decay
const (
	// TTL for posts less than 24 hours old (engagement still changing rapidly)
	ttlFreshPost = 15 * time.Minute
	// TTL for posts 1-7 days old (engagement settling down)
	ttlRecentPost = 1 * time.Hour
	// TTL for posts older than 7 days (stable, unlikely to change much)
	ttlOldPost = 24 * time.Hour
	// TTL for unavailable posts (shorter to allow re-checking)
	ttlUnavailable = 15 * time.Minute
)

// service implements the Service interface
type service struct {
	repo             Repository
	identityResolver identity.Resolver
	circuitBreaker   *circuitBreaker
	timeout          time.Duration
	maxCacheTTL      time.Duration // maximum TTL (used as fallback if age unknown)
}

// NewService creates a new Bluesky post service
func NewService(repo Repository, identityResolver identity.Resolver, opts ...ServiceOption) Service {
	if repo == nil {
		panic("blueskypost: repo cannot be nil")
	}
	if identityResolver == nil {
		panic("blueskypost: identityResolver cannot be nil")
	}

	s := &service{
		repo:             repo,
		identityResolver: identityResolver,
		timeout:          10 * time.Second,
		maxCacheTTL:      ttlOldPost, // max TTL for fallback; actual TTL is age-based
		circuitBreaker:   newCircuitBreaker(),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// ServiceOption configures the service
type ServiceOption func(*service)

// WithTimeout sets the HTTP timeout for Bluesky API requests
func WithTimeout(timeout time.Duration) ServiceOption {
	return func(s *service) {
		s.timeout = timeout
	}
}

// WithCacheTTL sets the maximum cache TTL (used as fallback when post age is unknown)
func WithCacheTTL(ttl time.Duration) ServiceOption {
	return func(s *service) {
		s.maxCacheTTL = ttl
	}
}

// CalculateCacheTTL determines the appropriate cache TTL based on post age.
// Newer posts get shorter TTLs since their engagement counts change rapidly.
// Older posts get longer TTLs since they're more stable.
// Unavailable posts get short TTLs to allow re-checking.
func CalculateCacheTTL(result *BlueskyPostResult, maxTTL time.Duration) time.Duration {
	// Unavailable posts: short TTL to allow re-checking if they come back
	if result == nil || result.Unavailable {
		return ttlUnavailable
	}

	// If we don't have a creation time, use max TTL as fallback
	if result.CreatedAt.IsZero() {
		return maxTTL
	}

	postAge := time.Since(result.CreatedAt)

	switch {
	case postAge < 24*time.Hour:
		// Fresh posts: 15 min TTL (engagement changing rapidly)
		return ttlFreshPost
	case postAge < 7*24*time.Hour:
		// Recent posts (1-7 days): 1 hour TTL
		return ttlRecentPost
	default:
		// Old posts (7+ days): 24 hour TTL
		return ttlOldPost
	}
}

// IsBlueskyURL checks if a URL is a valid bsky.app post URL.
// Returns true for URLs matching https://bsky.app/profile/{handle}/post/{rkey}
func (s *service) IsBlueskyURL(url string) bool {
	return IsBlueskyURL(url)
}

// ParseBlueskyURL converts a bsky.app URL to an AT-URI.
// Example: https://bsky.app/profile/user.bsky.social/post/abc123
//
//	-> at://did:plc:xxx/app.bsky.feed.post/abc123
//
// Returns error if the URL is invalid or handle resolution fails.
func (s *service) ParseBlueskyURL(ctx context.Context, url string) (string, error) {
	return ParseBlueskyURL(ctx, url, s.identityResolver)
}

// ResolvePost fetches and resolves a Bluesky post by AT-URI.
// It checks the cache first, then fetches from public.api.bsky.app if needed.
// Returns BlueskyPostResult with Unavailable=true if the post cannot be resolved.
func (s *service) ResolvePost(ctx context.Context, atURI string) (*BlueskyPostResult, error) {
	// 1. Check cache first
	cached, err := s.repo.Get(ctx, atURI)
	if err != nil && !errors.Is(err, ErrCacheMiss) {
		// Log cache errors (but not cache misses) at WARNING level for operator visibility
		log.Printf("[BLUESKY] Warning: Cache read error for %s: %v", atURI, err)
	} else if err == nil && cached != nil {
		log.Printf("[BLUESKY] Cache hit for %s", atURI)
		return cached, nil
	}

	// 2. Check circuit breaker
	provider := "bluesky"
	canAttempt, err := s.circuitBreaker.canAttempt(provider)
	if !canAttempt {
		log.Printf("[BLUESKY] Skipping %s due to circuit breaker: %v", atURI, err)
		return nil, err
	}

	// 3. Fetch from Bluesky API
	log.Printf("[BLUESKY] Cache miss for %s, fetching from API...", atURI)
	result, err := fetchBlueskyPost(ctx, atURI, s.timeout)
	if err != nil {
		s.circuitBreaker.recordFailure(provider, err)
		return nil, fmt.Errorf("failed to fetch Bluesky post: %w", err)
	}

	s.circuitBreaker.recordSuccess(provider)

	// 4. Cache the result with age-based TTL
	// Newer posts get shorter TTLs (engagement changing), older posts get longer TTLs (stable)
	cacheTTL := CalculateCacheTTL(result, s.maxCacheTTL)
	if cacheErr := s.repo.Set(ctx, atURI, result, cacheTTL); cacheErr != nil {
		// Log but don't fail - cache is best-effort
		log.Printf("[BLUESKY] Warning: Failed to cache result for %s: %v", atURI, cacheErr)
	}

	log.Printf("[BLUESKY] Successfully resolved %s (unavailable: %v, cacheTTL: %v)", atURI, result.Unavailable, cacheTTL)

	return result, nil
}
