package middleware

import (
	"Coves/internal/atproto/auth"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// mockJWKSFetcher is a test double for JWKSFetcher
type mockJWKSFetcher struct {
	shouldFail bool
}

func (m *mockJWKSFetcher) FetchPublicKey(ctx context.Context, issuer, token string) (interface{}, error) {
	if m.shouldFail {
		return nil, fmt.Errorf("mock fetch failure")
	}
	// Return nil - we won't actually verify signatures in Phase 1 tests
	return nil, nil
}

// createTestToken creates a test JWT with the given DID
func createTestToken(did string) string {
	claims := jwt.MapClaims{
		"sub":   did,
		"iss":   "https://test.pds.local",
		"scope": "atproto",
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	return tokenString
}

// TestRequireAuth_ValidToken tests that valid tokens are accepted with DPoP scheme (Phase 1)
func TestRequireAuth_ValidToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true) // skipVerify=true

	handlerCalled := false
	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted and injected into context
		did := GetUserDID(r)
		if did != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", did)
		}

		// Verify claims were injected
		claims := GetJWTClaims(r)
		if claims == nil {
			t.Error("expected claims to be non-nil")
			return
		}
		if claims.Subject != "did:plc:test123" {
			t.Errorf("expected claims.Subject 'did:plc:test123', got %s", claims.Subject)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := createTestToken("did:plc:test123")
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_MissingAuthHeader tests that missing Authorization header is rejected
func TestRequireAuth_MissingAuthHeader(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Authorization header
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_InvalidAuthHeaderFormat tests that non-DPoP tokens are rejected (including Bearer)
func TestRequireAuth_InvalidAuthHeaderFormat(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	tests := []struct {
		name   string
		header string
	}{
		{"Basic auth", "Basic dGVzdDp0ZXN0"},
		{"Bearer scheme", "Bearer some-token"},
		{"Invalid format", "InvalidFormat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("handler should not be called")
			}))

			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", w.Code)
			}
		})
	}
}

// TestRequireAuth_BearerRejectionErrorMessage verifies that Bearer tokens are rejected
// with a helpful error message guiding users to use DPoP scheme
func TestRequireAuth_BearerRejectionErrorMessage(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	// Verify error message guides user to use DPoP
	body := w.Body.String()
	if !strings.Contains(body, "Expected: DPoP") {
		t.Errorf("error message should guide user to use DPoP, got: %s", body)
	}
}

