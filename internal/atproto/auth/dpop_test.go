package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// === Test Helpers ===

// testECKey holds a test ES256 key pair
type testECKey struct {
	privateKey *ecdsa.PrivateKey
	publicKey  *ecdsa.PublicKey
	jwk        map[string]interface{}
	thumbprint string
}

// generateTestES256Key generates a test ES256 key pair and JWK
func generateTestES256Key(t *testing.T) *testECKey {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	// Encode public key coordinates as base64url
	xBytes := privateKey.PublicKey.X.Bytes()
	yBytes := privateKey.PublicKey.Y.Bytes()

	// P-256 coordinates must be 32 bytes (pad if needed)
	xBytes = padTo32Bytes(xBytes)
	yBytes = padTo32Bytes(yBytes)

	x := base64.RawURLEncoding.EncodeToString(xBytes)
	y := base64.RawURLEncoding.EncodeToString(yBytes)

	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   x,
		"y":   y,
	}

	// Calculate thumbprint
	thumbprint, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("Failed to calculate thumbprint: %v", err)
	}

	return &testECKey{
		privateKey: privateKey,
		publicKey:  &privateKey.PublicKey,
		jwk:        jwk,
		thumbprint: thumbprint,
	}
}

// padTo32Bytes pads a byte slice to 32 bytes (required for P-256 coordinates)
func padTo32Bytes(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	padded := make([]byte, 32)
	copy(padded[32-len(b):], b)
	return padded
}

// createDPoPProof creates a DPoP proof JWT for testing
func createDPoPProof(t *testing.T, key *testECKey, method, uri string, iat time.Time, jti string) string {
	t.Helper()

	claims := &DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: jwt.NewNumericDate(iat),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = key.jwk

	tokenString, err := token.SignedString(key.privateKey)
	if err != nil {
		t.Fatalf("Failed to create DPoP proof: %v", err)
	}

	return tokenString
}

// === JWK Thumbprint Tests (RFC 7638) ===

func TestCalculateJWKThumbprint_EC_P256(t *testing.T) {
	// Test with known values from RFC 7638 Appendix A (adapted for P-256)
	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "WKn-ZIGevcwGIyyrzFoZNBdaq9_TsqzGl96oc0CWuis",
		"y":   "y77t-RvAHRKTsSGdIYUfweuOvwrvDD-Q3Hv5J0fSKbE",
	}

	thumbprint, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("CalculateJWKThumbprint failed: %v", err)
	}

	if thumbprint == "" {
		t.Error("Expected non-empty thumbprint")
	}

	// Verify it's valid base64url
	_, err = base64.RawURLEncoding.DecodeString(thumbprint)
	if err != nil {
		t.Errorf("Thumbprint is not valid base64url: %v", err)
	}

	// Verify length (SHA-256 produces 32 bytes = 43 base64url chars)
	if len(thumbprint) != 43 {
		t.Errorf("Expected thumbprint length 43, got %d", len(thumbprint))
	}
}

func TestCalculateJWKThumbprint_Deterministic(t *testing.T) {
	// Same key should produce same thumbprint
	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "test-x-coordinate",
		"y":   "test-y-coordinate",
	}

	thumbprint1, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("First CalculateJWKThumbprint failed: %v", err)
	}

	thumbprint2, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("Second CalculateJWKThumbprint failed: %v", err)
	}

	if thumbprint1 != thumbprint2 {
		t.Errorf("Thumbprints are not deterministic: %s != %s", thumbprint1, thumbprint2)
	}
}

func TestCalculateJWKThumbprint_DifferentKeys(t *testing.T) {
	// Different keys should produce different thumbprints
	jwk1 := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "coordinate-x-1",
		"y":   "coordinate-y-1",
	}

	jwk2 := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "coordinate-x-2",
		"y":   "coordinate-y-2",
	}

	thumbprint1, err := CalculateJWKThumbprint(jwk1)
	if err != nil {
		t.Fatalf("First CalculateJWKThumbprint failed: %v", err)
	}

	thumbprint2, err := CalculateJWKThumbprint(jwk2)
	if err != nil {
		t.Fatalf("Second CalculateJWKThumbprint failed: %v", err)
	}

	if thumbprint1 == thumbprint2 {
		t.Error("Different keys produced same thumbprint (collision)")
	}
}

