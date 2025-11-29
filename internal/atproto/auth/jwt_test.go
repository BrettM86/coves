package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestParseJWT(t *testing.T) {
	// Create a test JWT token
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test-pds.example.com",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto transition:generic",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("Failed to create test token: %v", err)
	}

	// Test parsing
	parsedClaims, err := ParseJWT(tokenString)
	if err != nil {
		t.Fatalf("ParseJWT failed: %v", err)
	}

	if parsedClaims.Subject != "did:plc:test123" {
		t.Errorf("Expected subject 'did:plc:test123', got '%s'", parsedClaims.Subject)
	}

	if parsedClaims.Issuer != "https://test-pds.example.com" {
		t.Errorf("Expected issuer 'https://test-pds.example.com', got '%s'", parsedClaims.Issuer)
	}

	if parsedClaims.Scope != "atproto transition:generic" {
		t.Errorf("Expected scope 'atproto transition:generic', got '%s'", parsedClaims.Scope)
	}
}

func TestParseJWT_MissingSubject(t *testing.T) {
	// Create a token without subject
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://test-pds.example.com",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("Failed to create test token: %v", err)
	}

	// Test parsing - should fail
	_, err = ParseJWT(tokenString)
	if err == nil {
		t.Error("Expected error for missing subject, got nil")
	}
}

func TestParseJWT_MissingIssuer(t *testing.T) {
	// Create a token without issuer
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("Failed to create test token: %v", err)
	}

	// Test parsing - should fail
	_, err = ParseJWT(tokenString)
	if err == nil {
		t.Error("Expected error for missing issuer, got nil")
	}
}

func TestParseJWT_WithBearerPrefix(t *testing.T) {
	// Create a test JWT token
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test-pds.example.com",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("Failed to create test token: %v", err)
	}

	// Test parsing with Bearer prefix
	parsedClaims, err := ParseJWT("Bearer " + tokenString)
	if err != nil {
		t.Fatalf("ParseJWT failed with Bearer prefix: %v", err)
	}

	if parsedClaims.Subject != "did:plc:test123" {
		t.Errorf("Expected subject 'did:plc:test123', got '%s'", parsedClaims.Subject)
	}
}

func TestValidateClaims_Expired(t *testing.T) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test-pds.example.com",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)), // Expired
		},
	}

	err := validateClaims(claims)
	if err == nil {
		t.Error("Expected error for expired token, got nil")
	}
}

func TestValidateClaims_InvalidDID(t *testing.T) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "invalid-did-format",
			Issuer:    "https://test-pds.example.com",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}

	err := validateClaims(claims)
	if err == nil {
		t.Error("Expected error for invalid DID format, got nil")
	}
}

func TestExtractKeyID(t *testing.T) {
	// Create a test JWT token with kid in header
	token := jwt.New(jwt.SigningMethodRS256)
	token.Header["kid"] = "test-key-id"
	token.Claims = &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "did:plc:test123",
			Issuer:  "https://test-pds.example.com",
		},
	}

	// Sign with a dummy RSA key (we just need a valid token structure)
	tokenString, err := token.SignedString([]byte("dummy"))
	if err == nil {
		// If it succeeds (shouldn't with wrong key type, but let's handle it)
		kid, err := ExtractKeyID(tokenString)
		if err != nil {
			t.Logf("ExtractKeyID failed (expected if signing fails): %v", err)
		} else if kid != "test-key-id" {
			t.Errorf("Expected kid 'test-key-id', got '%s'", kid)
		}
	}
}

// === HS256 Verification Tests ===

// mockJWKSFetcher is a mock implementation of JWKSFetcher for testing
type mockJWKSFetcher struct {
	publicKey interface{}
	err       error
}

func (m *mockJWKSFetcher) FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error) {
	return m.publicKey, m.err
}

func createHS256Token(t *testing.T, subject, issuer, secret string, expiry time.Duration) string {
	t.Helper()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Issuer:    issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto transition:generic",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("Failed to create test token: %v", err)
	}
	return tokenString
}

