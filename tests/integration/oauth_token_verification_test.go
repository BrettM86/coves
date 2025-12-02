package integration

import (
	"Coves/internal/api/middleware"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestOAuthTokenVerification tests end-to-end OAuth token verification
// with real PDS-issued OAuth tokens. This replaces the old JWT verification test
// since we now use OAuth sealed session tokens instead of raw JWTs.
//
// Flow:
// 1. Create account on local PDS (or use existing)
// 2. Authenticate to get OAuth tokens and create sealed session token
// 3. Verify our auth middleware can unseal and validate the token
// 4. Test token validation and session retrieval
//
// NOTE: This test uses the E2E OAuth middleware which mocks the session unsealing
// for testing purposes. Real OAuth tokens from PDS would be sealed using the
// OAuth client's seal secret.
func TestOAuthTokenVerification(t *testing.T) {
	// Skip in short mode since this requires real PDS
	if testing.Short() {
		t.Skip("Skipping OAuth token verification test in short mode")
	}

	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	// Check if PDS is running
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	t.Run("OAuth token validation and middleware integration", func(t *testing.T) {
		// Step 1: Create a test account on PDS
		// Keep handle short to avoid PDS validation errors
		timestamp := time.Now().Unix() % 100000 // Last 5 digits
		handle := fmt.Sprintf("oauth%d.local.coves.dev", timestamp)
		password := "testpass123"
		email := fmt.Sprintf("oauth%d@test.com", timestamp)

		_, did, err := createPDSAccount(pdsURL, handle, email, password)
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}
		t.Logf("✓ Created test account: %s (DID: %s)", handle, did)

		// Step 2: Create OAuth middleware with mock unsealer for testing
		// In production, this would unseal real OAuth tokens from PDS
		t.Log("Testing OAuth middleware with sealed session tokens...")

		e2eAuth := NewE2EOAuthMiddleware()
		testToken := e2eAuth.AddUser(did)

		handlerCalled := false
		var extractedDID string

		testHandler := e2eAuth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			extractedDID = middleware.GetUserDID(r)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success": true}`))
		}))

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()

		testHandler.ServeHTTP(w, req)

		if !handlerCalled {
			t.Errorf("Handler was not called - auth middleware rejected valid token")
			t.Logf("Response status: %d", w.Code)
			t.Logf("Response body: %s", w.Body.String())
		}

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
			t.Logf("Response body: %s", w.Body.String())
		}

		if extractedDID != did {
			t.Errorf("Middleware extracted wrong DID: expected %s, got %s", did, extractedDID)
		}

		t.Logf("✅ OAuth middleware with token validation working correctly!")
		t.Logf("  Handler called: %v", handlerCalled)
		t.Logf("  Extracted DID: %s", extractedDID)
		t.Logf("  Response status: %d", w.Code)
	})

	t.Run("Rejects tampered/invalid sealed tokens", func(t *testing.T) {
		// Create valid user
		timestamp := time.Now().Unix() % 100000
		handle := fmt.Sprintf("tamp%d.local.coves.dev", timestamp)
		password := "testpass456"
		email := fmt.Sprintf("tamp%d@test.com", timestamp)

		_, did, err := createPDSAccount(pdsURL, handle, email, password)
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}

		// Create OAuth middleware
		e2eAuth := NewE2EOAuthMiddleware()
		validToken := e2eAuth.AddUser(did)

		// Create various invalid tokens to test
		testCases := []struct {
			name  string
			token string
		}{
			{"Empty token", ""},
			{"Invalid base64", "not-valid-base64!!!"},
			{"Tampered token", "dGFtcGVyZWQtdG9rZW4tZGF0YQ=="}, // Valid base64 but not a real sealed session
			{"Short token", "abc"},
			{"Modified valid token", validToken + "extra"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				handlerCalled := false
				testHandler := e2eAuth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handlerCalled = true
					w.WriteHeader(http.StatusOK)
				}))

				req := httptest.NewRequest("GET", "/test", nil)
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()

				testHandler.ServeHTTP(w, req)

				if handlerCalled {
					t.Error("Handler was called for invalid token - should have been rejected")
				}

				if w.Code != http.StatusUnauthorized {
					t.Errorf("Expected status 401 for invalid token, got %d", w.Code)
				}

				t.Logf("✓ Middleware correctly rejected %s with status %d", tc.name, w.Code)
			})
		}

		t.Logf("✅ All invalid token types correctly rejected")
	})

	t.Run("Session expiration handling", func(t *testing.T) {
		// OAuth session expiration is handled at the database level
		// See TestOAuthE2E_TokenExpiration in oauth_e2e_test.go for full expiration testing
		t.Log("ℹ️  Session expiration testing is covered in oauth_e2e_test.go")
		t.Log("   OAuth sessions expire based on database timestamps and are cleaned up periodically")
		t.Log("   This is different from JWT expiration which was timestamp-based in the token itself")
		t.Skip("Session expiration is tested in oauth_e2e_test.go - see TestOAuthE2E_TokenExpiration")
	})
}