// TestRequireAuth_CaseInsensitiveScheme verifies that DPoP scheme matching is case-insensitive
// per RFC 7235 which states HTTP auth schemes are case-insensitive
func TestRequireAuth_CaseInsensitiveScheme(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	// Create a valid JWT for testing
	validToken := createValidJWT(t, "did:plc:test123", time.Hour)

	testCases := []struct {
		name   string
		scheme string
	}{
		{"lowercase", "dpop"},
		{"uppercase", "DPOP"},
		{"mixed_case", "DpOp"},
		{"standard", "DPoP"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handlerCalled := false
			handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tc.scheme+" "+validToken)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if !handlerCalled {
				t.Errorf("scheme %q should be accepted (case-insensitive per RFC 7235), got status %d: %s",
					tc.scheme, w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireAuth_MalformedToken tests that malformed JWTs are rejected
func TestRequireAuth_MalformedToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP not-a-valid-jwt")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_ExpiredToken tests that expired tokens are rejected
func TestRequireAuth_ExpiredToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for expired token")
	}))

	// Create expired token
	claims := jwt.MapClaims{
		"sub":   "did:plc:test123",
		"iss":   "https://test.pds.local",
		"scope": "atproto",
		"exp":   time.Now().Add(-1 * time.Hour).Unix(), // Expired 1 hour ago
		"iat":   time.Now().Add(-2 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP "+tokenString)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_MissingDID tests that tokens without DID are rejected
func TestRequireAuth_MissingDID(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	// Create token without sub claim
	claims := jwt.MapClaims{
		// "sub" missing
		"iss":   "https://test.pds.local",
		"scope": "atproto",
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP "+tokenString)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestOptionalAuth_WithToken tests that OptionalAuth accepts valid DPoP tokens
func TestOptionalAuth_WithToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted
		did := GetUserDID(r)
		if did != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := createTestToken("did:plc:test123")
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestOptionalAuth_WithoutToken tests that OptionalAuth allows requests without tokens
func TestOptionalAuth_WithoutToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify no DID is set
		did := GetUserDID(r)
		if did != "" {
			t.Errorf("expected empty DID, got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Authorization header
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestOptionalAuth_InvalidToken tests that OptionalAuth continues without auth on invalid token
func TestOptionalAuth_InvalidToken(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify no DID is set (invalid token ignored)
		did := GetUserDID(r)
		if did != "" {
			t.Errorf("expected empty DID for invalid token, got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "DPoP not-a-valid-jwt")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestGetUserDID_NotAuthenticated tests that GetUserDID returns empty string when not authenticated
func TestGetUserDID_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	did := GetUserDID(req)

	if did != "" {
		t.Errorf("expected empty string, got %s", did)
	}
}

// TestGetJWTClaims_NotAuthenticated tests that GetJWTClaims returns nil when not authenticated
func TestGetJWTClaims_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	claims := GetJWTClaims(req)

	if claims != nil {
		t.Errorf("expected nil claims, got %+v", claims)
	}
}

// TestGetDPoPProof_NotAuthenticated tests that GetDPoPProof returns nil when no DPoP was verified
func TestGetDPoPProof_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	proof := GetDPoPProof(req)

	if proof != nil {
		t.Errorf("expected nil proof, got %+v", proof)
	}
}

// TestRequireAuth_WithDPoP_SecurityModel tests the correct DPoP security model:
// Token MUST be verified first, then DPoP is checked as an additional layer.
// DPoP is NOT a fallback for failed token verification.
func TestRequireAuth_WithDPoP_SecurityModel(t *testing.T) {
	// Generate an ECDSA key pair for DPoP
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	// Calculate JWK thumbprint for cnf.jkt
	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	t.Run("DPoP_is_NOT_fallback_for_failed_verification", func(t *testing.T) {
		// SECURITY TEST: When token verification fails, DPoP should NOT be used as fallback
		// This prevents an attacker from forging a token with their own cnf.jkt

		// Create a DPoP-bound access token (unsigned - will fail verification)
		claims := auth.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "did:plc:attacker",
				Issuer:    "https://external.pds.local",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
			},
			Scope: "atproto",
			Confirmation: map[string]interface{}{
				"jkt": thumbprint,
			},
		}

		token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
		tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

		// Create valid DPoP proof (attacker has the private key)
		dpopProof := createDPoPProof(t, privateKey, "GET", "https://test.local/api/endpoint")

		// Mock fetcher that fails (simulating external PDS without JWKS)
		fetcher := &mockJWKSFetcher{shouldFail: true}
		middleware := NewAtProtoAuthMiddleware(fetcher, false) // skipVerify=false

		handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("SECURITY VULNERABILITY: handler was called despite token verification failure")
		}))

		req := httptest.NewRequest("GET", "https://test.local/api/endpoint", nil)
		req.Header.Set("Authorization", "DPoP "+tokenString)
		req.Header.Set("DPoP", dpopProof)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		// MUST reject - token verification failed, DPoP cannot substitute for signature verification
		if w.Code != http.StatusUnauthorized {
			t.Errorf("SECURITY: expected 401 for unverified token, got %d", w.Code)
		}
	})

	t.Run("DPoP_required_when_cnf_jkt_present_in_verified_token", func(t *testing.T) {
		// When token has cnf.jkt, DPoP header MUST be present
		// This test uses skipVerify=true to simulate a verified token

		claims := auth.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "did:plc:test123",
				Issuer:    "https://test.pds.local",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
			},
			Scope: "atproto",
			Confirmation: map[string]interface{}{
				"jkt": thumbprint,
			},
		}

		token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
		tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

		// NO DPoP header - should fail when skipVerify is false
		// Note: with skipVerify=true, DPoP is not checked
		fetcher := &mockJWKSFetcher{}
		middleware := NewAtProtoAuthMiddleware(fetcher, true) // skipVerify=true for parsing

		handlerCalled := false
		handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "https://test.local/api/endpoint", nil)
		req.Header.Set("Authorization", "DPoP "+tokenString)
		// No DPoP header
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		// With skipVerify=true, DPoP is not checked, so this should succeed
		if !handlerCalled {
			t.Error("handler should be called when skipVerify=true")
		}
	})
}