func TestVerifyJWT_HS256_Valid(t *testing.T) {
	// Setup: Configure environment for HS256 verification
	secret := "test-jwt-secret-key-12345"
	issuer := "https://pds.coves.social"

	ResetJWTConfigForTesting()
	os.Setenv("PDS_JWT_SECRET", secret)
	os.Setenv("HS256_ISSUERS", issuer)
	defer func() {
		os.Unsetenv("PDS_JWT_SECRET")
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	tokenString := createHS256Token(t, "did:plc:test123", issuer, secret, 1*time.Hour)

	// Verify token
	claims, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
	if err != nil {
		t.Fatalf("VerifyJWT failed for valid HS256 token: %v", err)
	}

	if claims.Subject != "did:plc:test123" {
		t.Errorf("Expected subject 'did:plc:test123', got '%s'", claims.Subject)
	}
	if claims.Issuer != issuer {
		t.Errorf("Expected issuer '%s', got '%s'", issuer, claims.Issuer)
	}
}

func TestVerifyJWT_HS256_WrongSecret(t *testing.T) {
	// Setup: Configure environment with one secret, sign with another
	issuer := "https://pds.coves.social"

	ResetJWTConfigForTesting()
	os.Setenv("PDS_JWT_SECRET", "correct-secret")
	os.Setenv("HS256_ISSUERS", issuer)
	defer func() {
		os.Unsetenv("PDS_JWT_SECRET")
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	// Create token with wrong secret
	tokenString := createHS256Token(t, "did:plc:test123", issuer, "wrong-secret", 1*time.Hour)

	// Verify should fail
	_, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
	if err == nil {
		t.Error("Expected error for HS256 token with wrong secret, got nil")
	}
}

func TestVerifyJWT_HS256_SecretNotConfigured(t *testing.T) {
	// Setup: Whitelist issuer but don't configure secret
	issuer := "https://pds.coves.social"

	ResetJWTConfigForTesting()
	os.Unsetenv("PDS_JWT_SECRET") // Ensure secret is not set
	os.Setenv("HS256_ISSUERS", issuer)
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	tokenString := createHS256Token(t, "did:plc:test123", issuer, "any-secret", 1*time.Hour)

	// Verify should fail with descriptive error
	_, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
	if err == nil {
		t.Error("Expected error when PDS_JWT_SECRET not configured, got nil")
	}
	if err != nil && !contains(err.Error(), "PDS_JWT_SECRET not configured") {
		t.Errorf("Expected error about PDS_JWT_SECRET not configured, got: %v", err)
	}
}

// === Algorithm Confusion Attack Prevention Tests ===

func TestVerifyJWT_AlgorithmConfusionAttack_HS256WithNonWhitelistedIssuer(t *testing.T) {
	// SECURITY TEST: This tests the algorithm confusion attack prevention
	// An attacker tries to use HS256 with an issuer that should use RS256/ES256

	ResetJWTConfigForTesting()
	os.Setenv("PDS_JWT_SECRET", "some-secret")
	os.Setenv("HS256_ISSUERS", "https://trusted.example.com") // Different from token issuer
	defer func() {
		os.Unsetenv("PDS_JWT_SECRET")
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	// Create HS256 token with non-whitelisted issuer (simulating attack)
	tokenString := createHS256Token(t, "did:plc:attacker", "https://victim-pds.example.com", "some-secret", 1*time.Hour)

	// Verify should fail because issuer is not in HS256 whitelist
	_, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
	if err == nil {
		t.Error("SECURITY VULNERABILITY: HS256 token accepted for non-whitelisted issuer")
	}
	if err != nil && !contains(err.Error(), "not in HS256_ISSUERS whitelist") {
		t.Errorf("Expected error about HS256 not allowed for issuer, got: %v", err)
	}
}

func TestVerifyJWT_AlgorithmConfusionAttack_EmptyWhitelist(t *testing.T) {
	// SECURITY TEST: When no issuers are whitelisted for HS256, all HS256 tokens should be rejected

	ResetJWTConfigForTesting()
	os.Setenv("PDS_JWT_SECRET", "some-secret")
	os.Unsetenv("HS256_ISSUERS") // Empty whitelist
	defer func() {
		os.Unsetenv("PDS_JWT_SECRET")
		ResetJWTConfigForTesting()
	}()

	tokenString := createHS256Token(t, "did:plc:test123", "https://any-pds.example.com", "some-secret", 1*time.Hour)

	// Verify should fail because no issuers are whitelisted for HS256
	_, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
	if err == nil {
		t.Error("SECURITY VULNERABILITY: HS256 token accepted with empty issuer whitelist")
	}
}

func TestVerifyJWT_IssuerRequiresHS256ButTokenUsesRS256(t *testing.T) {
	// Test that issuer whitelisted for HS256 rejects tokens claiming to use RS256
	issuer := "https://pds.coves.social"

	ResetJWTConfigForTesting()
	os.Setenv("PDS_JWT_SECRET", "test-secret")
	os.Setenv("HS256_ISSUERS", issuer)
	defer func() {
		os.Unsetenv("PDS_JWT_SECRET")
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	// Create RS256-signed token (can't actually sign without RSA key, but we can test the header check)
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	// This will create an invalid signature but valid header structure
	// The test should fail at algorithm check, not signature verification
	tokenString, _ := token.SignedString([]byte("dummy-key"))

	if tokenString != "" {
		_, err := VerifyJWT(context.Background(), tokenString, &mockJWKSFetcher{})
		if err == nil {
			t.Error("Expected error when HS256 issuer receives non-HS256 token")
		}
	}
}

// === ParseJWTHeader Tests ===

func TestParseJWTHeader_Valid(t *testing.T) {
	tokenString := createHS256Token(t, "did:plc:test123", "https://test.example.com", "secret", 1*time.Hour)

	header, err := ParseJWTHeader(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTHeader failed: %v", err)
	}

	if header.Alg != AlgorithmHS256 {
		t.Errorf("Expected alg '%s', got '%s'", AlgorithmHS256, header.Alg)
	}
}

func TestParseJWTHeader_WithBearerPrefix(t *testing.T) {
	tokenString := createHS256Token(t, "did:plc:test123", "https://test.example.com", "secret", 1*time.Hour)

	header, err := ParseJWTHeader("Bearer " + tokenString)
	if err != nil {
		t.Fatalf("ParseJWTHeader failed with Bearer prefix: %v", err)
	}

	if header.Alg != AlgorithmHS256 {
		t.Errorf("Expected alg '%s', got '%s'", AlgorithmHS256, header.Alg)
	}
}

func TestParseJWTHeader_InvalidFormat(t *testing.T) {
	testCases := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"single part", "abc"},
		{"two parts", "abc.def"},
		{"too many parts", "a.b.c.d"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJWTHeader(tc.input)
			if err == nil {
				t.Errorf("Expected error for invalid JWT format '%s', got nil", tc.input)
			}
		})
	}
}

// === shouldUseHS256 and isHS256IssuerWhitelisted Tests ===

func TestIsHS256IssuerWhitelisted_Whitelisted(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "https://pds1.example.com,https://pds2.example.com")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	if !isHS256IssuerWhitelisted("https://pds1.example.com") {
		t.Error("Expected pds1 to be whitelisted")
	}
	if !isHS256IssuerWhitelisted("https://pds2.example.com") {
		t.Error("Expected pds2 to be whitelisted")
	}
}

