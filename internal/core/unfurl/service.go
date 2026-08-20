package unfurl

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
)

// Service handles URL unfurling with caching
type Service interface {
	UnfurlURL(ctx context.Context, url string) (*UnfurlResult, error)
	IsSupported(url string) bool
}

type service struct {
	repo           Repository
	circuitBreaker *circuitBreaker
	httpClient     *http.Client
	userAgent      string
	timeout        time.Duration
	cacheTTL       time.Duration

	// allowPrivateHosts disables the SSRF guard on every unfurl fetch. NEVER set
	// in production: the URL unfurled here is a link a signed-up account pasted
	// into a post, and this is the only fetch site in the tree that hands the
	// RESPONSE BODY back to the caller.
	allowPrivateHosts bool
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

	// ONE CLIENT FOR ALL THREE FETCH PATHS. providers.go used to build an
	// unguarded &http.Client{} inside each of fetchOEmbed, fetchOpenGraph and
	// fetchKagiKite; they now take this one, so a path cannot be left behind by a
	// conversion that only reached the two that looked alike.
	//
	// The SSRF-safe transport of internal/atproto/oauth resolves the host,
	// refuses private, loopback and link-local addresses, and then dials only the
	// address it vetted — closing the check-then-dial window a naive guard leaves
	// open. It matters more here than at any other fetch site in the tree: every
	// other one discards the response body or treats it as opaque bytes, while
	// this one PARSES it and RETURNS ITS CONTENT — og:title, og:description and
	// og:image land in an UnfurlResult, in the unfurl cache, and then in a post
	// read by strangers. The URL is a link a signed-up account pasted, so the
	// primitive being closed is not "can the AppView reach this address" but
	// "tell me what it said".
	//
	// THE BYTE CAP IS THIS SITE'S OWN, not the transport's 32 MiB default:
	// adopting the default would triple what a remote host can make this process
	// allocate, and nothing in the suite would notice because a looser bound
	// fails no existing fixture. See maxUnfurlBodyBytes.
	//
	// ONE BYTE ABOVE THAT LIMIT, deliberately, the way imageproxy/fetcher.go and
	// posts' newGuardedRematerializeBlobClient are. Both HTML read paths probe
	// maxUnfurlBodyBytes+1 through an io.LimitReader so that an oversized page is
	// DETECTED rather than truncated; a transport cap set to exactly the limit
	// clips the probing byte, the LimitReader returns a full-but-short read with
	// no error, and an error-tolerant parser turns half a page into a
	// complete-looking UnfurlResult. See maxUnfurlBodyBytes and ErrPageTooLarge.
	//
	// THE TIMEOUT IS THE CONFIGURED ONE, restored over the shared client's own
	// 15s ceiling the way blobs.NewBlobService and imageproxy.NewPDSFetcher
	// restore theirs. This service defaults to 10s and cmd/server passes
	// WithTimeout(unfurlTimeout), its own 10s constant, so both paths land on the
	// same number; loosening every unfurl by five seconds — on the path a post
	// write blocks behind — would be a second change wearing an SSRF fix's
	// clothes.
	clientOpts := append(
		covesoauth.PrivateAddressOptions(s.allowPrivateHosts),
		covesoauth.WithMaxResponseBytes(maxUnfurlBodyBytes+1),
	)
	s.httpClient = covesoauth.NewSSRFSafeHTTPClient(clientOpts...)
	s.httpClient.Timeout = s.timeout

	return s
}

// ServiceOption configures the service
type ServiceOption func(*service)

// WithPrivateHostsAllowed disables the SSRF address guard on every unfurl fetch.
//
// THE NAME IS THE CONTRACT: production must not call this. cmd/server derives
// the value from config once (the IS_DEV_ENV gate); tests that serve their
// fixtures from httptest pass it because loopback is exactly what the guard
// refuses.
func WithPrivateHostsAllowed() ServiceOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(s *service) { s.allowPrivateHosts = true }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to NewService: the hatch when it is set, and NOTHING when
// it is not.
//
// It mirrors oauth.PrivateAddressOptions and imageproxy.PrivateHostOptions, and
// it is a function rather than an `if` in cmd/server/wiring.go for the reason
// documented there: `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the
// PERMISSIVE branch at every call site holding such a boolean. A unit test
// against this function is the only place in the repository where the branch
// production actually runs is ever evaluated. Do not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's
// own defaults.
func PrivateHostOptions(allowPrivate bool) []ServiceOption {
	if !allowPrivate {
		return nil
	}
	return []ServiceOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

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
		result, err = fetchKagiKite(ctx, urlStr, s.httpClient, s.userAgent)
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
		oembed, err := fetchOEmbed(ctx, urlStr, s.httpClient, s.userAgent)
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
		result, err = fetchOpenGraph(ctx, urlStr, s.httpClient, s.userAgent)
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