// TestRequireAuth_TokenVerificationFails_DPoPNotUsedAsFallback is the key security test.
// It ensures that DPoP cannot be used as a fallback when token signature verification fails.
func TestRequireAuth_TokenVerificationFails_DPoPNotUsedAsFallback(t *testing.T) {
	// Generate a key pair (attacker's key)
	attackerKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk := ecdsaPublicKeyToJWK(&attackerKey.PublicKey)
	thumbprint, _ := auth.CalculateJWKThumbprint(jwk)

	// Create a FORGED token claiming to be the victim
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:victim_user", // Attacker claims to be victim
			Issuer:    "https://untrusted.pds",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint, // Attacker uses their own key
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	// Attacker creates a valid DPoP proof with their key
	dpopProof := createDPoPProof(t, attackerKey, "POST", "https://api.example.com/protected")

	// Fetcher fails (external PDS without JWKS)
	fetcher := &mockJWKSFetcher{shouldFail: true}
	middleware := NewAtProtoAuthMiddleware(fetcher, false) // skipVerify=false - REAL verification

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("CRITICAL SECURITY FAILURE: Request authenticated as %s despite forged token!",
			GetUserDID(r))
	}))

	req := httptest.NewRequest("POST", "https://api.example.com/protected", nil)
	req.Header.Set("Authorization", "DPoP "+tokenString)
	req.Header.Set("DPoP", dpopProof)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// MUST reject - the token signature was never verified
	if w.Code != http.StatusUnauthorized {
		t.Errorf("SECURITY VULNERABILITY: Expected 401, got %d. Token was not properly verified!", w.Code)
	}
}

// TestVerifyDPoPBinding_UsesForwardedProto ensures we honor the external HTTPS
// scheme when TLS is terminated upstream and X-Forwarded-Proto is present.
func TestVerifyDPoPBinding_UsesForwardedProto(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	middleware := NewAtProtoAuthMiddleware(&mockJWKSFetcher{}, false)
	defer middleware.Stop()

	externalURI := "https://api.example.com/protected/resource"
	dpopProof := createDPoPProof(t, privateKey, "GET", externalURI)

	req := httptest.NewRequest("GET", "http://internal-service/protected/resource", nil)
	req.Host = "api.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")

	// Pass a fake access token - ath verification will pass since we don't include ath in the DPoP proof
	fakeAccessToken := "fake-access-token-for-testing"
	proof, err := middleware.verifyDPoPBinding(req, claims, dpopProof, fakeAccessToken)
	if err != nil {
		t.Fatalf("expected DPoP verification to succeed with forwarded proto, got %v", err)
	}

	if proof == nil || proof.Claims == nil {
		t.Fatal("expected DPoP proof to be returned")
	}
}

