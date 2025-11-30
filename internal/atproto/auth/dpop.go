package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// VerifyDPoPProof verifies a DPoP proof JWT and returns the parsed proof.
// This supports all atProto-compatible ECDSA algorithms including ES256K (secp256k1).
func (v *DPoPVerifier) VerifyDPoPProof(dpopProof, httpMethod, httpURI string) (*DPoPProof, error) {
	// Manually parse the JWT to support ES256K (which golang-jwt doesn't recognize)
	header, claims, err := parseJWTHeaderAndClaims(dpopProof)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DPoP proof: %w", err)
	}

	// Extract and validate the typ header
	typ, ok := header["typ"].(string)
	if !ok || typ != "dpop+jwt" {
		return nil, fmt.Errorf("invalid DPoP proof: typ must be 'dpop+jwt', got '%s'", typ)
	}

	alg, ok := header["alg"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: missing alg header")
	}

	// Extract the JWK from the header first (needed for algorithm-curve validation)
	jwkRaw, ok := header["jwk"]
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: missing jwk header")
	}

	jwkMap, ok := jwkRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DPoP proof: jwk must be an object")
	}

	// Validate the algorithm is supported and matches the JWK curve
	// This is critical for security - prevents algorithm confusion attacks
	if err := validateAlgorithmCurveBinding(alg, jwkMap); err != nil {
		return nil, fmt.Errorf("invalid DPoP proof: %w", err)
	}

	// Parse the public key using indigo's crypto package
	// This supports all atProto curves including secp256k1 (ES256K)
	publicKey, err := parseJWKToIndigoPublicKey(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("invalid DPoP proof JWK: %w", err)
	}

	// Calculate the JWK thumbprint
	thumbprint, err := CalculateJWKThumbprint(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate JWK thumbprint: %w", err)
	}

	// Verify the signature using indigo's crypto package
	// This works for all ECDSA algorithms including ES256K
	if err := verifyJWTSignatureWithIndigo(dpopProof, publicKey); err != nil {
		return nil, fmt.Errorf("DPoP proof signature verification failed: %w", err)
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

	// SECURITY: Validate exp claim if present (RFC standard JWT validation)
	// While DPoP proofs typically use iat + MaxProofAge, if exp is included it must be honored
	if claims.ExpiresAt != nil {
		expWithSkew := claims.ExpiresAt.Time.Add(v.MaxClockSkew)
		if now.After(expWithSkew) {
			return fmt.Errorf("DPoP proof expired at %v", claims.ExpiresAt.Time)
		}
	}

	// SECURITY: Validate nbf claim if present (RFC standard JWT validation)
	if claims.NotBefore != nil {
		nbfWithSkew := claims.NotBefore.Time.Add(-v.MaxClockSkew)
		if now.Before(nbfWithSkew) {
			return fmt.Errorf("DPoP proof not valid before %v", claims.NotBefore.Time)
		}
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

// validateAlgorithmCurveBinding validates that the JWT algorithm matches the JWK curve.
// This is critical for security - an attacker could claim alg: "ES256K" but provide
// a P-256 key, potentially bypassing algorithm binding requirements.
func validateAlgorithmCurveBinding(alg string, jwkMap map[string]interface{}) error {
	kty, ok := jwkMap["kty"].(string)
	if !ok {
		return fmt.Errorf("JWK missing kty")
	}

	// ECDSA algorithms require EC key type
	switch alg {
	case "ES256K", "ES256", "ES384", "ES512":
		if kty != "EC" {
			return fmt.Errorf("algorithm %s requires EC key type, got %s", alg, kty)
		}
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		return fmt.Errorf("RSA algorithms not yet supported for DPoP: %s", alg)
	default:
		return fmt.Errorf("unsupported DPoP algorithm: %s", alg)
	}

	// Validate curve matches algorithm
	crv, ok := jwkMap["crv"].(string)
	if !ok {
		return fmt.Errorf("EC JWK missing crv")
	}

	var expectedCurve string
	switch alg {
	case "ES256K":
		expectedCurve = "secp256k1"
	case "ES256":
		expectedCurve = "P-256"
	case "ES384":
		expectedCurve = "P-384"
	case "ES512":
		expectedCurve = "P-521"
	}

	if crv != expectedCurve {
		return fmt.Errorf("algorithm %s requires curve %s, got %s", alg, expectedCurve, crv)
	}

	return nil
}

// parseJWKToIndigoPublicKey parses a JWK map to an indigo PublicKey.
// This returns indigo's PublicKey interface which supports all atProto curves
// including secp256k1 (ES256K), P-256 (ES256), P-384 (ES384), and P-521 (ES512).
func parseJWKToIndigoPublicKey(jwkMap map[string]interface{}) (indigoCrypto.PublicKey, error) {
	// Convert map to JSON bytes for indigo's parser
	jwkBytes, err := json.Marshal(jwkMap)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize JWK: %w", err)
	}

	// Parse with indigo's crypto package - this supports all atProto curves
	// including secp256k1 (ES256K) which Go's crypto/elliptic doesn't support
	pubKey, err := indigoCrypto.ParsePublicJWKBytes(jwkBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWK: %w", err)
	}

	return pubKey, nil
}

// parseJWTHeaderAndClaims manually parses a JWT's header and claims without using golang-jwt.
// This is necessary to support ES256K (secp256k1) which golang-jwt doesn't recognize.
func parseJWTHeaderAndClaims(tokenString string) (map[string]interface{}, *DPoPClaims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Decode header
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode JWT header: %w", err)
	}

	var header map[string]interface{}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, nil, fmt.Errorf("failed to parse JWT header: %w", err)
	}

	// Decode claims
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode JWT claims: %w", err)
	}

	// Parse into raw map first to extract standard claims
	var rawClaims map[string]interface{}
	if err := json.Unmarshal(claimsBytes, &rawClaims); err != nil {
		return nil, nil, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	// Build DPoPClaims struct
	claims := &DPoPClaims{}

	// Extract jti
	if jti, ok := rawClaims["jti"].(string); ok {
		claims.ID = jti
	}

	// Extract iat (issued at)
	if iat, ok := rawClaims["iat"].(float64); ok {
		t := time.Unix(int64(iat), 0)
		claims.IssuedAt = jwt.NewNumericDate(t)
	}

	// Extract exp (expiration) if present
	if exp, ok := rawClaims["exp"].(float64); ok {
		t := time.Unix(int64(exp), 0)
		claims.ExpiresAt = jwt.NewNumericDate(t)
	}

	// Extract nbf (not before) if present
	if nbf, ok := rawClaims["nbf"].(float64); ok {
		t := time.Unix(int64(nbf), 0)
		claims.NotBefore = jwt.NewNumericDate(t)
	}

	// Extract htm (HTTP method)
	if htm, ok := rawClaims["htm"].(string); ok {
		claims.HTTPMethod = htm
	}

	// Extract htu (HTTP URI)
	if htu, ok := rawClaims["htu"].(string); ok {
		claims.HTTPURI = htu
	}

	// Extract ath (access token hash) if present
	if ath, ok := rawClaims["ath"].(string); ok {
		claims.AccessTokenHash = ath
	}

	return header, claims, nil
}

// verifyJWTSignatureWithIndigo verifies a JWT signature using indigo's crypto package.
// This is used instead of golang-jwt for algorithms not supported by golang-jwt (like ES256K).
// It parses the JWT, extracts the signing input and signature, and uses indigo's
// PublicKey.HashAndVerifyLenient() for verification.
//
// JWT format: header.payload.signature (all base64url-encoded)
// Signature is verified over the raw bytes of "header.payload"
// (indigo's HashAndVerifyLenient handles SHA-256 hashing internally)
func verifyJWTSignatureWithIndigo(tokenString string, pubKey indigoCrypto.PublicKey) error {
	// Split the JWT into parts
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// The signing input is "header.payload" (without decoding)
	signingInput := parts[0] + "." + parts[1]

	// Decode the signature from base64url
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("failed to decode JWT signature: %w", err)
	}

	// Use indigo's verification - HashAndVerifyLenient handles hashing internally
	// and accepts both low-S and high-S signatures for maximum compatibility
	err = pubKey.HashAndVerifyLenient([]byte(signingInput), signature)
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}

	return nil
}

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
