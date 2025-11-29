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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtConfig holds cached JWT configuration to avoid reading env vars on every request
type jwtConfig struct {
	hs256Issuers map[string]struct{} // Set of whitelisted HS256 issuers
	pdsJWTSecret []byte              // Cached PDS_JWT_SECRET
	isDevEnv     bool                // Cached IS_DEV_ENV
}

var (
	cachedConfig *jwtConfig
	configOnce   sync.Once
)

// InitJWTConfig initializes the JWT configuration from environment variables.
// This should be called once at startup. If not called explicitly, it will be
// initialized lazily on first use.
func InitJWTConfig() {
	configOnce.Do(func() {
		cachedConfig = &jwtConfig{
			hs256Issuers: make(map[string]struct{}),
			isDevEnv:     os.Getenv("IS_DEV_ENV") == "true",
		}

		// Parse HS256_ISSUERS into a set for O(1) lookup
		if issuers := os.Getenv("HS256_ISSUERS"); issuers != "" {
			for _, issuer := range strings.Split(issuers, ",") {
				issuer = strings.TrimSpace(issuer)
				if issuer != "" {
					cachedConfig.hs256Issuers[issuer] = struct{}{}
				}
			}
		}

		// Cache PDS_JWT_SECRET
		if secret := os.Getenv("PDS_JWT_SECRET"); secret != "" {
			cachedConfig.pdsJWTSecret = []byte(secret)
		}
	})
}

// getConfig returns the cached config, initializing if needed
func getConfig() *jwtConfig {
	InitJWTConfig()
	return cachedConfig
}

// ResetJWTConfigForTesting resets the cached config to allow re-initialization.
// This should ONLY be used in tests.
func ResetJWTConfigForTesting() {
	cachedConfig = nil
	configOnce = sync.Once{}
}

// Algorithm constants for JWT signing methods
const (
	AlgorithmHS256 = "HS256"
	AlgorithmRS256 = "RS256"
	AlgorithmES256 = "ES256"
)

// JWTHeader represents the parsed JWT header
type JWTHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

// Claims represents the standard JWT claims we care about
type Claims struct {
	jwt.RegisteredClaims
	// Confirmation claim for DPoP token binding (RFC 9449)
	// Contains "jkt" (JWK thumbprint) when token is bound to a DPoP key
	Confirmation map[string]interface{} `json:"cnf,omitempty"`
	Scope        string                 `json:"scope,omitempty"`
}

// stripBearerPrefix removes the "Bearer " prefix from a token string
func stripBearerPrefix(tokenString string) string {
	tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	return strings.TrimSpace(tokenString)
}

// ParseJWTHeader extracts and parses the JWT header from a token string
// This is a reusable function for getting algorithm and key ID information
func ParseJWTHeader(tokenString string) (*JWTHeader, error) {
	tokenString = stripBearerPrefix(tokenString)

	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT header: %w", err)
	}

	var header JWTHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("failed to parse JWT header: %w", err)
	}

	return &header, nil
}

// shouldUseHS256 determines if a token should use HS256 verification
// This prevents algorithm confusion attacks by using multiple signals:
// 1. If the token has a `kid` (key ID), it MUST use asymmetric verification
// 2. If no `kid`, only allow HS256 from whitelisted issuers (your own PDS)
//
// This approach supports open federation because:
// - External PDSes publish keys via JWKS and include `kid` in their tokens
// - Only your own PDS (which shares PDS_JWT_SECRET) uses HS256 without `kid`
func shouldUseHS256(header *JWTHeader, issuer string) bool {
	// If token has a key ID, it MUST use asymmetric verification
	// This is the primary defense against algorithm confusion attacks
	if header.Kid != "" {
		return false
	}

	// No kid - check if issuer is whitelisted for HS256
	// This should only include your own PDS URL(s)
	return isHS256IssuerWhitelisted(issuer)
}

// isHS256IssuerWhitelisted checks if the issuer is in the HS256 whitelist
// Only your own PDS should be in this list - external PDSes should use JWKS
func isHS256IssuerWhitelisted(issuer string) bool {
	cfg := getConfig()
	_, whitelisted := cfg.hs256Issuers[issuer]
	return whitelisted
}