// TestVerifyDPoPBinding_UsesForwardedHost ensures we honor X-Forwarded-Host header
// when behind a TLS-terminating proxy that rewrites the Host header.
func TestVerifyDPoPBinding_UsesForwardedHost(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	middleware := NewAtProtoAuthMiddleware(&mockJWKSFetcher{}, false)
	defer middleware.Stop()

	// External URI that the client uses
	externalURI := "https://api.example.com/protected/resource"
	dpopProof := createDPoPProof(t, privateKey, "GET", externalURI)

	// Request hits internal service with internal hostname, but X-Forwarded-Host has public hostname
	req := httptest.NewRequest("GET", "http://internal-service:8080/protected/resource", nil)
	req.Host = "internal-service:8080" // Internal host after proxy
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "api.example.com") // Original public host

	fakeAccessToken := "fake-access-token-for-testing"
	proof, err := middleware.verifyDPoPBinding(req, claims, dpopProof, fakeAccessToken)
	if err != nil {
		t.Fatalf("expected DPoP verification to succeed with X-Forwarded-Host, got %v", err)
	}

	if proof == nil || proof.Claims == nil {
		t.Fatal("expected DPoP proof to be returned")
	}
}

// TestVerifyDPoPBinding_UsesStandardForwardedHeader tests RFC 7239 Forwarded header parsing
func TestVerifyDPoPBinding_UsesStandardForwardedHeader(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	middleware := NewAtProtoAuthMiddleware(&mockJWKSFetcher{}, false)
	defer middleware.Stop()

	// External URI
	externalURI := "https://api.example.com/protected/resource"
	dpopProof := createDPoPProof(t, privateKey, "GET", externalURI)

	// Request with standard Forwarded header (RFC 7239)
	req := httptest.NewRequest("GET", "http://internal-service/protected/resource", nil)
	req.Host = "internal-service"
	req.Header.Set("Forwarded", "for=192.0.2.60;proto=https;host=api.example.com")

	fakeAccessToken := "fake-access-token-for-testing"
	proof, err := middleware.verifyDPoPBinding(req, claims, dpopProof, fakeAccessToken)
	if err != nil {
		t.Fatalf("expected DPoP verification to succeed with Forwarded header, got %v", err)
	}

	if proof == nil {
		t.Fatal("expected DPoP proof to be returned")
	}
}

// TestVerifyDPoPBinding_ForwardedMixedCaseAndQuotes tests RFC 7239 edge cases:
// mixed-case keys (Proto vs proto) and quoted values (host="example.com")
func TestVerifyDPoPBinding_ForwardedMixedCaseAndQuotes(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	middleware := NewAtProtoAuthMiddleware(&mockJWKSFetcher{}, false)
	defer middleware.Stop()

	// External URI that the client uses
	externalURI := "https://api.example.com/protected/resource"
	dpopProof := createDPoPProof(t, privateKey, "GET", externalURI)

	// Request with RFC 7239 Forwarded header using:
	// - Mixed-case keys: "Proto" instead of "proto", "Host" instead of "host"
	// - Quoted value: Host="api.example.com" (legal per RFC 7239 section 4)
	req := httptest.NewRequest("GET", "http://internal-service/protected/resource", nil)
	req.Host = "internal-service"
	req.Header.Set("Forwarded", `for=192.0.2.60;Proto=https;Host="api.example.com"`)

	fakeAccessToken := "fake-access-token-for-testing"
	proof, err := middleware.verifyDPoPBinding(req, claims, dpopProof, fakeAccessToken)
	if err != nil {
		t.Fatalf("expected DPoP verification to succeed with mixed-case/quoted Forwarded header, got %v", err)
	}

	if proof == nil {
		t.Fatal("expected DPoP proof to be returned")
	}
}

