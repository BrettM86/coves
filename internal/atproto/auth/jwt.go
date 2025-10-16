package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the standard JWT claims we care about
type Claims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope,omitempty"`
}

// ParseJWT parses a JWT token without verification (Phase 1)
// Returns the claims if the token is valid JSON and has required fields
func ParseJWT(tokenString string) (*Claims, error) {
	// Remove "Bearer " prefix if present
	tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	tokenString = strings.TrimSpace(tokenString)

	// Parse without verification first to extract claims
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(tokenString, &Claims{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("invalid claims type")
	}

	// Validate required fields
	if claims.Subject == "" {
		return nil, fmt.Errorf("missing 'sub' claim (user DID)")
	}

	// atProto PDSes may use 'aud' instead of 'iss' for the authorization server
	// If 'iss' is missing, use 'aud' as the authorization server identifier
	if claims.Issuer == "" {
		if len(claims.Audience) > 0 {
			claims.Issuer = claims.Audience[0]
		} else {
			return nil, fmt.Errorf("missing both 'iss' and 'aud' claims (authorization server)")
		}
	}

	// Validate claims (even in Phase 1, we need basic validation like expiry)
	if err := validateClaims(claims); err != nil {
		return nil, err
	}

	return claims, nil
}

// VerifyJWT verifies a JWT token's signature and claims (Phase 2)
// Fetches the public key from the issuer's JWKS endpoint and validates the signature
func VerifyJWT(ctx context.Context, tokenString string, keyFetcher JWKSFetcher) (*Claims, error) {
	// First parse to get the issuer
	claims, err := ParseJWT(tokenString)
	if err != nil {
		return nil, err
	}

	// Fetch the public key from the issuer
	publicKey, err := keyFetcher.FetchPublicKey(ctx, claims.Issuer, tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch public key: %w", err)
	}

	// Now parse and verify with the public key
	tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method - support both RSA and ECDSA (atProto uses ES256 primarily)
		switch token.Method.(type) {
		case *jwt.SigningMethodRSA, *jwt.SigningMethodECDSA:
			// Valid signing methods for atProto
		default:
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return publicKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to verify JWT: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("token is invalid")
	}

	verifiedClaims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("invalid claims type after verification")
	}

	// Additional validation
	if err := validateClaims(verifiedClaims); err != nil {
		return nil, err
	}

	return verifiedClaims, nil
}

// validateClaims performs additional validation on JWT claims
func validateClaims(claims *Claims) error {
	now := time.Now()

	// Check expiration
	if claims.ExpiresAt != nil && claims.ExpiresAt.Before(now) {
		return fmt.Errorf("token has expired")
	}

	// Check not before
	if claims.NotBefore != nil && claims.NotBefore.After(now) {
		return fmt.Errorf("token not yet valid")
	}

	// Validate DID format in sub claim
	if !strings.HasPrefix(claims.Subject, "did:") {
		return fmt.Errorf("invalid DID format in 'sub' claim: %s", claims.Subject)
	}

	// Validate issuer is either an HTTPS URL or a DID
	// atProto uses DIDs (did:web:, did:plc:) or HTTPS URLs as issuer identifiers
	if !strings.HasPrefix(claims.Issuer, "https://") && !strings.HasPrefix(claims.Issuer, "did:") {
		return fmt.Errorf("issuer must be HTTPS URL or DID, got: %s", claims.Issuer)
	}

	// Parse to ensure it's a valid URL
	if _, err := url.Parse(claims.Issuer); err != nil {
		return fmt.Errorf("invalid issuer URL: %w", err)
	}

	// Validate scope if present (lenient: allow empty, but reject wrong scopes)
	if claims.Scope != "" && !strings.Contains(claims.Scope, "atproto") {
		return fmt.Errorf("token missing required 'atproto' scope, got: %s", claims.Scope)
	}

	return nil
}

// JWKSFetcher defines the interface for fetching public keys from JWKS endpoints
// Returns interface{} to support both RSA and ECDSA keys
type JWKSFetcher interface {
	FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error)
}

// JWK represents a JSON Web Key from a JWKS endpoint
// Supports both RSA and EC (ECDSA) keys
type JWK struct {
	Kid string `json:"kid"` // Key ID
	Kty string `json:"kty"` // Key type ("RSA" or "EC")
	Alg string `json:"alg"` // Algorithm (e.g., "RS256", "ES256")
	Use string `json:"use"` // Public key use (should be "sig" for signatures)
	// RSA fields
	N string `json:"n,omitempty"` // RSA modulus
	E string `json:"e,omitempty"` // RSA exponent
	// EC fields
	Crv string `json:"crv,omitempty"` // EC curve (e.g., "P-256")
	X   string `json:"x,omitempty"`   // EC x coordinate
	Y   string `json:"y,omitempty"`   // EC y coordinate
}

// ToPublicKey converts a JWK to a public key (RSA or ECDSA)
func (j *JWK) ToPublicKey() (interface{}, error) {
	switch j.Kty {
	case "RSA":
		return j.toRSAPublicKey()
	case "EC":
		return j.toECPublicKey()
	default:
		return nil, fmt.Errorf("unsupported key type: %s", j.Kty)
	}
}

// toRSAPublicKey converts a JWK to an RSA public key
func (j *JWK) toRSAPublicKey() (*rsa.PublicKey, error) {
	// Decode modulus
	nBytes, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil {
		return nil, fmt.Errorf("failed to decode RSA modulus: %w", err)
	}

	// Decode exponent
	eBytes, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil {
		return nil, fmt.Errorf("failed to decode RSA exponent: %w", err)
	}

	// Convert exponent to int
	var eInt int
	for _, b := range eBytes {
		eInt = eInt*256 + int(b)
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: eInt,
	}, nil
}

// toECPublicKey converts a JWK to an ECDSA public key
func (j *JWK) toECPublicKey() (*ecdsa.PublicKey, error) {
	// Determine curve
	var curve elliptic.Curve
	switch j.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve: %s", j.Crv)
	}

	// Decode X coordinate
	xBytes, err := base64.RawURLEncoding.DecodeString(j.X)
	if err != nil {
		return nil, fmt.Errorf("failed to decode EC x coordinate: %w", err)
	}

	// Decode Y coordinate
	yBytes, err := base64.RawURLEncoding.DecodeString(j.Y)
	if err != nil {
		return nil, fmt.Errorf("failed to decode EC y coordinate: %w", err)
	}

	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}

// JWKS represents a JSON Web Key Set
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// FindKeyByID finds a key in the JWKS by its key ID
func (j *JWKS) FindKeyByID(kid string) (*JWK, error) {
	for _, key := range j.Keys {
		if key.Kid == kid {
			return &key, nil
		}
	}
	return nil, fmt.Errorf("key with kid %s not found", kid)
}

// ExtractKeyID extracts the key ID from a JWT token header
func ExtractKeyID(tokenString string) (string, error) {
	tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}

	// Decode header
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("failed to decode header: %w", err)
	}

	var header struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return "", fmt.Errorf("failed to unmarshal header: %w", err)
	}

	if header.Kid == "" {
		return "", fmt.Errorf("missing kid in token header")
	}

	return header.Kid, nil
}
