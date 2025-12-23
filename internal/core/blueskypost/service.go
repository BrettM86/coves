package blueskypost

import (
	"Coves/internal/atproto/identity"
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// service implements the Service interface
type service struct {
	repo             Repository
	identityResolver identity.Resolver
	circuitBreaker   *circuitBreaker
	timeout          time.Duration
	cacheTTL         time.Duration
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
		cacheTTL:         1 * time.Hour, // 1 hour cache (shorter than unfurl since posts can be deleted)
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

// WithCacheTTL sets the cache TTL
func WithCacheTTL(ttl time.Duration) ServiceOption {
	return func(s *service) {
		s.cacheTTL = ttl
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

	// 4. Cache the result (even if unavailable, to prevent repeated fetches)
	if cacheErr := s.repo.Set(ctx, atURI, result, s.cacheTTL); cacheErr != nil {
		// Log but don't fail - cache is best-effort
		log.Printf("[BLUESKY] Warning: Failed to cache result for %s: %v", atURI, cacheErr)
	}

	log.Printf("[BLUESKY] Successfully resolved %s (unavailable: %v)", atURI, result.Unavailable)

	return result, nil
}