// ParseJWT parses a JWT token without verification (Phase 1)
// Returns the claims if the token is valid JSON and has required fields
func ParseJWT(tokenString string) (*Claims, error) {
	// Remove "Bearer " prefix if present
	tokenString = stripBearerPrefix(tokenString)

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
// For HS256 tokens from whitelisted issuers, uses the shared PDS_JWT_SECRET
//
// SECURITY: Algorithm is determined by the issuer whitelist, NOT the token header,
// to prevent algorithm confusion attacks where an attacker could re-sign a token
// with HS256 using a public key as the secret.
func VerifyJWT(ctx context.Context, tokenString string, keyFetcher JWKSFetcher) (*Claims, error) {
	// Strip Bearer prefix once at the start
	tokenString = stripBearerPrefix(tokenString)

	// First parse to get the issuer (needed to determine expected algorithm)
	claims, err := ParseJWT(tokenString)
	if err != nil {
		return nil, err
	}

	// Parse header to get the claimed algorithm (for validation)
	header, err := ParseJWTHeader(tokenString)
	if err != nil {
		return nil, err
	}

	// SECURITY: Determine verification method based on token characteristics
	// 1. Tokens with `kid` MUST use asymmetric verification (supports federation)
	// 2. Tokens without `kid` can use HS256 only from whitelisted issuers (your own PDS)
	useHS256 := shouldUseHS256(header, claims.Issuer)

	if useHS256 {
		// Verify token actually claims to use HS256
		if header.Alg != AlgorithmHS256 {
			return nil, fmt.Errorf("expected HS256 for issuer %s but token uses %s", claims.Issuer, header.Alg)
		}
		return verifyHS256Token(tokenString)
	}

	// Token must use asymmetric verification
	// Reject HS256 tokens that don't meet the criteria above
	if header.Alg == AlgorithmHS256 {
		if header.Kid != "" {
			return nil, fmt.Errorf("HS256 tokens with kid must use asymmetric verification")
		}
		return nil, fmt.Errorf("HS256 not allowed for issuer %s (not in HS256_ISSUERS whitelist)", claims.Issuer)
	}

	// For RSA/ECDSA, fetch public key from JWKS and verify
	return verifyAsymmetricToken(ctx, tokenString, claims.Issuer, keyFetcher)
}

// verifyHS256Token verifies a JWT using HMAC-SHA256 with the shared secret
func verifyHS256Token(tokenString string) (*Claims, error) {
	cfg := getConfig()
	if len(cfg.pdsJWTSecret) == 0 {
		return nil, fmt.Errorf("HS256 verification failed: PDS_JWT_SECRET not configured")
	}

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return cfg.pdsJWTSecret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("HS256 verification failed: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("HS256 verification failed: token signature invalid")
	}

	verifiedClaims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("HS256 verification failed: invalid claims type")
	}

	if err := validateClaims(verifiedClaims); err != nil {
		return nil, err
	}

	return verifiedClaims, nil
}

// verifyAsymmetricToken verifies a JWT using RSA or ECDSA with a public key from JWKS
func verifyAsymmetricToken(ctx context.Context, tokenString, issuer string, keyFetcher JWKSFetcher) (*Claims, error) {
	publicKey, err := keyFetcher.FetchPublicKey(ctx, issuer, tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch public key: %w", err)
	}

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
		return nil, fmt.Errorf("asymmetric verification failed: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("asymmetric verification failed: token signature invalid")
	}

	verifiedClaims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("asymmetric verification failed: invalid claims type")
	}

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
	// In dev mode (IS_DEV_ENV=true), allow HTTP for local PDS testing
	isHTTP := strings.HasPrefix(claims.Issuer, "http://")
	isHTTPS := strings.HasPrefix(claims.Issuer, "https://")
	isDID := strings.HasPrefix(claims.Issuer, "did:")

	if !isHTTPS && !isDID && !isHTTP {
		return fmt.Errorf("issuer must be HTTPS URL, HTTP URL (dev only), or DID, got: %s", claims.Issuer)
	}

	// In production, reject HTTP issuers (only for non-dev environments)
	cfg := getConfig()
	if isHTTP && !cfg.isDevEnv {
		return fmt.Errorf("HTTP issuer not allowed in production, got: %s", claims.Issuer)
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
	header, err := ParseJWTHeader(tokenString)
	if err != nil {
		return "", err
	}

	if header.Kid == "" {
		return "", fmt.Errorf("missing kid in token header")
	}

	return header.Kid, nil
}
