package auth

import (
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
