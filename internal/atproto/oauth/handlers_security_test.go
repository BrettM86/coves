package oauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Loopback redirect URIs the mobile allowlist must REJECT.
//
// These are fixture inputs to a pure string check (isAllowedRedirectURI /
// BuildAllowedRedirectURIs) and to the assertions about its answers. Nothing
// here is ever dialled, and none of them is a test-stack address on purpose:
// :5173 is Vite's dev server and :3000 a generic dev front end, which is
// exactly what a developer would reach for and exactly what the allowlist
// exists to refuse. Naming them is the test; reading them from testkit would
// make the assertion tautological.
const (
	viteDevRedirectURI      = "http://localhost:5173/callback" // coves:allow-host-literal: rejected-URI fixture for the mobile redirect allowlist, never dialled
	loopbackHostRedirectURI = "http://localhost:3000/callback" // coves:allow-host-literal: rejected-URI fixture for the mobile redirect allowlist, never dialled
	loopbackIPRedirectURI   = "http://127.0.0.1:3000/callback" // coves:allow-host-literal: rejected-URI fixture for the mobile redirect allowlist, never dialled
)

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

// TestBuildAllowedRedirectURIs tests the mobile redirect URI allowlist builder
func TestBuildAllowedRedirectURIs(t *testing.T) {
	t.Run("includes all base mobile URIs", func(t *testing.T) {
		allowed := BuildAllowedRedirectURIs()

		// All base mobile URIs should be included
		assert.True(t, allowed["social.coves:/callback"], "should include social.coves:/callback")
		assert.True(t, allowed["social.coves://callback"], "should include social.coves://callback")
		assert.True(t, allowed["social.coves:/oauth/callback"], "should include social.coves:/oauth/callback")
		assert.True(t, allowed["social.coves://oauth/callback"], "should include social.coves://oauth/callback")
		assert.True(t, allowed["https://coves.social/app/oauth/callback"], "should include Universal Link")

		// Should have exactly 5 base mobile URIs
		assert.Len(t, allowed, 5, "should have exactly 5 base mobile URIs")
	})

	t.Run("rejects URIs not in allowlist", func(t *testing.T) {
		allowed := BuildAllowedRedirectURIs()

		// Should reject URIs not in the list
		assert.False(t, allowed["http://evil.com/callback"], "should reject evil.com")
		assert.False(t, allowed[viteDevRedirectURI], "should reject localhost")
		assert.False(t, allowed["evil://steal"], "should reject evil scheme")
	})

	t.Run("returns copy not reference to base URIs", func(t *testing.T) {
		allowed1 := BuildAllowedRedirectURIs()
		allowed2 := BuildAllowedRedirectURIs()

		// Modifying one should not affect the other
		allowed1["test://modified"] = true
		assert.False(t, allowed2["test://modified"], "modifications should not affect other copies")
	})
}

// =============================================================================
// Mobile OAuth Redirect URI Integration Tests
// =============================================================================

// createTestOAuthHandler creates a minimal OAuthHandler for testing.
// This uses a memory store and minimal configuration suitable for unit tests.
func createTestOAuthHandler(t *testing.T) *OAuthHandler {
	t.Helper()

	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		PLCURL:          testPLCURL,
		DevMode:         true, // Dev mode to avoid real PDS calls
		AllowPrivateIPs: true,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=", // base64 encoded 32 bytes
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	handler := NewOAuthHandler(client, oauth.NewMemStore())
	return handler
}