func TestCalculateJWKThumbprint_MissingKty(t *testing.T) {
	jwk := map[string]interface{}{
		"crv": "P-256",
		"x":   "test-x",
		"y":   "test-y",
	}

	_, err := CalculateJWKThumbprint(jwk)
	if err == nil {
		t.Error("Expected error for missing kty, got nil")
	}
	if err != nil && !contains(err.Error(), "missing kty") {
		t.Errorf("Expected error about missing kty, got: %v", err)
	}
}

func TestCalculateJWKThumbprint_EC_MissingCrv(t *testing.T) {
	jwk := map[string]interface{}{
		"kty": "EC",
		"x":   "test-x",
		"y":   "test-y",
	}

	_, err := CalculateJWKThumbprint(jwk)
	if err == nil {
		t.Error("Expected error for missing crv, got nil")
	}
	if err != nil && !contains(err.Error(), "missing crv") {
		t.Errorf("Expected error about missing crv, got: %v", err)
	}
}

func TestCalculateJWKThumbprint_EC_MissingX(t *testing.T) {
	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"y":   "test-y",
	}

	_, err := CalculateJWKThumbprint(jwk)
	if err == nil {
		t.Error("Expected error for missing x, got nil")
	}
	if err != nil && !contains(err.Error(), "missing x") {
		t.Errorf("Expected error about missing x, got: %v", err)
	}
}

func TestCalculateJWKThumbprint_EC_MissingY(t *testing.T) {
	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "test-x",
	}

	_, err := CalculateJWKThumbprint(jwk)
	if err == nil {
		t.Error("Expected error for missing y, got nil")
	}
	if err != nil && !contains(err.Error(), "missing y") {
		t.Errorf("Expected error about missing y, got: %v", err)
	}
}

func TestCalculateJWKThumbprint_RSA(t *testing.T) {
	// Test RSA key thumbprint calculation
	jwk := map[string]interface{}{
		"kty": "RSA",
		"e":   "AQAB",
		"n":   "test-modulus",
	}

	thumbprint, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("CalculateJWKThumbprint failed for RSA: %v", err)
	}

	if thumbprint == "" {
		t.Error("Expected non-empty thumbprint for RSA key")
	}
}

func TestCalculateJWKThumbprint_OKP(t *testing.T) {
	// Test OKP (Octet Key Pair) thumbprint calculation
	jwk := map[string]interface{}{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   "test-x-coordinate",
	}

	thumbprint, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("CalculateJWKThumbprint failed for OKP: %v", err)
	}

	if thumbprint == "" {
		t.Error("Expected non-empty thumbprint for OKP key")
	}
}

func TestCalculateJWKThumbprint_UnsupportedKeyType(t *testing.T) {
	jwk := map[string]interface{}{
		"kty": "UNKNOWN",
	}

	_, err := CalculateJWKThumbprint(jwk)
	if err == nil {
		t.Error("Expected error for unsupported key type, got nil")
	}
	if err != nil && !contains(err.Error(), "unsupported JWK key type") {
		t.Errorf("Expected error about unsupported key type, got: %v", err)
	}
}

func TestCalculateJWKThumbprint_CanonicalJSON(t *testing.T) {
	// RFC 7638 requires lexicographic ordering of keys in canonical JSON
	// This test verifies that the canonical JSON is correctly ordered

	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "x-coord",
		"y":   "y-coord",
	}

	// The canonical JSON should be: {"crv":"P-256","kty":"EC","x":"x-coord","y":"y-coord"}
	// (lexicographically ordered: crv, kty, x, y)

	canonical := map[string]string{
		"crv": "P-256",
		"kty": "EC",
		"x":   "x-coord",
		"y":   "y-coord",
	}

	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("Failed to marshal canonical JSON: %v", err)
	}

	expectedHash := sha256.Sum256(canonicalJSON)
	expectedThumbprint := base64.RawURLEncoding.EncodeToString(expectedHash[:])

	actualThumbprint, err := CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("CalculateJWKThumbprint failed: %v", err)
	}

	if actualThumbprint != expectedThumbprint {
		t.Errorf("Thumbprint doesn't match expected canonical JSON hash\nExpected: %s\nGot: %s",
			expectedThumbprint, actualThumbprint)
	}
}

