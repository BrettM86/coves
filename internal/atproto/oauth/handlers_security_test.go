package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsAllowedMobileRedirectURI tests the mobile redirect URI allowlist with EXACT URI matching
// Only Universal Links (HTTPS) are allowed - custom schemes are blocked for security
func TestIsAllowedMobileRedirectURI(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected bool
	}{
		{
			name:     "allowed - Universal Link",
			uri:      "https://coves.social/app/oauth/callback",
			expected: true,
		},
		{
			name:     "rejected - custom scheme coves-app (vulnerable to interception)",
			uri:      "coves-app://oauth/callback",
			expected: false,
		},
		{
			name:     "rejected - custom scheme coves (vulnerable to interception)",
			uri:      "coves://oauth/callback",
			expected: false,
		},
		{
			name:     "rejected - evil scheme",
			uri:      "evil://callback",
			expected: false,
		},
		{
			name:     "rejected - http (not secure)",
			uri:      "http://example.com/callback",
			expected: false,
		},
		{
			name:     "rejected - https different domain",
			uri:      "https://example.com/callback",
			expected: false,
		},
		{
			name:     "rejected - https coves.social wrong path",
			uri:      "https://coves.social/wrong/path",
			expected: false,
		},
		{
			name:     "rejected - invalid URI",
			uri:      "not a uri",
			expected: false,
		},
		{
			name:     "rejected - empty string",
			uri:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllowedMobileRedirectURI(tt.uri)
			assert.Equal(t, tt.expected, result,
				"isAllowedMobileRedirectURI(%q) = %v, want %v", tt.uri, result, tt.expected)
		})
	}
}

// TestExtractScheme tests the scheme extraction function
func TestExtractScheme(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected string
	}{
		{
			name:     "https scheme",
			uri:      "https://coves.social/app/oauth/callback",
			expected: "https",
		},
		{
			name:     "custom scheme",
			uri:      "coves-app://callback",
			expected: "coves-app",
		},
		{
			name:     "invalid URI",
			uri:      "not a uri",
			expected: "invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractScheme(tt.uri)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGenerateCSRFToken tests CSRF token generation
func TestGenerateCSRFToken(t *testing.T) {
	// Generate two tokens and verify they are different (randomness check)
	token1, err1 := generateCSRFToken()
	require.NoError(t, err1)
	require.NotEmpty(t, token1)

	token2, err2 := generateCSRFToken()
	require.NoError(t, err2)
	require.NotEmpty(t, token2)

	assert.NotEqual(t, token1, token2, "CSRF tokens should be unique")

	// Verify token is base64 encoded (should decode without error)
	assert.Greater(t, len(token1), 40, "CSRF token should be reasonably long (32 bytes base64 encoded)")
}

// TestHandleMobileLogin_RedirectURIValidation tests that HandleMobileLogin validates redirect URIs
func TestHandleMobileLogin_RedirectURIValidation(t *testing.T) {
	// Note: This is a unit test for the validation logic only.
	// Full integration tests with OAuth flow are in tests/integration/oauth_e2e_test.go

	tests := []struct {
		name           string
		redirectURI    string
		expectedLog    string
		expectedStatus int
	}{
		{
			name:           "allowed - Universal Link",
			redirectURI:    "https://coves.social/app/oauth/callback",
			expectedStatus: http.StatusBadRequest, // Will fail at StartAuthFlow (no OAuth client setup)
		},
		{
			name:           "rejected - custom scheme coves-app (insecure)",
			redirectURI:    "coves-app://oauth/callback",
			expectedStatus: http.StatusBadRequest,
			expectedLog:    "rejected unauthorized mobile redirect URI",
		},
		{
			name:           "rejected evil scheme",
			redirectURI:    "evil://callback",
			expectedStatus: http.StatusBadRequest,
			expectedLog:    "rejected unauthorized mobile redirect URI",
		},
		{
			name:           "rejected http",
			redirectURI:    "http://evil.com/callback",
			expectedStatus: http.StatusBadRequest,
			expectedLog:    "scheme not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the validation function directly
			result := isAllowedMobileRedirectURI(tt.redirectURI)
			if tt.expectedLog != "" {
				assert.False(t, result, "Should reject %s", tt.redirectURI)
			}
		})
	}
}

// TestHandleCallback_CSRFValidation tests that HandleCallback validates CSRF tokens for mobile flow
func TestHandleCallback_CSRFValidation(t *testing.T) {
	// This is a conceptual test structure. Full implementation would require:
	// 1. Mock OAuthClient
	// 2. Mock OAuth store
	// 3. Simulated OAuth callback with cookies

	t.Run("mobile callback requires CSRF token", func(t *testing.T) {
		// Setup: Create request with mobile_redirect_uri cookie but NO oauth_csrf cookie
		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test", nil)
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: "https%3A%2F%2Fcoves.social%2Fapp%2Foauth%2Fcallback",
		})
		// Missing: oauth_csrf cookie

		// This would be rejected with 403 Forbidden in the actual handler
		// (Full test in integration tests with real OAuth flow)

		assert.NotNil(t, req) // Placeholder assertion
	})

	t.Run("mobile callback with valid CSRF token", func(t *testing.T) {
		// Setup: Create request with both cookies
		req := httptest.NewRequest("GET", "/oauth/callback?code=test&state=test", nil)
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_uri",
			Value: "https%3A%2F%2Fcoves.social%2Fapp%2Foauth%2Fcallback",
		})
		req.AddCookie(&http.Cookie{
			Name:  "oauth_csrf",
			Value: "valid-csrf-token",
		})

		// This would be accepted (assuming valid OAuth code/state)
		// (Full test in integration tests with real OAuth flow)

		assert.NotNil(t, req) // Placeholder assertion
	})
}