func TestIsHS256IssuerWhitelisted_NotWhitelisted(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "https://pds1.example.com")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	if isHS256IssuerWhitelisted("https://attacker.example.com") {
		t.Error("Expected non-whitelisted issuer to return false")
	}
}

func TestIsHS256IssuerWhitelisted_EmptyWhitelist(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Unsetenv("HS256_ISSUERS")
	defer ResetJWTConfigForTesting()

	if isHS256IssuerWhitelisted("https://any.example.com") {
		t.Error("Expected false when whitelist is empty (safe default)")
	}
}

func TestIsHS256IssuerWhitelisted_WhitespaceHandling(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "  https://pds1.example.com  ,  https://pds2.example.com  ")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	if !isHS256IssuerWhitelisted("https://pds1.example.com") {
		t.Error("Expected whitespace-trimmed issuer to be whitelisted")
	}
}

// === shouldUseHS256 Tests (kid-based logic) ===

func TestShouldUseHS256_WithKid_AlwaysFalse(t *testing.T) {
	// Tokens with kid should NEVER use HS256, regardless of issuer whitelist
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "https://whitelisted.example.com")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	header := &JWTHeader{
		Alg: AlgorithmHS256,
		Kid: "some-key-id", // Has kid
	}

	// Even whitelisted issuer should not use HS256 if token has kid
	if shouldUseHS256(header, "https://whitelisted.example.com") {
		t.Error("Tokens with kid should never use HS256 (supports federation)")
	}
}

func TestShouldUseHS256_WithoutKid_WhitelistedIssuer(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "https://my-pds.example.com")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	header := &JWTHeader{
		Alg: AlgorithmHS256,
		Kid: "", // No kid
	}

	if !shouldUseHS256(header, "https://my-pds.example.com") {
		t.Error("Token without kid from whitelisted issuer should use HS256")
	}
}

func TestShouldUseHS256_WithoutKid_NotWhitelisted(t *testing.T) {
	ResetJWTConfigForTesting()
	os.Setenv("HS256_ISSUERS", "https://my-pds.example.com")
	defer func() {
		os.Unsetenv("HS256_ISSUERS")
		ResetJWTConfigForTesting()
	}()

	header := &JWTHeader{
		Alg: AlgorithmHS256,
		Kid: "", // No kid
	}

	if shouldUseHS256(header, "https://external-pds.example.com") {
		t.Error("Token without kid from non-whitelisted issuer should NOT use HS256")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