// === DPoP Proof Verification Tests ===

func TestVerifyDPoPProof_Valid(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	result, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed for valid proof: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil proof result")
	}

	if result.Claims.HTTPMethod != method {
		t.Errorf("Expected method %s, got %s", method, result.Claims.HTTPMethod)
	}

	if result.Claims.HTTPURI != uri {
		t.Errorf("Expected URI %s, got %s", uri, result.Claims.HTTPURI)
	}

	if result.Claims.ID != jti {
		t.Errorf("Expected jti %s, got %s", jti, result.Claims.ID)
	}

	if result.Thumbprint != key.thumbprint {
		t.Errorf("Expected thumbprint %s, got %s", key.thumbprint, result.Thumbprint)
	}
}

func TestVerifyDPoPProof_InvalidSignature(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)
	wrongKey := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	// Create proof with one key
	proof := createDPoPProof(t, key, method, uri, iat, jti)

	// Parse and modify to use wrong key's JWK in header (signature won't match)
	parts := splitJWT(proof)
	header := parseJWTHeader(t, parts[0])
	header["jwk"] = wrongKey.jwk
	modifiedHeader := encodeJSON(t, header)
	tamperedProof := modifiedHeader + "." + parts[1] + "." + parts[2]

	_, err := verifier.VerifyDPoPProof(tamperedProof, method, uri)
	if err == nil {
		t.Error("Expected error for invalid signature, got nil")
	}
	if err != nil && !contains(err.Error(), "signature verification failed") {
		t.Errorf("Expected signature verification error, got: %v", err)
	}
}

func TestVerifyDPoPProof_WrongHTTPMethod(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	wrongMethod := "GET"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, wrongMethod, uri)
	if err == nil {
		t.Error("Expected error for HTTP method mismatch, got nil")
	}
	if err != nil && !contains(err.Error(), "htm mismatch") {
		t.Errorf("Expected htm mismatch error, got: %v", err)
	}
}

func TestVerifyDPoPProof_WrongURI(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	wrongURI := "https://api.example.com/different"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, method, wrongURI)
	if err == nil {
		t.Error("Expected error for URI mismatch, got nil")
	}
	if err != nil && !contains(err.Error(), "htu mismatch") {
		t.Errorf("Expected htu mismatch error, got: %v", err)
	}
}

func TestVerifyDPoPProof_URIWithQuery(t *testing.T) {
	// URI comparison should strip query and fragment
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	baseURI := "https://api.example.com/resource"
	uriWithQuery := baseURI + "?param=value"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, baseURI, iat, jti)

	// Should succeed because query is stripped
	_, err := verifier.VerifyDPoPProof(proof, method, uriWithQuery)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed for URI with query: %v", err)
	}
}

func TestVerifyDPoPProof_URIWithFragment(t *testing.T) {
	// URI comparison should strip query and fragment
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	baseURI := "https://api.example.com/resource"
	uriWithFragment := baseURI + "#section"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, baseURI, iat, jti)

	// Should succeed because fragment is stripped
	_, err := verifier.VerifyDPoPProof(proof, method, uriWithFragment)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed for URI with fragment: %v", err)
	}
}

func TestVerifyDPoPProof_ExpiredProof(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	// Proof issued 10 minutes ago (exceeds default MaxProofAge of 5 minutes)
	iat := time.Now().Add(-10 * time.Minute)
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for expired proof, got nil")
	}
	if err != nil && !contains(err.Error(), "too old") {
		t.Errorf("Expected 'too old' error, got: %v", err)
	}
}

func TestVerifyDPoPProof_FutureProof(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	// Proof issued 1 minute in the future (exceeds MaxClockSkew)
	iat := time.Now().Add(1 * time.Minute)
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for future proof, got nil")
	}
	if err != nil && !contains(err.Error(), "in the future") {
		t.Errorf("Expected 'in the future' error, got: %v", err)
	}
}

