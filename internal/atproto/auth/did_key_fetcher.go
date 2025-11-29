package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
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
// Returns an *ecdsa.PublicKey suitable for use with jwt-go.
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

	// Convert to JWK format to extract coordinates
	jwk, err := pubKey.JWK()
	if err != nil {
		return nil, fmt.Errorf("failed to convert public key to JWK: %w", err)
	}

	// Convert atcrypto JWK to Go ecdsa.PublicKey
	return atcryptoJWKToECDSA(jwk)
}

// atcryptoJWKToECDSA converts an atcrypto.JWK to a Go ecdsa.PublicKey
func atcryptoJWKToECDSA(jwk *atcrypto.JWK) (*ecdsa.PublicKey, error) {
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
	case "secp256k1":
		// secp256k1 (K-256) is used by some atproto implementations
		// Go's standard library doesn't include secp256k1, but we can still
		// construct the key - jwt-go may not support it directly
		return nil, fmt.Errorf("secp256k1 curve requires special handling for JWT verification")
	default:
		return nil, fmt.Errorf("unsupported JWK curve: %s", jwk.Curve)
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
