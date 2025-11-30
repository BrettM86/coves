package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	indigoCrypto "github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/golang-jwt/jwt/v5"
)

// NonceCache provides replay protection for DPoP proofs by tracking seen jti values.
// This prevents an attacker from reusing a captured DPoP proof within the validity window.
// Per RFC 9449 Section 11.1, servers SHOULD prevent replay attacks.
type NonceCache struct {
	seen    map[string]time.Time // jti -> expiration time
	stopCh  chan struct{}
	maxAge  time.Duration // How long to keep entries
	cleanup time.Duration // How often to clean up expired entries
	mu      sync.RWMutex
}

// NewNonceCache creates a new nonce cache for DPoP replay protection.
// maxAge should match or exceed DPoPVerifier.MaxProofAge.
func NewNonceCache(maxAge time.Duration) *NonceCache {
	nc := &NonceCache{
		seen:    make(map[string]time.Time),
		maxAge:  maxAge,
		cleanup: maxAge / 2, // Clean up at half the max age
		stopCh:  make(chan struct{}),
	}

	// Start background cleanup goroutine
	go nc.cleanupLoop()

	return nc
}

// CheckAndStore checks if a jti has been seen before and stores it if not.
// Returns true if the jti is fresh (not a replay), false if it's a replay.
func (nc *NonceCache) CheckAndStore(jti string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()

	now := time.Now()
	expiry := now.Add(nc.maxAge)

	// Check if already seen
	if existingExpiry, seen := nc.seen[jti]; seen {
		// Still valid (not expired) - this is a replay
		if existingExpiry.After(now) {
			return false
		}
		// Expired entry - allow reuse and update expiry
	}

	// Store the new jti
	nc.seen[jti] = expiry
	return true
}

// cleanupLoop periodically removes expired entries from the cache
func (nc *NonceCache) cleanupLoop() {
	ticker := time.NewTicker(nc.cleanup)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			nc.cleanupExpired()
		case <-nc.stopCh:
			return
		}
	}
}

// cleanupExpired removes expired entries from the cache
func (nc *NonceCache) cleanupExpired() {
	nc.mu.Lock()
	defer nc.mu.Unlock()

	now := time.Now()
	for jti, expiry := range nc.seen {
		if expiry.Before(now) {
			delete(nc.seen, jti)
		}
	}
}

// Stop stops the cleanup goroutine. Call this when done with the cache.
func (nc *NonceCache) Stop() {
	close(nc.stopCh)
}

// Size returns the number of entries in the cache (for testing/monitoring)
func (nc *NonceCache) Size() int {
	nc.mu.RLock()
	defer nc.mu.RUnlock()
	return len(nc.seen)
}

// DPoPClaims represents the claims in a DPoP proof JWT (RFC 9449)
type DPoPClaims struct {
	jwt.RegisteredClaims

	// HTTP method of the request (e.g., "GET", "POST")
	HTTPMethod string `json:"htm"`

	// HTTP URI of the request (without query and fragment parts)
	HTTPURI string `json:"htu"`

	// Access token hash (optional, for token binding)
	AccessTokenHash string `json:"ath,omitempty"`
}

// DPoPProof represents a parsed and verified DPoP proof
type DPoPProof struct {
	RawPublicJWK map[string]interface{}
	Claims       *DPoPClaims
	PublicKey    interface{} // *ecdsa.PublicKey or similar
	Thumbprint   string      // JWK thumbprint (base64url)
}

// DPoPVerifier verifies DPoP proofs for OAuth token binding
type DPoPVerifier struct {
	// Optional: custom nonce validation function (for server-issued nonces)
	ValidateNonce func(nonce string) bool

	// NonceCache for replay protection (optional but recommended)
	// If nil, jti replay protection is disabled
	NonceCache *NonceCache

	// Maximum allowed clock skew for timestamp validation
	MaxClockSkew time.Duration

	// Maximum age of DPoP proof (prevents replay with old proofs)
	MaxProofAge time.Duration
}

