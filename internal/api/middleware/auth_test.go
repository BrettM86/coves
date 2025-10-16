package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

// TestRequireAuth_ValidToken tests that valid tokens are accepted (Phase 1)
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
	req.Header.Set("Authorization", "Bearer "+token)
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

// TestRequireAuth_InvalidAuthHeaderFormat tests that non-Bearer tokens are rejected
func TestRequireAuth_InvalidAuthHeaderFormat(t *testing.T) {
	fetcher := &mockJWKSFetcher{}
	middleware := NewAtProtoAuthMiddleware(fetcher, true)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Basic dGVzdDp0ZXN0") // Wrong format
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
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
	req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
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
	req.Header.Set("Authorization", "Bearer "+tokenString)
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
	req.Header.Set("Authorization", "Bearer "+tokenString)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestOptionalAuth_WithToken tests that OptionalAuth accepts valid tokens
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
	req.Header.Set("Authorization", "Bearer "+token)
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
	req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
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