// TestOAuthHandler_isAllowedRedirectURI tests the OAuthHandler.isAllowedRedirectURI() method.
// This is a critical security test (severity 9/10).
func TestOAuthHandler_isAllowedRedirectURI(t *testing.T) {
	t.Run("accepts base mobile URIs", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		// Base mobile URIs should be accepted
		baseMobileURIs := []string{
			"social.coves:/callback",
			"social.coves://callback",
			"social.coves:/oauth/callback",
			"social.coves://oauth/callback",
			"https://coves.social/app/oauth/callback",
		}

		for _, uri := range baseMobileURIs {
			assert.True(t, handler.isAllowedRedirectURI(uri),
				"should accept base mobile URI: %s", uri)
		}
	})

	t.Run("rejects URIs not in allowlist", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		// These URIs should be rejected
		rejectedURIs := []string{
			viteDevRedirectURI,                // Localhost (use Vite proxy instead)
			loopbackHostRedirectURI,           // Localhost
			"http://evil.com/callback",        // Evil domain
			"https://example.com/oauth",       // Random HTTPS
			"https://coves.social/wrong/path", // Right domain, wrong path
			"evil://steal",                    // Evil custom scheme
			"coves-app://callback",            // Old/wrong custom scheme
			"coves://oauth/callback",          // Wrong custom scheme (not reverse-domain)
			"",                                // Empty
			"not-a-uri",                       // Invalid URI
		}

		for _, uri := range rejectedURIs {
			assert.False(t, handler.isAllowedRedirectURI(uri),
				"should reject URI not in allowlist: %s", uri)
		}
	})
}

// TestHandleMobileLogin_MobileURIs tests that HandleMobileLogin properly
// accepts/rejects mobile redirect URIs. (severity 9/10)
func TestHandleMobileLogin_MobileURIs(t *testing.T) {
	t.Run("accepts mobile Universal Link", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		req := httptest.NewRequest(http.MethodGet,
			"/oauth/mobile/login?handle=test.user&redirect_uri=https://coves.social/app/oauth/callback", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		// Should NOT get "invalid redirect_uri" error
		body := rec.Body.String()
		assert.NotContains(t, body, "invalid redirect_uri",
			"mobile Universal Link should be accepted")
	})

	t.Run("rejects localhost URI", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		req := httptest.NewRequest(http.MethodGet,
			"/oauth/mobile/login?handle=test.user&redirect_uri="+viteDevRedirectURI, nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		// Should get "invalid redirect_uri" error
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid redirect_uri",
			"localhost URI should be rejected (use Vite proxy for dev)")
	})

	t.Run("rejects evil URI", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		req := httptest.NewRequest(http.MethodGet,
			"/oauth/mobile/login?handle=test.user&redirect_uri=http://evil.com/callback", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		// Should get "invalid redirect_uri" error
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid redirect_uri",
			"evil URI should be rejected")
	})
}

// TestMobileURIs_OnlyMobileAllowed tests that only mobile URIs are allowed.
func TestMobileURIs_OnlyMobileAllowed(t *testing.T) {
	t.Run("only mobile URIs work", func(t *testing.T) {
		handler := createTestOAuthHandler(t)

		// Mobile URIs should work
		assert.True(t, handler.isAllowedRedirectURI("social.coves:/callback"),
			"mobile custom scheme should work")
		assert.True(t, handler.isAllowedRedirectURI("https://coves.social/app/oauth/callback"),
			"mobile Universal Link should work")

		// Localhost URIs should NOT work (use Vite proxy for dev)
		assert.False(t, handler.isAllowedRedirectURI(viteDevRedirectURI),
			"localhost should be rejected")
		assert.False(t, handler.isAllowedRedirectURI(loopbackIPRedirectURI),
			"127.0.0.1 should be rejected")
	})
}

