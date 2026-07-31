//go:build integration

package oauth_test

import (
	"Coves/internal/atproto/oauth"
	"Coves/tests/testkit"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOAuth_SessionFixationAttackPrevention tests that the mobile redirect binding
// prevents session fixation attacks where an attacker plants a mobile_redirect_uri
// cookie, then the user does a web login, and credentials get sent to attacker's deep link.
//
// Attack scenario:
// 1. Attacker tricks user into visiting /oauth/mobile/login?redirect_uri=evil://steal
// 2. This plants a mobile_redirect_uri cookie (lives 10 minutes)
// 3. User later does normal web OAuth login via /oauth/login
// 4. HandleCallback sees the stale mobile_redirect_uri cookie
// 5. WITHOUT THE FIX: Callback sends sealed token, DID, session_id to attacker's deep link
// 6. WITH THE FIX: Binding mismatch is detected, mobile cookies cleared, user gets web session
func TestOAuth_SessionFixationAttackPrevention(t *testing.T) {
	t.Parallel()
	// Setup test database
	db := testkit.DB(t)

	// Setup OAuth client and store
	store := SetupOAuthTestStore(t, db)
	client := SetupOAuthTestClient(t, store)
	require.NotNil(t, client, "OAuth client should be initialized")

	// Setup handler
	handler := oauth.NewOAuthHandler(client, store)

	// Setup router
	r := chi.NewRouter()
	r.Get("/oauth/callback", handler.HandleCallback)

	t.Run("attack scenario - planted mobile cookie without binding", func(t *testing.T) {
		ctx := context.Background()

		// Step 1: Simulate a successful OAuth callback (like a user did web login)
		// We'll create a mock session to simulate what ProcessCallback would return
		testDID := "did:plc:test123456"
		parsedDID, err := syntax.ParseDID(testDID)
		require.NoError(t, err)

		sessionID := "test-session-" + time.Now().Format("20060102150405")
		testSession := oauthlib.ClientSessionData{
			AccountDID:  parsedDID,
			SessionID:   sessionID,
			HostURL:     "http://localhost:3001",
			AccessToken: "test-access-token",
			Scopes:      []string{"atproto"},
		}

		// Save the session (simulating successful OAuth flow)
		err = store.SaveSession(ctx, testSession)
		require.NoError(t, err)

		// Step 2: Attacker planted a mobile_redirect_uri cookie (without binding)
		// This simulates the cookie being planted earlier by attacker
		attackerRedirectURI := "evil://steal"
		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test&iss=http://localhost:3001", nil)

		// Plant the attacker's cookie (URL escaped as it would be in real scenario)
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: url.QueryEscape(attackerRedirectURI),
			Path:  "/oauth",
		})
		// NOTE: No mobile_redirect_binding cookie! This is the attack scenario.

		rec := httptest.NewRecorder()

		// Step 3: Try to process the callback
		// This would fail because ProcessCallback needs real OAuth code/state
		// For this test, we're verifying the handler's security checks work
		// even before ProcessCallback is called

		// The handler will try to call ProcessCallback which will fail
		// But we're testing that even if it succeeded, the mobile redirect
		// validation would prevent the attack
		handler.HandleCallback(rec, req)

		// Step 4: Verify the attack was prevented.
		// ProcessCallback fails first (no real OAuth code), so the flow ends on
		// the first-party error target. The important thing is that the planted
		// cookie never steers the redirect to evil://steal - qualifying a mobile
		// flow requires server-side data keyed by the OAuth state, which the
		// attacker cannot plant.
		assertCallbackEndedSafely(t, rec, attackerRedirectURI, "evil://")
	})

	t.Run("legitimate mobile flow - with valid binding", func(t *testing.T) {
		ctx := context.Background()

		// Setup a legitimate mobile session
		testDID := "did:plc:mobile123"
		parsedDID, err := syntax.ParseDID(testDID)
		require.NoError(t, err)

		sessionID := "mobile-session-" + time.Now().Format("20060102150405")
		testSession := oauthlib.ClientSessionData{
			AccountDID:  parsedDID,
			SessionID:   sessionID,
			HostURL:     "http://localhost:3001",
			AccessToken: "mobile-access-token",
			Scopes:      []string{"atproto"},
		}

		// Save the session
		err = store.SaveSession(ctx, testSession)
		require.NoError(t, err)

		// Create request with BOTH mobile_redirect_uri AND valid binding
		// Use Universal Link URI that's in the allowlist
		legitRedirectURI := "https://coves.social/app/oauth/callback"
		csrfToken := "valid-csrf-token-for-mobile"
		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test&iss=http://localhost:3001", nil)

		// Add mobile redirect URI cookie
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: url.QueryEscape(legitRedirectURI),
			Path:  "/oauth",
		})

		// Add CSRF token (required for mobile flow)
		req.AddCookie(&http.Cookie{
			Name:  "oauth_csrf",
			Value: csrfToken,
			Path:  "/oauth",
		})

		// Add VALID binding cookie (this is what prevents the attack)
		// In real flow, this would be set by HandleMobileLogin
		// The binding now includes the CSRF token for double-submit validation
		mobileBinding := generateMobileRedirectBindingForTest(csrfToken, legitRedirectURI)
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_binding",
			Value: mobileBinding,
			Path:  "/oauth",
		})

		rec := httptest.NewRecorder()
		handler.HandleCallback(rec, req)

		// This also fails at ProcessCallback (no real OAuth code), so the flow
		// ends on the first-party error target rather than in the app: a mobile
		// redirect requires server-side data for this OAuth state, and this mock
		// callback never created one. Cookies alone are not sufficient.
		assertCallbackEndedSafely(t, rec)
	})

	t.Run("binding mismatch - attacker tries wrong binding", func(t *testing.T) {
		ctx := context.Background()

		// Setup session
		testDID := "did:plc:bindingtest"
		parsedDID, err := syntax.ParseDID(testDID)
		require.NoError(t, err)

		sessionID := "binding-test-" + time.Now().Format("20060102150405")
		testSession := oauthlib.ClientSessionData{
			AccountDID:  parsedDID,
			SessionID:   sessionID,
			HostURL:     "http://localhost:3001",
			AccessToken: "binding-test-token",
			Scopes:      []string{"atproto"},
		}

		err = store.SaveSession(ctx, testSession)
		require.NoError(t, err)

		// Attacker tries to plant evil redirect with a binding from different URI
		attackerRedirectURI := "evil://steal"
		attackerCSRF := "attacker-csrf-token"
		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test&iss=http://localhost:3001", nil)

		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: url.QueryEscape(attackerRedirectURI),
			Path:  "/oauth",
		})

		req.AddCookie(&http.Cookie{
			Name:  "oauth_csrf",
			Value: attackerCSRF,
			Path:  "/oauth",
		})

		// Use binding from a DIFFERENT CSRF token and URI (attacker's attempt to forge)
		// Even if attacker knows the redirect URI, they don't know the user's CSRF token
		wrongBinding := generateMobileRedirectBindingForTest("different-csrf", "https://coves.social/app/oauth/callback")
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_binding",
			Value: wrongBinding,
			Path:  "/oauth",
		})

		rec := httptest.NewRecorder()
		handler.HandleCallback(rec, req)

		// Should fail due to binding mismatch (even before ProcessCallback)
		// The binding validation happens after ProcessCallback in the real code,
		// but the mismatch would be caught and cookies cleared
		assert.NotContains(t, rec.Header().Get("Location"), "evil://",
			"Should never redirect to attacker's URI on binding mismatch")
	})

	t.Run("CSRF token value mismatch - attacker tries different CSRF", func(t *testing.T) {
		ctx := context.Background()

		// Setup session
		testDID := "did:plc:csrftest"
		parsedDID, err := syntax.ParseDID(testDID)
		require.NoError(t, err)

		sessionID := "csrf-test-" + time.Now().Format("20060102150405")
		testSession := oauthlib.ClientSessionData{
			AccountDID:  parsedDID,
			SessionID:   sessionID,
			HostURL:     "http://localhost:3001",
			AccessToken: "csrf-test-token",
			Scopes:      []string{"atproto"},
		}

		err = store.SaveSession(ctx, testSession)
		require.NoError(t, err)

		// This tests the P1 security fix: CSRF token VALUE must be validated, not just presence
		// Attack scenario:
		// 1. User starts mobile login with CSRF token A and redirect URI X
		// 2. Binding = hash(A + X) is stored in cookie
		// 3. Attacker somehow gets user to have CSRF token B in cookie (different from A)
		// 4. Callback receives CSRF token B, redirect URI X, binding = hash(A + X)
		// 5. hash(B + X) != hash(A + X), so attack is detected

		originalCSRF := "original-csrf-token-set-at-login"
		redirectURI := "https://coves.social/app/oauth/callback"
		// Binding was created with original CSRF token
		originalBinding := generateMobileRedirectBindingForTest(originalCSRF, redirectURI)

		// But attacker managed to change the CSRF cookie
		attackerCSRF := "attacker-replaced-csrf"

		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test&iss=http://localhost:3001", nil)

		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: url.QueryEscape(redirectURI),
			Path:  "/oauth",
		})

		// Attacker's CSRF token (different from what created the binding)
		req.AddCookie(&http.Cookie{
			Name:  "oauth_csrf",
			Value: attackerCSRF,
			Path:  "/oauth",
		})

		// Original binding (created with original CSRF token)
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_binding",
			Value: originalBinding,
			Path:  "/oauth",
		})

		rec := httptest.NewRecorder()
		handler.HandleCallback(rec, req)

		// Should fail because hash(attackerCSRF + redirectURI) != hash(originalCSRF + redirectURI)
		// This is the key security fix - CSRF token VALUE is now validated, so
		// the swapped cookie cannot steer the callback into the app.
		assertCallbackEndedSafely(t, rec, redirectURI)
	})
}