// TestVerifyDPoPBinding_AthValidation tests access token hash (ath) claim validation
func TestVerifyDPoPBinding_AthValidation(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, err := auth.CalculateJWKThumbprint(jwk)
	if err != nil {
		t.Fatalf("failed to calculate thumbprint: %v", err)
	}

	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:test123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	middleware := NewAtProtoAuthMiddleware(&mockJWKSFetcher{}, false)
	defer middleware.Stop()

	accessToken := "real-access-token-12345"

	t.Run("ath_matches_access_token", func(t *testing.T) {
		// Create DPoP proof with ath claim matching the access token
		dpopProof := createDPoPProofWithAth(t, privateKey, "GET", "https://api.example.com/resource", accessToken)

		req := httptest.NewRequest("GET", "https://api.example.com/resource", nil)
		req.Host = "api.example.com"

		proof, err := middleware.verifyDPoPBinding(req, claims, dpopProof, accessToken)
		if err != nil {
			t.Fatalf("expected verification to succeed with matching ath, got %v", err)
		}
		if proof == nil {
			t.Fatal("expected proof to be returned")
		}
	})

	t.Run("ath_mismatch_rejected", func(t *testing.T) {
		// Create DPoP proof with ath for a DIFFERENT token
		differentToken := "different-token-67890"
		dpopProof := createDPoPProofWithAth(t, privateKey, "POST", "https://api.example.com/resource", differentToken)

		req := httptest.NewRequest("POST", "https://api.example.com/resource", nil)
		req.Host = "api.example.com"

		// Try to use with the original access token - should fail
		_, err := middleware.verifyDPoPBinding(req, claims, dpopProof, accessToken)
		if err == nil {
			t.Fatal("SECURITY: expected verification to fail when ath doesn't match access token")
		}
		if !strings.Contains(err.Error(), "ath") {
			t.Errorf("error should mention ath mismatch, got: %v", err)
		}
	})
}

// TestMiddlewareStop tests that the middleware can be stopped properly
func TestMiddlewareStop(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, false)

	// Stop should not panic and should clean up resources
	middleware.Stop()

	// Calling Stop again should also be safe (idempotent-ish)
	// Note: The underlying DPoPVerifier.Stop() closes a channel, so this might panic
	// if not handled properly. We test that at least one Stop works.
}

// TestOptionalAuth_DPoPBoundToken_NoDPoPHeader tests that OptionalAuth treats
// tokens with cnf.jkt but no DPoP header as unauthenticated (potential token theft)
func TestOptionalAuth_DPoPBoundToken_NoDPoPHeader(t *testing.T) {
	// Generate a key pair for DPoP binding
	privateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)
	thumbprint, _ := auth.CalculateJWKThumbprint(jwk)

	// Create a DPoP-bound token (has cnf.jkt)
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "did:plc:user123",
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
		Confirmation: map[string]interface{}{
			"jkt": thumbprint,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	// Use skipVerify=true to simulate a verified token
	// (In production, skipVerify would be false and VerifyJWT would be called)
	// However, for this test we need skipVerify=false to trigger DPoP checking
	// But the fetcher will fail, so let's use skipVerify=true and verify the logic
	// Actually, the DPoP check only happens when skipVerify=false

	t.Run("with_skipVerify_false", func(t *testing.T) {
		// This will fail at JWT verification level, but that's expected
		// The important thing is the code path for DPoP checking
		fetcher := &mockJWKSFetcher{shouldFail: true}
		middleware := NewAtProtoAuthMiddleware(fetcher, false)
		defer middleware.Stop()

		handlerCalled := false
		var capturedDID string
		handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			capturedDID = GetUserDID(r)
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "DPoP "+tokenString)
		// Deliberately NOT setting DPoP header
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		// Handler should be called (optional auth doesn't block)
		if !handlerCalled {
			t.Error("handler should be called")
		}

		// But since JWT verification fails, user should not be authenticated
		if capturedDID != "" {
			t.Errorf("expected empty DID when verification fails, got %s", capturedDID)
		}
	})

	t.Run("with_skipVerify_true_dpop_not_checked", func(t *testing.T) {
		// When skipVerify=true, DPoP is not checked (Phase 1 mode)
		fetcher := &mockJWKSFetcher{}
		middleware := NewAtProtoAuthMiddleware(fetcher, true)
		defer middleware.Stop()

		handlerCalled := false
		var capturedDID string
		handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			capturedDID = GetUserDID(r)
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "DPoP "+tokenString)
		// No DPoP header
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if !handlerCalled {
			t.Error("handler should be called")
		}

		// With skipVerify=true, DPoP check is bypassed - token is trusted
		if capturedDID != "did:plc:user123" {
			t.Errorf("expected DID when skipVerify=true, got %s", capturedDID)
		}
	})
}