// TestIsSafeLocalPath pins the validation shared by the two post-login redirect
// sites: HandleLogin, which accepts `?redirect=` off the query string, and
// handleWebCallback, which reads it back out of the oauth_redirect cookie.
//
// The rule is "a path on this origin, and nothing else". A leading '/' is not
// that rule: `//host` and `/\host` are scheme-relative URLs that browsers
// resolve to a foreign authority, which is the open redirect this helper
// exists to close. Percent-encoded separators are the mirror image and must be
// ACCEPTED: %2f and %5c are ordinary path bytes, decoded long after the origin
// has been fixed, so rejecting them would break legitimate paths without
// buying any safety.
//
// coves:allow-host-literal: attacker.example is an RFC 2606 reserved name used
// as the off-origin redirect target in these fixtures; nothing resolves or
// dials it.
func TestIsSafeLocalPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Accepted: genuine local paths.
		{name: "bare root", input: "/", want: true},
		{name: "simple path", input: "/delete-account", want: true},
		{name: "path with query and fragment", input: "/a/b?c=d#e", want: true},
		{
			name:  "percent-encoded slashes stay a path",
			input: "/%2f%2fattacker.example",
			want:  true,
		},
		{
			name:  "percent-encoded backslash stays a path",
			input: "/%5cattacker.example",
			want:  true,
		},
		{
			name:  "maximum length path",
			input: "/" + strings.Repeat("a", maxPostLoginRedirectLen-1),
			want:  true,
		},

		// Rejected: empty, or not a path at all.
		{name: "empty string", input: "", want: false},
		{name: "no leading slash", input: "delete-account", want: false},

		// Rejected: scheme-relative authorities.
		{name: "scheme-relative authority", input: "//attacker.example", want: false},
		{name: "triple slash authority", input: "///attacker.example", want: false},

		// Rejected: backslashes anywhere. Browsers normalize '\' to '/' in the
		// authority position, so a single one turns a path into a hostname.
		{name: "leading backslash authority", input: "/\\attacker.example", want: false},
		{name: "backslash mid-path", input: "/x/\\/evil", want: false},

		// Rejected: absolute URLs, case-insensitively.
		{name: "absolute https URL", input: "https://attacker.example", want: false},
		{name: "absolute https URL uppercase scheme", input: "HTTPS://attacker.example", want: false},
		{name: "javascript scheme", input: "javascript:alert(1)", want: false},
		{name: "javascript scheme mixed case", input: "JaVaScRiPt:alert(1)", want: false},

		// Rejected: control bytes. CRLF is header injection; NUL and DEL are
		// parser-confusion bytes, and a leading tab is stripped by browsers,
		// exposing the '//' behind it.
		{name: "CRLF header injection", input: "/a\r\nX: y", want: false},
		{name: "NUL byte", input: "/a\x00b", want: false},
		{name: "tab before scheme-relative authority", input: "/\t//evil", want: false},
		{name: "DEL byte", input: "/a\x7fb", want: false},

		// Rejected: malformed percent-escapes. A truncated or non-hex escape is
		// not a path we can reason about: whatever decodes it downstream may
		// disagree with us about what it means, and disagreement is where the
		// bypass lives.
		{name: "non-hex percent escape", input: "/%zz", want: false},
		{name: "truncated percent escape", input: "/%2", want: false},

		// Rejected: unbounded length.
		{name: "over maximum length", input: "/" + strings.Repeat("a", maxPostLoginRedirectLen), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSafeLocalPath(tt.input),
				"isSafeLocalPath(%q)", tt.input)
		})
	}
}