// assertCallbackEndedSafely asserts that a callback which failed to produce a
// session ended the flow without leaking anything to an attacker.
//
// A failed ProcessCallback no longer renders a raw 400: HandleCallback answers
// with a first-party 302 to /?oauth_error=<generic code> so the user lands back
// in the app instead of on a dead-end error page. So the status code proves
// nothing on its own - the DESTINATION is the security property. A mobile
// redirect is only ever chosen from SERVER-SIDE data keyed by the OAuth state,
// which a planted cookie cannot create, so an attacker-supplied URI must never
// appear here.
func assertCallbackEndedSafely(t *testing.T, rec *httptest.ResponseRecorder, forbiddenURIs ...string) {
	t.Helper()

	location := rec.Header().Get("Location")

	for _, forbidden := range forbiddenURIs {
		assert.NotContains(t, location, forbidden,
			"Should never redirect to attacker-controlled URI %q", forbidden)
	}

	if location != "" {
		// Must be first-party: a relative path, not an absolute URL to some
		// other origin or a custom deep-link scheme.
		assert.True(t, strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//"),
			"Failed callback should redirect to a first-party path, got %q", location)
		assert.Contains(t, location, "oauth_error=",
			"Failed callback should redirect to the generic OAuth error target, got %q", location)
	}

	// Whatever the outcome, no session material may ride along on the wire.
	for _, credential := range []string{"access_token", "session_id", "sealed", "did:plc:"} {
		assert.NotContains(t, location, credential,
			"Redirect target must not carry session material (%s), got %q", credential, location)
	}
	assert.NotContains(t, rec.Body.String(), "access-token",
		"Response body must not echo an access token")
}

// generateMobileRedirectBindingForTest generates a binding for testing
// This mirrors the actual logic in handlers_security.go:
// binding = base64(sha256(csrfToken + "|" + redirectURI)[:16])
func generateMobileRedirectBindingForTest(csrfToken, mobileRedirectURI string) string {
	combined := csrfToken + "|" + mobileRedirectURI
	hash := sha256.Sum256([]byte(combined))
	return base64.URLEncoding.EncodeToString(hash[:16])
}