func TestVerifyDPoPProof_WithinClockSkew(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	// Proof issued 15 seconds in the future (within MaxClockSkew of 30s)
	iat := time.Now().Add(15 * time.Second)
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed for proof within clock skew: %v", err)
	}
}

func TestVerifyDPoPProof_MissingJti(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()

	claims := &DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			// No ID (jti)
			IssuedAt: jwt.NewNumericDate(iat),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = key.jwk

	proof, err := token.SignedString(key.privateKey)
	if err != nil {
		t.Fatalf("Failed to create test proof: %v", err)
	}

	_, err = verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for missing jti, got nil")
	}
	if err != nil && !contains(err.Error(), "missing jti") {
		t.Errorf("Expected missing jti error, got: %v", err)
	}
}

func TestVerifyDPoPProof_MissingTypHeader(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	claims := &DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: jwt.NewNumericDate(iat),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	// Don't set typ header
	token.Header["jwk"] = key.jwk

	proof, err := token.SignedString(key.privateKey)
	if err != nil {
		t.Fatalf("Failed to create test proof: %v", err)
	}

	_, err = verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for missing typ header, got nil")
	}
	if err != nil && !contains(err.Error(), "typ must be 'dpop+jwt'") {
		t.Errorf("Expected typ header error, got: %v", err)
	}
}

func TestVerifyDPoPProof_WrongTypHeader(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	claims := &DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: jwt.NewNumericDate(iat),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "JWT" // Wrong typ
	token.Header["jwk"] = key.jwk

	proof, err := token.SignedString(key.privateKey)
	if err != nil {
		t.Fatalf("Failed to create test proof: %v", err)
	}

	_, err = verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for wrong typ header, got nil")
	}
	if err != nil && !contains(err.Error(), "typ must be 'dpop+jwt'") {
		t.Errorf("Expected typ header error, got: %v", err)
	}
}

func TestVerifyDPoPProof_MissingJWK(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	claims := &DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: jwt.NewNumericDate(iat),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	// Don't include JWK

	proof, err := token.SignedString(key.privateKey)
	if err != nil {
		t.Fatalf("Failed to create test proof: %v", err)
	}

	_, err = verifier.VerifyDPoPProof(proof, method, uri)
	if err == nil {
		t.Error("Expected error for missing jwk header, got nil")
	}
	if err != nil && !contains(err.Error(), "missing jwk") {
		t.Errorf("Expected missing jwk error, got: %v", err)
	}
}

func TestVerifyDPoPProof_CustomTimeSettings(t *testing.T) {
	verifier := &DPoPVerifier{
		MaxClockSkew: 1 * time.Minute,
		MaxProofAge:  10 * time.Minute,
	}
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	// Proof issued 50 seconds in the future (within custom MaxClockSkew)
	iat := time.Now().Add(50 * time.Second)
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	_, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed with custom time settings: %v", err)
	}
}

func TestVerifyDPoPProof_HTTPMethodCaseInsensitive(t *testing.T) {
	// HTTP method comparison should be case-insensitive per spec
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "post"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	// Verify with uppercase method
	_, err := verifier.VerifyDPoPProof(proof, "POST", uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed for case-insensitive method: %v", err)
	}
}

// === Token Binding Verification Tests ===

func TestVerifyTokenBinding_Matching(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	result, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed: %v", err)
	}

	// Verify token binding with matching thumbprint
	err = verifier.VerifyTokenBinding(result, key.thumbprint)
	if err != nil {
		t.Fatalf("VerifyTokenBinding failed for matching thumbprint: %v", err)
	}
}