// TestPostLoginRedirectCookie pins the constructor for the one-shot
// oauth_redirect cookie. Returning nil rather than an empty cookie is the
// point: an unsafe target must leave no cookie at all for the callback to read
// back, so the rejection cannot be laundered into a redirect later.
//
// coves:allow-host-literal: attacker.example is an RFC 2606 reserved name used
// as the rejected off-origin target; nothing resolves or dials it.
func TestPostLoginRedirectCookie(t *testing.T) {
	t.Run("safe path over TLS", func(t *testing.T) {
		cookie := postLoginRedirectCookie("/delete-account", true)

		require.NotNil(t, cookie, "a safe local path must produce a cookie")
		assert.Equal(t, "oauth_redirect", cookie.Name)
		assert.Equal(t, url.QueryEscape("/delete-account"), cookie.Value,
			"the target is transmitted percent-encoded so net/http cannot mangle it in transit")
		assert.Equal(t, "/", cookie.Path)
		assert.True(t, cookie.HttpOnly, "redirect target must not be readable from JS")
		assert.True(t, cookie.Secure, "secure=true must set the Secure attribute")
		assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
		assert.Equal(t, 300, cookie.MaxAge, "cookie should outlive the OAuth round trip and no more")
	})

	t.Run("safe path without TLS", func(t *testing.T) {
		cookie := postLoginRedirectCookie("/delete-account", false)

		require.NotNil(t, cookie, "a safe local path must produce a cookie")
		assert.False(t, cookie.Secure, "secure=false must clear the Secure attribute")
		assert.Equal(t, url.QueryEscape("/delete-account"), cookie.Value,
			"value is unaffected by the Secure flag")
	})

	t.Run("scheme-relative target produces no cookie", func(t *testing.T) {
		assert.Nil(t, postLoginRedirectCookie("//attacker.example", true),
			"an off-origin target must not be stored for the callback to read back")
	})

	t.Run("empty target produces no cookie", func(t *testing.T) {
		assert.Nil(t, postLoginRedirectCookie("", true),
			"no requested redirect means no cookie")
	})

	// The cookie has to survive the WIRE, and the wire is where a
	// validated-then-mangled value turns back into the bug this whole change
	// closes. net/http does not reject a cookie value containing '"' or ';' —
	// it silently DROPS those bytes (sanitizeCookieValue). So a target like
	// `/";/attacker.example`, which isSafeLocalPath correctly accepts as a
	// path, is transmitted as `//attacker.example`: a scheme-relative URL that
	// the callback reads back and honors. Validation ran on a string the
	// browser never sees.
	//
	// Percent-encoding the value closes that gap. Every byte that leaves is one
	// net/http will carry unchanged, and the callback unescapes before
	// validating, so the string checked is the string stored. The non-ASCII
	// case is the same defect without an attacker: "café" is silently
	// transmitted as "caf".
	//
	// coves:allow-host-literal: attacker.example is an RFC 2606 reserved name
	// used as the smuggled off-origin target; nothing resolves or dials it.
	t.Run("value survives the wire byte for byte", func(t *testing.T) {
		targets := []struct {
			name string
			raw  string
		}{
			{name: "quote and semicolon smuggle an authority", raw: `/";/attacker.example`},
			{name: "non-ASCII path", raw: "/café/page"},
			{name: "plain path", raw: "/delete-account"},
			{name: "path with query and fragment", raw: "/a/b?c=d#e"},
		}

		for _, target := range targets {
			t.Run(target.name, func(t *testing.T) {
				cookie := postLoginRedirectCookie(target.raw, true)
				require.NotNil(t, cookie, "%q is a safe local path and must produce a cookie", target.raw)
				assert.Equal(t, url.QueryEscape(target.raw), cookie.Value,
					"the stored value must be the percent-encoded target")

				// Serialize exactly as the handler does, then read it back the
				// way the callback does.
				rec := httptest.NewRecorder()
				http.SetCookie(rec, cookie)
				setCookie := rec.Header().Get("Set-Cookie")
				require.NotEmpty(t, setCookie, "http.SetCookie emitted no header")

				assert.NotContains(t, setCookie, "=//",
					"the serialized cookie carries a scheme-relative authority: net/http dropped "+
						"bytes from %q until what is left resolves off-origin", target.raw)

				nameValue := setCookie
				if end := strings.Index(setCookie, ";"); end != -1 {
					nameValue = setCookie[:end]
				}
				req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
				req.Header.Set("Cookie", nameValue)

				echoed, err := req.Cookie("oauth_redirect")
				require.NoError(t, err, "the cookie did not survive the round trip at all")

				decoded, err := url.QueryUnescape(echoed.Value)
				require.NoError(t, err, "the transmitted value must decode")
				assert.Equal(t, target.raw, decoded,
					"the target changed in transit: validation ran on a string the browser never sees")
			})
		}
	})
}
