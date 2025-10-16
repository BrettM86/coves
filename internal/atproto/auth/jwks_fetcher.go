package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CachedJWKSFetcher fetches and caches JWKS from authorization servers
type CachedJWKSFetcher struct {
	cache      map[string]*cachedJWKS
	httpClient *http.Client
	cacheMutex sync.RWMutex
	cacheTTL   time.Duration
}

type cachedJWKS struct {
	jwks      *JWKS
	expiresAt time.Time
}

// NewCachedJWKSFetcher creates a new JWKS fetcher with caching
func NewCachedJWKSFetcher(cacheTTL time.Duration) *CachedJWKSFetcher {
	return &CachedJWKSFetcher{
		cache: make(map[string]*cachedJWKS),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cacheTTL: cacheTTL,
	}
}

// FetchPublicKey fetches the public key for verifying a JWT from the issuer
// Implements JWKSFetcher interface
// Returns interface{} to support both RSA and ECDSA keys
func (f *CachedJWKSFetcher) FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error) {
	// Extract key ID from token
	kid, err := ExtractKeyID(token)
	if err != nil {
		return nil, fmt.Errorf("failed to extract key ID: %w", err)
	}

	// Get JWKS from cache or fetch
	jwks, err := f.getJWKS(ctx, issuer)
	if err != nil {
		return nil, err
	}

	// Find the key by ID
	jwk, err := jwks.FindKeyByID(kid)
	if err != nil {
		// Key not found in cache - try refreshing
		jwks, err = f.fetchJWKS(ctx, issuer)
		if err != nil {
			return nil, fmt.Errorf("failed to refresh JWKS: %w", err)
		}
		f.cacheJWKS(issuer, jwks)

		// Try again with fresh JWKS
		jwk, err = jwks.FindKeyByID(kid)
		if err != nil {
			return nil, err
		}
	}

	// Convert JWK to public key (RSA or ECDSA)
	return jwk.ToPublicKey()
}

// getJWKS gets JWKS from cache or fetches if not cached/expired
func (f *CachedJWKSFetcher) getJWKS(ctx context.Context, issuer string) (*JWKS, error) {
	// Check cache first
	f.cacheMutex.RLock()
	cached, exists := f.cache[issuer]
	f.cacheMutex.RUnlock()

	if exists && time.Now().Before(cached.expiresAt) {
		return cached.jwks, nil
	}

	// Not in cache or expired - fetch from issuer
	jwks, err := f.fetchJWKS(ctx, issuer)
	if err != nil {
		return nil, err
	}

	// Cache it
	f.cacheJWKS(issuer, jwks)

	return jwks, nil
}

// fetchJWKS fetches JWKS from the authorization server
func (f *CachedJWKSFetcher) fetchJWKS(ctx context.Context, issuer string) (*JWKS, error) {
	// Step 1: Fetch OAuth server metadata to get JWKS URI
	metadataURL := strings.TrimSuffix(issuer, "/") + "/.well-known/oauth-authorization-server"

	req, err := http.NewRequestWithContext(ctx, "GET", metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata request: %w", err)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metadata: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata endpoint returned status %d", resp.StatusCode)
	}

	var metadata struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	if metadata.JWKSURI == "" {
		return nil, fmt.Errorf("jwks_uri not found in metadata")
	}

	// Step 2: Fetch JWKS from the JWKS URI
	jwksReq, err := http.NewRequestWithContext(ctx, "GET", metadata.JWKSURI, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWKS request: %w", err)
	}

	jwksResp, err := f.httpClient.Do(jwksReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer func() {
		_ = jwksResp.Body.Close()
	}()

	if jwksResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned status %d", jwksResp.StatusCode)
	}

	var jwks JWKS
	if err := json.NewDecoder(jwksResp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("failed to decode JWKS: %w", err)
	}

	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("no keys found in JWKS")
	}

	return &jwks, nil
}

// cacheJWKS stores JWKS in the cache
func (f *CachedJWKSFetcher) cacheJWKS(issuer string, jwks *JWKS) {
	f.cacheMutex.Lock()
	defer f.cacheMutex.Unlock()

	f.cache[issuer] = &cachedJWKS{
		jwks:      jwks,
		expiresAt: time.Now().Add(f.cacheTTL),
	}
}

// ClearCache clears the entire JWKS cache
func (f *CachedJWKSFetcher) ClearCache() {
	f.cacheMutex.Lock()
	defer f.cacheMutex.Unlock()
	f.cache = make(map[string]*cachedJWKS)
}

// CleanupExpiredCache removes expired entries from the cache
func (f *CachedJWKSFetcher) CleanupExpiredCache() {
	f.cacheMutex.Lock()
	defer f.cacheMutex.Unlock()

	now := time.Now()
	for issuer, cached := range f.cache {
		if now.After(cached.expiresAt) {
			delete(f.cache, issuer)
		}
	}
}