// NewDPoPVerifier creates a DPoP verifier with sensible defaults including replay protection
func NewDPoPVerifier() *DPoPVerifier {
	maxProofAge := 5 * time.Minute
	return &DPoPVerifier{
		MaxClockSkew: 30 * time.Second,
		MaxProofAge:  maxProofAge,
		NonceCache:   NewNonceCache(maxProofAge),
	}
}

// NewDPoPVerifierWithoutReplayProtection creates a DPoP verifier without replay protection.
// This should only be used in testing or when replay protection is handled externally.
func NewDPoPVerifierWithoutReplayProtection() *DPoPVerifier {
	return &DPoPVerifier{
		MaxClockSkew: 30 * time.Second,
		MaxProofAge:  5 * time.Minute,
		NonceCache:   nil, // No replay protection
	}
}

// Stop stops background goroutines. Call this when shutting down.
func (v *DPoPVerifier) Stop() {
	if v.NonceCache != nil {
		v.NonceCache.Stop()
	}
}

// VerifyDPoPProof verifies a DPoP proof JWT and returns the parsed proof
func (v *DPoPVerifier) VerifyDPoPProof(dpopProof, httpMethod, httpURI string) (*DPoPProof, error) {
	// Parse the DPoP JWT without verification first to extract the header
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(dpopProof, &DPoPClaims{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse DPoP proof: %w", err)
	}

	// Extract and validate the header
	header, ok := token.Header["typ"].(string)
	if !ok || header != "dpop+jwt" {
		return nil, fmt.Errorf("invalid DPoP proof: typ must be 'dpop+jwt', got '%s'", header)
	}

	alg, ok := token.Header["alg"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: missing alg header")
	}

	// Extract the JWK from the header
	jwkRaw, ok := token.Header["jwk"]
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: missing jwk header")
	}

	jwkMap, ok := jwkRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: jwk must be an object")
	}

	// Parse the public key from JWK
	publicKey, err := parseJWKToPublicKey(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("invalid DPoP proof JWK: %w", err)
	}

	// Calculate the JWK thumbprint
	thumbprint, err := CalculateJWKThumbprint(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate JWK thumbprint: %w", err)
	}

	// Now verify the signature
	verifiedToken, err := jwt.ParseWithClaims(dpopProof, &DPoPClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Verify the signing method matches what we expect
		switch alg {
		case "ES256":
			if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
		case "ES384", "ES512":
			if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
		case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
			// RSA methods - we primarily support ES256 for atproto
			return nil, fmt.Errorf("RSA algorithms not yet supported for DPoP: %s", alg)
		default:
			return nil, fmt.Errorf("unsupported DPoP algorithm: %s", alg)
		}
		return publicKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("DPoP proof signature verification failed: %w", err)
	}

	claims, ok := verifiedToken.Claims.(*DPoPClaims)
	if !ok {
		return nil, fmt.Errorf("invalid DPoP claims type")
	}

	// Validate the claims
	if err := v.validateDPoPClaims(claims, httpMethod, httpURI); err != nil {
		return nil, err
	}

	return &DPoPProof{
		Claims:       claims,
		PublicKey:    publicKey,
		Thumbprint:   thumbprint,
		RawPublicJWK: jwkMap,
	}, nil
}