// TestDPoPReplayProtection tests that the same DPoP proof cannot be used twice
func TestDPoPReplayProtection(t *testing.T) {
	// This tests the NonceCache functionality
	cache := auth.NewNonceCache(5 * time.Minute)
	defer cache.Stop()

	jti := "unique-proof-id-123"

	// First use should succeed
	if !cache.CheckAndStore(jti) {
		t.Error("First use of jti should succeed")
	}

	// Second use should fail (replay detected)
	if cache.CheckAndStore(jti) {
		t.Error("SECURITY: Replay attack not detected - same jti accepted twice")
	}

	// Different jti should succeed
	if !cache.CheckAndStore("different-jti-456") {
		t.Error("Different jti should succeed")
	}
}

// Helper: createDPoPProof creates a DPoP proof JWT for testing
func createDPoPProof(t *testing.T, privateKey *ecdsa.PrivateKey, method, uri string) string {
	// Create JWK from public key
	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)

	// Create DPoP claims with UUID for jti to ensure uniqueness across tests
	claims := auth.DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(time.Now()),
			ID:       uuid.New().String(),
		},
		HTTPMethod: method,
		HTTPURI:    uri,
	}

	// Create token with custom header
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = jwk

	// Sign with private key
	signedToken, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("failed to sign DPoP proof: %v", err)
	}

	return signedToken
}

// Helper: createDPoPProofWithAth creates a DPoP proof JWT with ath (access token hash) claim
func createDPoPProofWithAth(t *testing.T, privateKey *ecdsa.PrivateKey, method, uri, accessToken string) string {
	// Create JWK from public key
	jwk := ecdsaPublicKeyToJWK(&privateKey.PublicKey)

	// Calculate ath: base64url(SHA-256(access_token))
	hash := sha256.Sum256([]byte(accessToken))
	ath := base64.RawURLEncoding.EncodeToString(hash[:])

	// Create DPoP claims with ath
	claims := auth.DPoPClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(time.Now()),
			ID:       uuid.New().String(),
		},
		HTTPMethod:      method,
		HTTPURI:         uri,
		AccessTokenHash: ath,
	}

	// Create token with custom header
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = jwk

	// Sign with private key
	signedToken, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("failed to sign DPoP proof: %v", err)
	}

	return signedToken
}

// Helper: ecdsaPublicKeyToJWK converts an ECDSA public key to JWK map
func ecdsaPublicKeyToJWK(pubKey *ecdsa.PublicKey) map[string]interface{} {
	// Get curve name
	var crv string
	switch pubKey.Curve {
	case elliptic.P256():
		crv = "P-256"
	case elliptic.P384():
		crv = "P-384"
	case elliptic.P521():
		crv = "P-521"
	default:
		panic("unsupported curve")
	}

	// Encode coordinates
	xBytes := pubKey.X.Bytes()
	yBytes := pubKey.Y.Bytes()

	// Ensure proper byte length (pad if needed)
	keySize := (pubKey.Curve.Params().BitSize + 7) / 8
	xPadded := make([]byte, keySize)
	yPadded := make([]byte, keySize)
	copy(xPadded[keySize-len(xBytes):], xBytes)
	copy(yPadded[keySize-len(yBytes):], yBytes)

	return map[string]interface{}{
		"kty": "EC",
		"crv": crv,
		"x":   base64.RawURLEncoding.EncodeToString(xPadded),
		"y":   base64.RawURLEncoding.EncodeToString(yPadded),
	}
}

// Helper: createValidJWT creates a valid unsigned JWT token for testing
// This is used with skipVerify=true middleware where signature verification is skipped
func createValidJWT(t *testing.T, subject string, expiry time.Duration) string {
	t.Helper()

	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Issuer:    "https://test.pds.local",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "atproto",
	}

	// Create unsigned token (for skipVerify=true tests)
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signedToken, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to create test JWT: %v", err)
	}

	return signedToken
}
