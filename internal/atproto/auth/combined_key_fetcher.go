package auth

import (
	"context"
	"fmt"
	"strings"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
)

// CombinedKeyFetcher handles JWT public key fetching for both:
// - DID issuers (did:plc:, did:web:) → resolves via DID document
// - URL issuers (https://) → fetches via JWKS endpoint (legacy/fallback)
//
// For atproto service authentication, the issuer is typically the user's DID,
// and the signing key is published in their DID document.
type CombinedKeyFetcher struct {
	didFetcher  *DIDKeyFetcher
	jwksFetcher JWKSFetcher
}

// NewCombinedKeyFetcher creates a key fetcher that supports both DID and URL issuers.
// Parameters:
//   - directory: Indigo's identity directory for DID resolution
//   - jwksFetcher: fallback JWKS fetcher for URL issuers (can be nil if not needed)
func NewCombinedKeyFetcher(directory indigoIdentity.Directory, jwksFetcher JWKSFetcher) *CombinedKeyFetcher {
	return &CombinedKeyFetcher{
		didFetcher:  NewDIDKeyFetcher(directory),
		jwksFetcher: jwksFetcher,
	}
}

// FetchPublicKey fetches the public key for verifying a JWT.
// Routes to the appropriate fetcher based on issuer format:
// - DID (did:plc:, did:web:) → DIDKeyFetcher
// - URL (https://) → JWKSFetcher
func (f *CombinedKeyFetcher) FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error) {
	// Check if issuer is a DID
	if strings.HasPrefix(issuer, "did:") {
		return f.didFetcher.FetchPublicKey(ctx, issuer, token)
	}

	// Check if issuer is a URL (https:// or http:// in dev)
	if strings.HasPrefix(issuer, "https://") || strings.HasPrefix(issuer, "http://") {
		if f.jwksFetcher == nil {
			return nil, fmt.Errorf("URL issuer %s requires JWKS fetcher, but none configured", issuer)
		}
		return f.jwksFetcher.FetchPublicKey(ctx, issuer, token)
	}

	return nil, fmt.Errorf("unsupported issuer format: %s (expected DID or URL)", issuer)
}