// validateDPoPClaims validates the DPoP proof claims
func (v *DPoPVerifier) validateDPoPClaims(claims *DPoPClaims, expectedMethod, expectedURI string) error {
	// Validate jti (unique identifier) is present
	if claims.ID == "" {
		return fmt.Errorf("DPoP proof missing jti claim")
	}

	// Validate htm (HTTP method)
	if !strings.EqualFold(claims.HTTPMethod, expectedMethod) {
		return fmt.Errorf("DPoP proof htm mismatch: expected %s, got %s", expectedMethod, claims.HTTPMethod)
	}

	// Validate htu (HTTP URI) - compare without query/fragment
	expectedURIBase := stripQueryFragment(expectedURI)
	claimURIBase := stripQueryFragment(claims.HTTPURI)
	if expectedURIBase != claimURIBase {
		return fmt.Errorf("DPoP proof htu mismatch: expected %s, got %s", expectedURIBase, claimURIBase)
	}

	// Validate iat (issued at) is present and recent
	if claims.IssuedAt == nil {
		return fmt.Errorf("DPoP proof missing iat claim")
	}

	now := time.Now()
	iat := claims.IssuedAt.Time

	// Check clock skew (not too far in the future)
	if iat.After(now.Add(v.MaxClockSkew)) {
		return fmt.Errorf("DPoP proof iat is in the future")
	}

	// Check proof age (not too old)
	if now.Sub(iat) > v.MaxProofAge {
		return fmt.Errorf("DPoP proof is too old (issued %v ago, max %v)", now.Sub(iat), v.MaxProofAge)
	}

	// SECURITY: Check for replay attack using jti
	// Per RFC 9449 Section 11.1, servers SHOULD prevent replay attacks
	if v.NonceCache != nil {
		if !v.NonceCache.CheckAndStore(claims.ID) {
			return fmt.Errorf("DPoP proof replay detected: jti %s already used", claims.ID)
		}
	}

	return nil
}

// VerifyTokenBinding verifies that the DPoP proof binds to the access token
// by comparing the proof's thumbprint to the token's cnf.jkt claim
func (v *DPoPVerifier) VerifyTokenBinding(proof *DPoPProof, expectedThumbprint string) error {
	if proof.Thumbprint != expectedThumbprint {
		return fmt.Errorf("DPoP proof thumbprint mismatch: token expects %s, proof has %s",
			expectedThumbprint, proof.Thumbprint)
	}
	return nil
}

// VerifyAccessTokenHash verifies the DPoP proof's ath (access token hash) claim
// matches the SHA-256 hash of the presented access token.
// Per RFC 9449 section 4.2, if ath is present, the RS MUST verify it.
func (v *DPoPVerifier) VerifyAccessTokenHash(proof *DPoPProof, accessToken string) error {
	// If ath claim is not present, that's acceptable per RFC 9449
	// (ath is only required when the RS mandates it)
	if proof.Claims.AccessTokenHash == "" {
		return nil
	}

	// Calculate the expected ath: base64url(SHA-256(access_token))
	hash := sha256.Sum256([]byte(accessToken))
	expectedAth := base64.RawURLEncoding.EncodeToString(hash[:])

	if proof.Claims.AccessTokenHash != expectedAth {
		return fmt.Errorf("DPoP proof ath mismatch: proof bound to different access token")
	}

	return nil
}

// CalculateJWKThumbprint calculates the JWK thumbprint per RFC 7638
// The thumbprint is the base64url-encoded SHA-256 hash of the canonical JWK representation
func CalculateJWKThumbprint(jwk map[string]interface{}) (string, error) {
	kty, ok := jwk["kty"].(string)
	if !ok {
		return "", fmt.Errorf("JWK missing kty")
	}

	// Build the canonical JWK representation based on key type
	// Per RFC 7638, only specific members are included, in lexicographic order
	var canonical map[string]string

	switch kty {
	case "EC":
		crv, ok := jwk["crv"].(string)
		if !ok {
			return "", fmt.Errorf("EC JWK missing crv")
		}
		x, ok := jwk["x"].(string)
		if !ok {
			return "", fmt.Errorf("EC JWK missing x")
		}
		y, ok := jwk["y"].(string)
		if !ok {
			return "", fmt.Errorf("EC JWK missing y")
		}
		// Lexicographic order: crv, kty, x, y
		canonical = map[string]string{
			"crv": crv,
			"kty": kty,
			"x":   x,
			"y":   y,
		}
	case "RSA":
		e, ok := jwk["e"].(string)
		if !ok {
			return "", fmt.Errorf("RSA JWK missing e")
		}
		n, ok := jwk["n"].(string)
		if !ok {
			return "", fmt.Errorf("RSA JWK missing n")
		}
		// Lexicographic order: e, kty, n
		canonical = map[string]string{
			"e":   e,
			"kty": kty,
			"n":   n,
		}
	case "OKP":
		crv, ok := jwk["crv"].(string)
		if !ok {
			return "", fmt.Errorf("OKP JWK missing crv")
		}
		x, ok := jwk["x"].(string)
		if !ok {
			return "", fmt.Errorf("OKP JWK missing x")
		}
		// Lexicographic order: crv, kty, x
		canonical = map[string]string{
			"crv": crv,
			"kty": kty,
			"x":   x,
		}
	default:
		return "", fmt.Errorf("unsupported JWK key type: %s", kty)
	}

	// Serialize to JSON (Go's json.Marshal produces lexicographically ordered keys for map[string]string)
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("failed to serialize canonical JWK: %w", err)
	}

	// SHA-256 hash
	hash := sha256.Sum256(canonicalJSON)

	// Base64url encode (no padding)
	thumbprint := base64.RawURLEncoding.EncodeToString(hash[:])

	return thumbprint, nil
}

