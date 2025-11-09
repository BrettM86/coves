package unfurl

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Service handles URL unfurling with caching
type Service interface {
	UnfurlURL(ctx context.Context, url string) (*UnfurlResult, error)
	IsSupported(url string) bool
}

type service struct {
	repo           Repository
	circuitBreaker *circuitBreaker
	userAgent      string
	timeout        time.Duration
	cacheTTL       time.Duration
}

// NewService creates a new unfurl service
func NewService(repo Repository, opts ...ServiceOption) Service {
	s := &service{
		repo:           repo,
		timeout:        10 * time.Second,
		userAgent:      "CovesBot/1.0 (+https://coves.social)",
		cacheTTL:       24 * time.Hour,
		circuitBreaker: newCircuitBreaker(),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// ServiceOption configures the service
type ServiceOption func(*service)

// WithTimeout sets the HTTP timeout for oEmbed requests
func WithTimeout(timeout time.Duration) ServiceOption {
	return func(s *service) {
		s.timeout = timeout
	}
}

// WithUserAgent sets the User-Agent header for oEmbed requests
func WithUserAgent(userAgent string) ServiceOption {
	return func(s *service) {
		s.userAgent = userAgent
	}
}

// WithCacheTTL sets the cache TTL
func WithCacheTTL(ttl time.Duration) ServiceOption {
	return func(s *service) {
		s.cacheTTL = ttl
	}
}

// IsSupported returns true if we can unfurl this URL
func (s *service) IsSupported(url string) bool {
	return isSupported(url)
}

// UnfurlURL fetches metadata for a URL (with caching)
func (s *service) UnfurlURL(ctx context.Context, urlStr string) (*UnfurlResult, error) {
	// 1. Check cache first
	cached, err := s.repo.Get(ctx, urlStr)
	if err == nil && cached != nil {
		log.Printf("[UNFURL] Cache hit for %s (provider: %s)", urlStr, cached.Provider)
		return cached, nil
	}

	// 2. Check if we support this URL
	if !isSupported(urlStr) {
		return nil, fmt.Errorf("unsupported URL: %s", urlStr)
	}

	var result *UnfurlResult
	domain := extractDomain(urlStr)

	// 3. Smart routing: Special handling for Kagi Kite (client-side rendered, no og:image tags)
	if domain == "kite.kagi.com" {
		provider := "kagi"

		// Check circuit breaker
		canAttempt, err := s.circuitBreaker.canAttempt(provider)
		if !canAttempt {
			log.Printf("[UNFURL] Skipping %s due to circuit breaker: %v", urlStr, err)
			return nil, err
		}

		log.Printf("[UNFURL] Cache miss for %s, fetching via Kagi parser...", urlStr)
		result, err = fetchKagiKite(ctx, urlStr, s.timeout, s.userAgent)
		if err != nil {
			s.circuitBreaker.recordFailure(provider, err)
			return nil, err
		}

		s.circuitBreaker.recordSuccess(provider)

		// Cache result
		if cacheErr := s.repo.Set(ctx, urlStr, result, s.cacheTTL); cacheErr != nil {
			log.Printf("[UNFURL] Warning: failed to cache result: %v", cacheErr)
		}
		return result, nil
	}

	// 4. Check if this is a known oEmbed provider
	if isOEmbedProvider(urlStr) {
		provider := domain // Use domain as provider name (e.g., "streamable.com", "youtube.com")

		// Check circuit breaker
		canAttempt, err := s.circuitBreaker.canAttempt(provider)
		if !canAttempt {
			log.Printf("[UNFURL] Skipping %s due to circuit breaker: %v", urlStr, err)
			return nil, err
		}

		log.Printf("[UNFURL] Cache miss for %s, fetching from oEmbed...", urlStr)

		// Fetch from oEmbed provider
		oembed, err := fetchOEmbed(ctx, urlStr, s.timeout, s.userAgent)
		if err != nil {
			s.circuitBreaker.recordFailure(provider, err)
			return nil, fmt.Errorf("failed to fetch oEmbed data: %w", err)
		}

		s.circuitBreaker.recordSuccess(provider)

		// Convert to UnfurlResult
		result = mapOEmbedToResult(oembed, urlStr)
	} else {
		provider := "opengraph"

		// Check circuit breaker
		canAttempt, err := s.circuitBreaker.canAttempt(provider)
		if !canAttempt {
			log.Printf("[UNFURL] Skipping %s due to circuit breaker: %v", urlStr, err)
			return nil, err
		}

		log.Printf("[UNFURL] Cache miss for %s, fetching via OpenGraph...", urlStr)

		// Fetch via OpenGraph
		result, err = fetchOpenGraph(ctx, urlStr, s.timeout, s.userAgent)
		if err != nil {
			s.circuitBreaker.recordFailure(provider, err)
			return nil, fmt.Errorf("failed to fetch OpenGraph data: %w", err)
		}

		s.circuitBreaker.recordSuccess(provider)
	}

	// 5. Store in cache
	if cacheErr := s.repo.Set(ctx, urlStr, result, s.cacheTTL); cacheErr != nil {
		// Log but don't fail - cache is best-effort
		log.Printf("[UNFURL] Warning: Failed to cache result for %s: %v", urlStr, cacheErr)
	}

	log.Printf("[UNFURL] Successfully unfurled %s (provider: %s, type: %s)",
		urlStr, result.Provider, result.Type)

	return result, nil
}
