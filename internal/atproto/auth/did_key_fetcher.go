package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"

	indigoCrypto "github.com/bluesky-social/indigo/atproto/atcrypto"
	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// DIDKeyFetcher fetches public keys from DID documents for JWT verification.
// This is the primary method for atproto service authentication, where:
// - The JWT issuer is the user's DID (e.g., did:plc:abc123)
// - The signing key is published in the user's DID document
// - Verification happens by resolving the DID and checking the signature
type DIDKeyFetcher struct {
	directory indigoIdentity.Directory
}

// NewDIDKeyFetcher creates a new DID-based key fetcher.
func NewDIDKeyFetcher(directory indigoIdentity.Directory) *DIDKeyFetcher {
	return &DIDKeyFetcher{
		directory: directory,
	}
}

// FetchPublicKey fetches the public key for verifying a JWT from the issuer's DID document.
// For DID issuers (did:plc: or did:web:), resolves the DID and extracts the signing key.
//
// Returns:
//   - indigoCrypto.PublicKey for secp256k1 (ES256K) keys - use indigo for verification
//   - *ecdsa.PublicKey for NIST curves (P-256, P-384, P-521) - compatible with golang-jwt
func (f *DIDKeyFetcher) FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error) {
	// Only handle DID issuers
	if !strings.HasPrefix(issuer, "did:") {
		return nil, fmt.Errorf("DIDKeyFetcher only handles DID issuers, got: %s", issuer)
	}

	// Parse the DID
	did, err := syntax.ParseDID(issuer)
	if err != nil {
		return nil, fmt.Errorf("invalid DID format: %w", err)
	}

	// Resolve the DID to get the identity (includes public keys)
	ident, err := f.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve DID %s: %w", issuer, err)
	}

	// Get the atproto signing key from the DID document
	pubKey, err := ident.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get public key from DID document: %w", err)
	}

	// Convert to JWK format to check curve type
	jwk, err := pubKey.JWK()
	if err != nil {
		return nil, fmt.Errorf("failed to convert public key to JWK: %w", err)
	}

	// For secp256k1 (ES256K), return indigo's PublicKey directly
	// since Go's crypto/ecdsa doesn't support this curve
	if jwk.Curve == "secp256k1" {
		return pubKey, nil
	}

	// For NIST curves, convert to Go's ecdsa.PublicKey for golang-jwt compatibility
	return atcryptoJWKToECDSA(jwk)
}

// atcryptoJWKToECDSA converts an indigoCrypto.JWK to a Go ecdsa.PublicKey.
// Note: secp256k1 is handled separately in FetchPublicKey by returning indigo's PublicKey directly.
func atcryptoJWKToECDSA(jwk *indigoCrypto.JWK) (*ecdsa.PublicKey, error) {
	if jwk.KeyType != "EC" {
		return nil, fmt.Errorf("unsupported JWK key type: %s (expected EC)", jwk.KeyType)
	}

	// Decode X and Y coordinates (base64url, no padding)
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("invalid JWK X coordinate encoding: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("invalid JWK Y coordinate encoding: %w", err)
	}

	var ecCurve elliptic.Curve
	switch jwk.Curve {
	case "P-256":
		ecCurve = elliptic.P256()
	case "P-384":
		ecCurve = elliptic.P384()
	case "P-521":
		ecCurve = elliptic.P521()
	default:
		// secp256k1 should be handled before calling this function
		return nil, fmt.Errorf("unsupported JWK curve for Go ecdsa: %s (secp256k1 uses indigo)", jwk.Curve)
	}

	// Create the public key
	pubKey := &ecdsa.PublicKey{
		Curve: ecCurve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}

	// Validate point is on curve
	if !ecCurve.IsOnCurve(pubKey.X, pubKey.Y) {
		return nil, fmt.Errorf("invalid public key: point not on curve")
	}

	return pubKey, nil
}