// parseJWKToPublicKey parses a JWK map to a Go public key
func parseJWKToPublicKey(jwkMap map[string]interface{}) (interface{}, error) {
	// Convert map to JSON bytes for indigo's parser
	jwkBytes, err := json.Marshal(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize JWK: %w", err)
	}

	// Try to parse with indigo's crypto package
	pubKey, err := indigoCrypto.ParsePublicJWKBytes(jwkBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWK: %w", err)
	}

	// Convert indigo's PublicKey to Go's ecdsa.PublicKey
	jwk, err := pubKey.JWK()
	if err != nil {
		return nil, fmt.Errorf("failed to get JWK from public key: %w", err)
	}

	// Use our existing conversion function
	return atcryptoJWKToECDSAFromIndigoJWK(jwk)
}

// atcryptoJWKToECDSAFromIndigoJWK converts an indigo JWK to Go ecdsa.PublicKey
func atcryptoJWKToECDSAFromIndigoJWK(jwk *indigoCrypto.JWK) (*ecdsa.PublicKey, error) {
	if jwk.KeyType != "EC" {
		return nil, fmt.Errorf("unsupported JWK key type: %s (expected EC)", jwk.KeyType)
	}

	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("invalid JWK X coordinate: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("invalid JWK Y coordinate: %w", err)
	}

	var curve ecdsa.PublicKey
	switch jwk.Curve {
	case "P-256":
		curve.Curve = ecdsaP256Curve()
	case "P-384":
		curve.Curve = ecdsaP384Curve()
	case "P-521":
		curve.Curve = ecdsaP521Curve()
	default:
		return nil, fmt.Errorf("unsupported curve: %s", jwk.Curve)
	}

	curve.X = new(big.Int).SetBytes(xBytes)
	curve.Y = new(big.Int).SetBytes(yBytes)

	return &curve, nil
}

// Helper functions for elliptic curves
func ecdsaP256Curve() elliptic.Curve { return elliptic.P256() }
func ecdsaP384Curve() elliptic.Curve { return elliptic.P384() }
func ecdsaP521Curve() elliptic.Curve { return elliptic.P521() }

// stripQueryFragment removes query and fragment from a URI
func stripQueryFragment(uri string) string {
	if idx := strings.Index(uri, "?"); idx != -1 {
		uri = uri[:idx]
	}
	if idx := strings.Index(uri, "#"); idx != -1 {
		uri = uri[:idx]
	}
	return uri
}

// ExtractCnfJkt extracts the cnf.jkt (confirmation key thumbprint) from JWT claims
func ExtractCnfJkt(claims *Claims) (string, error) {
	if claims.Confirmation == nil {
		return "", fmt.Errorf("token missing cnf claim (no DPoP binding)")
	}

	jkt, ok := claims.Confirmation["jkt"].(string)
	if !ok || jkt == "" {
		return "", fmt.Errorf("token cnf claim missing jkt (DPoP key thumbprint)")
	}

	return jkt, nil
}