func TestVerifyTokenBinding_Mismatch(t *testing.T) {
	verifier := NewDPoPVerifier()
	key := generateTestES256Key(t)
	wrongKey := generateTestES256Key(t)

	method := "POST"
	uri := "https://api.example.com/resource"
	iat := time.Now()
	jti := uuid.New().String()

	proof := createDPoPProof(t, key, method, uri, iat, jti)

	result, err := verifier.VerifyDPoPProof(proof, method, uri)
	if err != nil {
		t.Fatalf("VerifyDPoPProof failed: %v", err)
	}

	// Verify token binding with wrong thumbprint
	err = verifier.VerifyTokenBinding(result, wrongKey.thumbprint)
	if err == nil {
		t.Error("Expected error for thumbprint mismatch, got nil")
	}
	if err != nil && !contains(err.Error(), "thumbprint mismatch") {
		t.Errorf("Expected thumbprint mismatch error, got: %v", err)
	}
}

// === ExtractCnfJkt Tests ===

func TestExtractCnfJkt_Valid(t *testing.T) {
	expectedJkt := "test-thumbprint-123"
	claims := &Claims{
		Confirmation: map[string]interface{}{
			"jkt": expectedJkt,
		},
	}

	jkt, err := ExtractCnfJkt(claims)
	if err != nil {
		t.Fatalf("ExtractCnfJkt failed for valid claims: %v", err)
	}

	if jkt != expectedJkt {
		t.Errorf("Expected jkt %s, got %s", expectedJkt, jkt)
	}
}

func TestExtractCnfJkt_MissingCnf(t *testing.T) {
	claims := &Claims{
		// No Confirmation
	}

	_, err := ExtractCnfJkt(claims)
	if err == nil {
		t.Error("Expected error for missing cnf, got nil")
	}
	if err != nil && !contains(err.Error(), "missing cnf claim") {
		t.Errorf("Expected missing cnf error, got: %v", err)
	}
}

func TestExtractCnfJkt_NilCnf(t *testing.T) {
	claims := &Claims{
		Confirmation: nil,
	}

	_, err := ExtractCnfJkt(claims)
	if err == nil {
		t.Error("Expected error for nil cnf, got nil")
	}
	if err != nil && !contains(err.Error(), "missing cnf claim") {
		t.Errorf("Expected missing cnf error, got: %v", err)
	}
}

func TestExtractCnfJkt_MissingJkt(t *testing.T) {
	claims := &Claims{
		Confirmation: map[string]interface{}{
			"other": "value",
		},
	}

	_, err := ExtractCnfJkt(claims)
	if err == nil {
		t.Error("Expected error for missing jkt, got nil")
	}
	if err != nil && !contains(err.Error(), "missing jkt") {
		t.Errorf("Expected missing jkt error, got: %v", err)
	}
}

func TestExtractCnfJkt_EmptyJkt(t *testing.T) {
	claims := &Claims{
		Confirmation: map[string]interface{}{
			"jkt": "",
		},
	}

	_, err := ExtractCnfJkt(claims)
	if err == nil {
		t.Error("Expected error for empty jkt, got nil")
	}
	if err != nil && !contains(err.Error(), "missing jkt") {
		t.Errorf("Expected missing jkt error, got: %v", err)
	}
}

func TestExtractCnfJkt_WrongType(t *testing.T) {
	claims := &Claims{
		Confirmation: map[string]interface{}{
			"jkt": 123, // Not a string
		},
	}

	_, err := ExtractCnfJkt(claims)
	if err == nil {
		t.Error("Expected error for wrong type jkt, got nil")
	}
	if err != nil && !contains(err.Error(), "missing jkt") {
		t.Errorf("Expected missing jkt error, got: %v", err)
	}
}

// === Helper Functions for Tests ===

// splitJWT splits a JWT into its three parts
func splitJWT(token string) []string {
	return []string{
		token[:strings.IndexByte(token, '.')],
		token[strings.IndexByte(token, '.')+1 : strings.LastIndexByte(token, '.')],
		token[strings.LastIndexByte(token, '.')+1:],
	}
}

// parseJWTHeader parses a base64url-encoded JWT header
func parseJWTHeader(t *testing.T, encoded string) map[string]interface{} {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("Failed to decode header: %v", err)
	}

	var header map[string]interface{}
	if err := json.Unmarshal(decoded, &header); err != nil {
		t.Fatalf("Failed to unmarshal header: %v", err)
	}

	return header
}

// encodeJSON encodes a value to base64url-encoded JSON
func encodeJSON(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}