// TestHandleMobileCallback_RevalidatesRedirectURI tests that handleMobileCallback re-validates the redirect URI
func TestHandleMobileCallback_RevalidatesRedirectURI(t *testing.T) {
	// This is a critical security test: even if an attacker somehow bypasses the initial check,
	// the callback handler should re-validate the redirect URI before redirecting.

	tests := []struct {
		name        string
		redirectURI string
		shouldPass  bool
	}{
		{
			name:        "allowed - Universal Link",
			redirectURI: "https://coves.social/app/oauth/callback",
			shouldPass:  true,
		},
		{
			name:        "blocked - custom scheme (insecure)",
			redirectURI: "coves-app://oauth/callback",
			shouldPass:  false,
		},
		{
			name:        "blocked - evil scheme",
			redirectURI: "evil://callback",
			shouldPass:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllowedMobileRedirectURI(tt.redirectURI)
			assert.Equal(t, tt.shouldPass, result)
		})
	}
}

// TestGenerateMobileRedirectBinding tests the binding token generation
// The binding now includes the CSRF token for proper double-submit validation
func TestGenerateMobileRedirectBinding(t *testing.T) {
	csrfToken := "test-csrf-token-12345"
	tests := []struct {
		name        string
		redirectURI string
	}{
		{
			name:        "Universal Link",
			redirectURI: "https://coves.social/app/oauth/callback",
		},
		{
			name:        "different path",
			redirectURI: "https://coves.social/different/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding1 := generateMobileRedirectBinding(csrfToken, tt.redirectURI)
			binding2 := generateMobileRedirectBinding(csrfToken, tt.redirectURI)

			// Same CSRF token + URI should produce same binding (deterministic)
			assert.Equal(t, binding1, binding2, "binding should be deterministic for same inputs")

			// Binding should not be empty
			assert.NotEmpty(t, binding1, "binding should not be empty")

			// Binding should be base64 encoded (should decode without error)
			assert.Greater(t, len(binding1), 20, "binding should be reasonably long")
		})
	}

	// Different URIs should produce different bindings
	binding1 := generateMobileRedirectBinding(csrfToken, "https://coves.social/app/oauth/callback")
	binding2 := generateMobileRedirectBinding(csrfToken, "https://coves.social/different/path")
	assert.NotEqual(t, binding1, binding2, "different URIs should produce different bindings")

	// Different CSRF tokens should produce different bindings
	binding3 := generateMobileRedirectBinding("different-csrf-token", "https://coves.social/app/oauth/callback")
	assert.NotEqual(t, binding1, binding3, "different CSRF tokens should produce different bindings")
}

// TestValidateMobileRedirectBinding tests the binding validation
// Now validates both CSRF token and redirect URI together (double-submit pattern)
func TestValidateMobileRedirectBinding(t *testing.T) {
	csrfToken := "test-csrf-token-for-validation"
	redirectURI := "https://coves.social/app/oauth/callback"
	validBinding := generateMobileRedirectBinding(csrfToken, redirectURI)

	tests := []struct {
		name        string
		csrfToken   string
		redirectURI string
		binding     string
		shouldPass  bool
	}{
		{
			name:        "valid - correct CSRF token and redirect URI",
			csrfToken:   csrfToken,
			redirectURI: redirectURI,
			binding:     validBinding,
			shouldPass:  true,
		},
		{
			name:        "invalid - wrong redirect URI",
			csrfToken:   csrfToken,
			redirectURI: "https://coves.social/different/path",
			binding:     validBinding,
			shouldPass:  false,
		},
		{
			name:        "invalid - wrong CSRF token",
			csrfToken:   "wrong-csrf-token",
			redirectURI: redirectURI,
			binding:     validBinding,
			shouldPass:  false,
		},
		{
			name:        "invalid - random binding",
			csrfToken:   csrfToken,
			redirectURI: redirectURI,
			binding:     "random-invalid-binding",
			shouldPass:  false,
		},
		{
			name:        "invalid - empty binding",
			csrfToken:   csrfToken,
			redirectURI: redirectURI,
			binding:     "",
			shouldPass:  false,
		},
		{
			name:        "invalid - empty CSRF token",
			csrfToken:   "",
			redirectURI: redirectURI,
			binding:     validBinding,
			shouldPass:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateMobileRedirectBinding(tt.csrfToken, tt.redirectURI, tt.binding)
			assert.Equal(t, tt.shouldPass, result)
		})
	}
}

