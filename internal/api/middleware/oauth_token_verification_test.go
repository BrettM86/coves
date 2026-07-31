//go:build integration

package middleware_test

import (
	"Coves/internal/api/middleware"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
	t.Parallel()

	pds := testkit.NewPDS(t)

	t.Run("OAuth token validation and middleware integration", func(t *testing.T) {
		// Step 1: Create a test account on PDS
		// Keep handle short to avoid PDS validation errors
		testID := testkit.UniqueID(t)
		handle := fmt.Sprintf("oauth%s.local.coves.dev", testID)
		password := "testpass123"
		email := fmt.Sprintf("oauth%s@test.com", testID)

		account := pds.CreateAccount(t, testkit.WithHandle(handle), testkit.WithEmail(email), testkit.WithPassword(password))
		did := account.DID
		t.Logf("✓ Created test account: %s (DID: %s)", handle, did)

		// Step 2: Create OAuth middleware with mock unsealer for testing
		// In production, this would unseal real OAuth tokens from PDS
		t.Log("Testing OAuth middleware with sealed session tokens...")

		e2eAuth := fixtures.NewOAuthMiddleware()
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
		testID := testkit.UniqueID(t)
		handle := fmt.Sprintf("tamp%s.local.coves.dev", testID)
		password := "testpass456"
		email := fmt.Sprintf("tamp%s@test.com", testID)

		account := pds.CreateAccount(t, testkit.WithHandle(handle), testkit.WithEmail(email), testkit.WithPassword(password))
		did := account.DID

		// Create OAuth middleware
		e2eAuth := fixtures.NewOAuthMiddleware()
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
}