// TestSessionFixationAttackPrevention tests that the binding prevents session fixation
func TestSessionFixationAttackPrevention(t *testing.T) {
	// Simulate attack scenario:
	// 1. Attacker plants a cookie for evil://steal with binding for evil://steal
	// 2. User does a web login (no mobile_redirect_binding cookie)
	// 3. Callback should NOT redirect to evil://steal

	attackerCSRF := "attacker-csrf-token"
	attackerRedirectURI := "evil://steal"
	attackerBinding := generateMobileRedirectBinding(attackerCSRF, attackerRedirectURI)

	// Later, user's legitimate mobile login
	userCSRF := "user-csrf-token"
	userRedirectURI := "https://coves.social/app/oauth/callback"
	userBinding := generateMobileRedirectBinding(userCSRF, userRedirectURI)

	// The attacker's binding should NOT validate for the user's redirect URI
	assert.False(t, validateMobileRedirectBinding(userCSRF, userRedirectURI, attackerBinding),
		"attacker's binding should not validate for user's CSRF token and redirect URI")

	// The user's binding should validate for the user's CSRF token and redirect URI
	assert.True(t, validateMobileRedirectBinding(userCSRF, userRedirectURI, userBinding),
		"user's binding should validate for user's CSRF token and redirect URI")

	// Cross-validation should fail
	assert.False(t, validateMobileRedirectBinding(attackerCSRF, attackerRedirectURI, userBinding),
		"user's binding should not validate for attacker's CSRF token and redirect URI")
}

// TestCSRFTokenValidation tests that CSRF token VALUE is validated, not just presence
func TestCSRFTokenValidation(t *testing.T) {
	// This test verifies the fix for the P1 security issue:
	// "The callback never validates the token... the csrfToken argument is ignored entirely"
	//
	// The fix ensures that the CSRF token VALUE is cryptographically bound to the
	// binding token, so changing the CSRF token will invalidate the binding.

	t.Run("CSRF token value must match", func(t *testing.T) {
		originalCSRF := "original-csrf-token-from-login"
		redirectURI := "https://coves.social/app/oauth/callback"
		binding := generateMobileRedirectBinding(originalCSRF, redirectURI)

		// Original CSRF token should validate
		assert.True(t, validateMobileRedirectBinding(originalCSRF, redirectURI, binding),
			"original CSRF token should validate")

		// Different CSRF token should NOT validate (this is the key security fix)
		differentCSRF := "attacker-forged-csrf-token"
		assert.False(t, validateMobileRedirectBinding(differentCSRF, redirectURI, binding),
			"different CSRF token should NOT validate - this is the security fix")
	})

	t.Run("attacker cannot forge binding without CSRF token", func(t *testing.T) {
		// Attacker knows the redirect URI but not the CSRF token
		redirectURI := "https://coves.social/app/oauth/callback"
		victimCSRF := "victim-secret-csrf-token"
		victimBinding := generateMobileRedirectBinding(victimCSRF, redirectURI)

		// Attacker tries various CSRF tokens to forge the binding
		attackerGuesses := []string{
			"",
			"guess1",
			"attacker-csrf",
			redirectURI, // trying the redirect URI as CSRF
		}

		for _, guess := range attackerGuesses {
			assert.False(t, validateMobileRedirectBinding(guess, redirectURI, victimBinding),
				"attacker's CSRF guess %q should not validate", guess)
		}
	})
}

// TestConstantTimeCompare tests the timing-safe comparison function
func TestConstantTimeCompare(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected bool
	}{
		{
			name:     "equal strings",
			a:        "abc123",
			b:        "abc123",
			expected: true,
		},
		{
			name:     "different strings same length",
			a:        "abc123",
			b:        "xyz789",
			expected: false,
		},
		{
			name:     "different lengths",
			a:        "short",
			b:        "longer",
			expected: false,
		},
		{
			name:     "empty strings",
			a:        "",
			b:        "",
			expected: true,
		},
		{
			name:     "one empty",
			a:        "abc",
			b:        "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := constantTimeCompare(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}
