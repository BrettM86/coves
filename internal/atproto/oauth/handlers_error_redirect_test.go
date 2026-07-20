package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// OAuth Callback Error Redirect Tests
//
// These tests cover the authorization-server error branch of HandleCallback
// (?error=... callbacks), the ProcessCallback-failure fallback, and the
// helpers behind them (clampOAuthError, webErrorRedirect, redirectMobileError).
//
// The contract under test:
//   - Mobile flows are qualified by SERVER-STORED data (keyed by OAuth state),
//     not by cookies. Zero cookies must still redirect errors into the app.
//   - Forged callbacks (no state, or state with no pending auth request) go to
//     the web root with ?oauth_error=invalid_request and must NOT clear mobile
//     cookies or redirect into the app.
//   - Error codes are clamped to a known OAuth allowlist; unknown codes become
//     server_error and their description is dropped.
//   - ProcessCallback failures redirect (302) instead of rendering a raw 400
//     text page.
// =============================================================================

// fakeMobileAuthStore is a test double implementing oauth.ClientAuthStore
// (via an embedded indigo MemStore) plus MobileOAuthStore, so NewOAuthHandler
// wires it up as h.mobileStore.
//
// GetMobileOAuthData mirrors PostgresOAuthStore semantics:
//   - unknown state            -> (nil, ErrAuthRequestNotFound)  no pending row
//   - seeded web-flow state    -> (nil, nil)                     row, no mobile data
//   - seeded mobile-flow state -> (*MobileOAuthData, nil)        mobile row
//   - lookupErr set            -> (nil, lookupErr)               DB failure
type fakeMobileAuthStore struct {
	oauth.ClientAuthStore
	mu        sync.Mutex
	rows      map[string]*MobileOAuthData
	lookupErr error
}

var (
	_ oauth.ClientAuthStore = (*fakeMobileAuthStore)(nil)
	_ MobileOAuthStore      = (*fakeMobileAuthStore)(nil)
)

func newFakeMobileAuthStore() *fakeMobileAuthStore {
	return &fakeMobileAuthStore{
		ClientAuthStore: oauth.NewMemStore(),
		rows:            make(map[string]*MobileOAuthData),
	}
}

func (s *fakeMobileAuthStore) SaveMobileOAuthData(_ context.Context, state string, data MobileOAuthData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[state] = &data
	return nil
}

func (s *fakeMobileAuthStore) GetMobileOAuthData(_ context.Context, state string) (*MobileOAuthData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	data, ok := s.rows[state]
	if !ok {
		return nil, ErrAuthRequestNotFound
	}
	if data == nil {
		// Pending oauth_requests row exists, but the flow was started via the
		// web login path so no mobile data was stored.
		return nil, nil
	}
	copied := *data
	return &copied, nil
}

// seedWebFlowRow records a pending auth request for state with NO mobile data,
// matching what GetMobileOAuthData returns for a web-initiated flow.
func (s *fakeMobileAuthStore) seedWebFlowRow(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[state] = nil
}

// newErrorRedirectTestHandler builds an OAuthHandler whose store supports
// mobile OAuth data, suitable for driving HandleCallback error paths without
// any network I/O. devMode controls webErrorRedirect's relative-vs-PublicURL
// target.
func newErrorRedirectTestHandler(t *testing.T, devMode bool) (*OAuthHandler, *fakeMobileAuthStore) {
	t.Helper()

	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         devMode,
		AllowPrivateIPs: devMode,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=", // base64 encoded 32 bytes
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	store := newFakeMobileAuthStore()
	return NewOAuthHandler(client, store), store
}

var mobileCookieNames = []string{"mobile_redirect_uri", "mobile_redirect_binding", "oauth_csrf"}

// assertMobileCookiesCleared verifies every mobile cookie is expired via Set-Cookie.
func assertMobileCookiesCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	cleared := make(map[string]bool)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	for _, name := range mobileCookieNames {
		assert.True(t, cleared[name], "mobile cookie %q should be cleared (Set-Cookie with MaxAge<0)", name)
	}
}

// assertMobileCookiesUntouched verifies NO Set-Cookie header touches any
// mobile cookie (a forged callback must not kill an in-flight login).
func assertMobileCookiesUntouched(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		for _, name := range mobileCookieNames {
			assert.NotEqual(t, name, c.Name,
				"mobile cookie %q must not be touched on this path", name)
		}
	}
}

// TestHandleCallback_AuthServerErrorRedirects covers the ?error=... branch of
// HandleCallback end-to-end via httptest.
func TestHandleCallback_AuthServerErrorRedirects(t *testing.T) {
	const (
		// One of the allowlisted social.coves variants (see baseMobileRedirectURIs).
		mobileURI = "social.coves://oauth/callback"
		csrfToken = "test-csrf-token-value-1234567890"
	)

	validMobileCookies := func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "mobile_redirect_uri", Value: url.QueryEscape(mobileURI)})
		req.AddCookie(&http.Cookie{Name: "oauth_csrf", Value: csrfToken})
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_binding",
			Value: generateMobileRedirectBinding(csrfToken, mobileURI),
		})
	}
	seedMobileRow := func(state string) func(*fakeMobileAuthStore) {
		return func(s *fakeMobileAuthStore) {
			require.NoError(t, s.SaveMobileOAuthData(context.Background(), state, MobileOAuthData{
				CSRFToken:   csrfToken,
				RedirectURI: mobileURI,
			}))
		}
	}

	tests := []struct {
		name string
		// query string appended to /oauth/callback
		target string
		seed   func(*fakeMobileAuthStore)
		// cookies attached to the request (nil = zero cookies)
		cookies func(*http.Request)
		// wantAppRedirect: Location must target the server-stored mobile URI;
		// otherwise Location must be the web root and never the custom scheme.
		wantAppRedirect bool
		// expected query params in the Location URL (exact match per key)
		wantParams map[string]string
		// query params that must NOT appear in the Location URL
		wantAbsentParams []string
		// whether the mobile cookies must be cleared (vs left untouched)
		wantCookiesCleared bool
	}{
		{
			// (a) AS error with a valid pending mobile flow and full cookie set
			name:            "mobile flow with valid cookies redirects error into app",
			target:          "/oauth/callback?state=state-a&error=access_denied&error_description=User%20denied%20access",
			seed:            seedMobileRow("state-a"),
			cookies:         validMobileCookies,
			wantAppRedirect: true,
			wantParams: map[string]string{
				"error":             "access_denied",
				"error_description": "User denied access",
			},
			wantCookiesCleared: true,
		},
		{
			// (b) same but ZERO cookies - server-stored data alone qualifies the flow
			name:            "mobile flow with zero cookies still redirects error into app",
			target:          "/oauth/callback?state=state-b&error=access_denied&error_description=User%20denied%20access",
			seed:            seedMobileRow("state-b"),
			cookies:         nil,
			wantAppRedirect: true,
			wantParams: map[string]string{
				"error":             "access_denied",
				"error_description": "User denied access",
			},
			wantCookiesCleared: true,
		},
		{
			// (c) forged callback: no state param at all
			name:               "forged callback with missing state goes to web invalid_request without clearing cookies",
			target:             "/oauth/callback?error=access_denied",
			seed:               nil,
			cookies:            validMobileCookies,
			wantAppRedirect:    false,
			wantParams:         map[string]string{"oauth_error": "invalid_request"},
			wantCookiesCleared: false,
		},
		{
			// (c) forged callback: state present but no pending auth request row
			name:               "forged callback with unknown state goes to web invalid_request without clearing cookies",
			target:             "/oauth/callback?state=no-such-state&error=access_denied",
			seed:               nil,
			cookies:            validMobileCookies,
			wantAppRedirect:    false,
			wantParams:         map[string]string{"oauth_error": "invalid_request"},
			wantCookiesCleared: false,
		},
		{
			// (d) unknown error code is clamped and its description dropped
			name:               "unknown error code clamps to server_error and drops description",
			target:             "/oauth/callback?state=state-d&error=weird_code&error_description=attacker%20chosen%20text",
			seed:               seedMobileRow("state-d"),
			cookies:            nil,
			wantAppRedirect:    true,
			wantParams:         map[string]string{"error": "server_error"},
			wantAbsentParams:   []string{"error_description"},
			wantCookiesCleared: true,
		},
		{
			// (e) exactly one of the CSRF/binding cookie pair present: fail closed
			// to the web redirect. The handler's web fallback clears the cookies.
			name:   "partial binding cookie pair falls back to web redirect",
			target: "/oauth/callback?state=state-e&error=access_denied",
			seed:   seedMobileRow("state-e"),
			cookies: func(req *http.Request) {
				req.AddCookie(&http.Cookie{Name: "oauth_csrf", Value: csrfToken})
				// mobile_redirect_binding deliberately absent
			},
			wantAppRedirect:    false,
			wantParams:         map[string]string{"oauth_error": "access_denied"},
			wantCookiesCleared: true,
		},
		{
			// (g) empty error_description: the param is omitted entirely
			name:               "empty error_description omitted from app redirect",
			target:             "/oauth/callback?state=state-g&error=access_denied",
			seed:               seedMobileRow("state-g"),
			cookies:            nil,
			wantAppRedirect:    true,
			wantParams:         map[string]string{"error": "access_denied"},
			wantAbsentParams:   []string{"error_description"},
			wantCookiesCleared: true,
		},
		{
			// pending row WITHOUT mobile data = web flow: end on the web side
			name:   "web flow row redirects error to web root and clears mobile cookies",
			target: "/oauth/callback?state=state-w&error=access_denied",
			seed: func(s *fakeMobileAuthStore) {
				s.seedWebFlowRow("state-w")
			},
			cookies:            nil,
			wantAppRedirect:    false,
			wantParams:         map[string]string{"oauth_error": "access_denied"},
			wantCookiesCleared: true,
		},
		{
			// store lookup failure: cannot distinguish real from forged, so keep
			// cookies intact and degrade to a generic web error
			name:   "store lookup failure degrades to web server_error without clearing cookies",
			target: "/oauth/callback?state=state-x&error=access_denied",
			seed: func(s *fakeMobileAuthStore) {
				s.lookupErr = errors.New("database connection lost")
			},
			cookies:            validMobileCookies,
			wantAppRedirect:    false,
			wantParams:         map[string]string{"oauth_error": "server_error"},
			wantCookiesCleared: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, store := newErrorRedirectTestHandler(t, true) // dev mode: web redirects are relative
			if tt.seed != nil {
				tt.seed(store)
			}

			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			if tt.cookies != nil {
				tt.cookies(req)
			}
			rec := httptest.NewRecorder()

			handler.HandleCallback(rec, req)

			// Always a redirect - never a raw error page on the AS-error branch.
			require.Equal(t, http.StatusFound, rec.Code,
				"expected 302 redirect, got %d with body: %s", rec.Code, rec.Body.String())

			location := rec.Header().Get("Location")
			require.NotEmpty(t, location, "redirect must carry a Location header")

			locURL, err := url.Parse(location)
			require.NoError(t, err, "Location must be a parseable URL: %s", location)
			query := locURL.Query()

			if tt.wantAppRedirect {
				assert.True(t, strings.HasPrefix(location, mobileURI+"?"),
					"Location should target the server-stored app URI, got: %s", location)
			} else {
				assert.NotContains(t, location, "social.coves",
					"Location must never use the custom app scheme on this path, got: %s", location)
				assert.True(t, strings.HasPrefix(location, "/?"),
					"dev-mode web error redirect should target the relative root, got: %s", location)
			}

			for key, want := range tt.wantParams {
				assert.Equal(t, want, query.Get(key),
					"Location query param %q mismatch in %s", key, location)
			}
			for _, key := range tt.wantAbsentParams {
				assert.False(t, query.Has(key),
					"Location query param %q must be absent in %s", key, location)
			}

			if tt.wantCookiesCleared {
				assertMobileCookiesCleared(t, rec)
			} else {
				assertMobileCookiesUntouched(t, rec)
			}
		})
	}
}

// TestHandleCallback_ProcessCallbackFailureRedirects covers case (f): when the
// code+state exchange fails, the user gets a 302 redirect (into the app for
// mobile flows, to the web root otherwise) instead of the old raw 400 page.
//
// ProcessCallback is driven to failure without network I/O: indigo loads the
// auth request info by state from the CLIENT's store before any token
// exchange, and that store has no row for the state used here.
func TestHandleCallback_ProcessCallbackFailureRedirects(t *testing.T) {
	const (
		mobileURI = "social.coves://oauth/callback"
		csrfToken = "test-csrf-token-value-1234567890"
	)

	t.Run("web flow failure redirects to web root with server_error, not a 400 page", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		// No ?error param, so HandleCallback proceeds to ProcessCallback,
		// which fails to load auth request info for this unknown state.
		req := httptest.NewRequest(http.MethodGet,
			"/oauth/callback?state=unknown-state&code=some-code&iss=https://pds.example.com", nil)
		rec := httptest.NewRecorder()

		handler.HandleCallback(rec, req)

		require.Equal(t, http.StatusFound, rec.Code,
			"expected 302 redirect, got %d with body: %s", rec.Code, rec.Body.String())
		assert.Equal(t, "/?oauth_error=server_error", rec.Header().Get("Location"))
		assertMobileCookiesCleared(t, rec)
	})

	t.Run("mobile flow failure redirects into app with server_error", func(t *testing.T) {
		handler, store := newErrorRedirectTestHandler(t, true)

		// Seed the HANDLER's mobile store so the flow qualifies as mobile,
		// but leave the CLIENT's auth-request store empty so ProcessCallback
		// fails before any network I/O.
		require.NoError(t, store.SaveMobileOAuthData(context.Background(), "state-fail", MobileOAuthData{
			CSRFToken:   csrfToken,
			RedirectURI: mobileURI,
		}))

		req := httptest.NewRequest(http.MethodGet,
			"/oauth/callback?state=state-fail&code=some-code&iss=https://pds.example.com", nil)
		rec := httptest.NewRecorder()

		handler.HandleCallback(rec, req)

		require.Equal(t, http.StatusFound, rec.Code,
			"expected 302 redirect, got %d with body: %s", rec.Code, rec.Body.String())

		location := rec.Header().Get("Location")
		require.True(t, strings.HasPrefix(location, mobileURI+"?"),
			"Location should target the server-stored app URI, got: %s", location)

		locURL, err := url.Parse(location)
		require.NoError(t, err)
		query := locURL.Query()
		assert.Equal(t, "server_error", query.Get("error"))
		assert.Equal(t, "OAuth callback failed", query.Get("error_description"))
		assertMobileCookiesCleared(t, rec)
	})
}

// TestClampOAuthError verifies the error-code allowlist: known RFC 6749 / OIDC
// codes pass through with their description; anything else collapses to
// server_error with the (equally untrusted) description dropped.
func TestClampOAuthError(t *testing.T) {
	allowedCodes := []string{
		"access_denied",
		"invalid_request",
		"unauthorized_client",
		"server_error",
		"temporarily_unavailable",
		"invalid_scope",
		"unsupported_response_type",
		"login_required",
		"interaction_required",
	}

	for _, code := range allowedCodes {
		t.Run("allows "+code, func(t *testing.T) {
			gotCode, gotDesc := clampOAuthError(code, "some description")
			assert.Equal(t, code, gotCode)
			assert.Equal(t, "some description", gotDesc)
		})
	}

	unknownCodes := []string{
		"weird_code",
		"",
		"ACCESS_DENIED", // case-sensitive: not an exact allowlist match
		"<script>alert(1)</script>",
		"access_denied ", // trailing space
	}

	for _, code := range unknownCodes {
		t.Run("clamps unknown code "+strings.TrimSpace(code), func(t *testing.T) {
			gotCode, gotDesc := clampOAuthError(code, "attacker chosen description")
			assert.Equal(t, "server_error", gotCode)
			assert.Empty(t, gotDesc, "description must be dropped with unknown codes")
		})
	}
}

// TestWebErrorRedirect verifies webErrorRedirect's target construction:
// relative root in dev mode, PublicURL-prefixed in production, and clamped
// error codes in both.
func TestWebErrorRedirect(t *testing.T) {
	t.Run("dev mode redirects to relative root", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()
		handler.webErrorRedirect(rec, req, "access_denied")

		assert.Equal(t, http.StatusFound, rec.Code)
		assert.Equal(t, "/?oauth_error=access_denied", rec.Header().Get("Location"))
	})

	t.Run("production redirects to PublicURL root", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, false)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()
		handler.webErrorRedirect(rec, req, "access_denied")

		assert.Equal(t, http.StatusFound, rec.Code)
		assert.Equal(t, "https://coves.social/?oauth_error=access_denied", rec.Header().Get("Location"))
	})

	t.Run("unknown code is clamped to server_error", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()
		handler.webErrorRedirect(rec, req, "totally_made_up")

		assert.Equal(t, http.StatusFound, rec.Code)
		assert.Equal(t, "/?oauth_error=server_error", rec.Header().Get("Location"))
	})

	t.Run("does not touch cookies", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()
		handler.webErrorRedirect(rec, req, "access_denied")

		assertMobileCookiesUntouched(t, rec)
	})
}

// TestRedirectMobileError_Unit exercises redirectMobileError directly for the
// branches the handler-level tests cannot isolate: nil/empty server data,
// binding validation failure, and allowlist rejection. Returning false must
// never clear cookies.
func TestRedirectMobileError_Unit(t *testing.T) {
	const (
		mobileURI = "social.coves://oauth/callback"
		csrfToken = "test-csrf-token-value-1234567890"
	)

	t.Run("nil server data returns false without touching cookies", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()

		ok := handler.redirectMobileError(rec, req, nil, "access_denied", "desc")
		assert.False(t, ok)
		assertMobileCookiesUntouched(t, rec)
		assert.Empty(t, rec.Header().Get("Location"))
	})

	t.Run("empty server redirect URI returns false", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()

		ok := handler.redirectMobileError(rec, req,
			&MobileOAuthData{CSRFToken: csrfToken, RedirectURI: ""}, "access_denied", "")
		assert.False(t, ok)
		assertMobileCookiesUntouched(t, rec)
	})

	t.Run("binding validation failure returns false without touching cookies", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		// Both cookies present but the binding was computed for a DIFFERENT csrf token.
		req.AddCookie(&http.Cookie{Name: "oauth_csrf", Value: csrfToken})
		req.AddCookie(&http.Cookie{
			Name:  "mobile_redirect_binding",
			Value: generateMobileRedirectBinding("some-other-csrf-token", mobileURI),
		})
		rec := httptest.NewRecorder()

		ok := handler.redirectMobileError(rec, req,
			&MobileOAuthData{CSRFToken: csrfToken, RedirectURI: mobileURI}, "access_denied", "")
		assert.False(t, ok)
		assertMobileCookiesUntouched(t, rec)
	})

	t.Run("server-stored URI outside allowlist returns false", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		rec := httptest.NewRecorder()

		ok := handler.redirectMobileError(rec, req,
			&MobileOAuthData{CSRFToken: csrfToken, RedirectURI: "evil://steal"}, "access_denied", "")
		assert.False(t, ok)
		assertMobileCookiesUntouched(t, rec)
	})

	t.Run("undecodable mobile_redirect_uri cookie is tolerated", func(t *testing.T) {
		handler, _ := newErrorRedirectTestHandler(t, true)

		req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
		// Invalid percent-encoding; warned about but the server-stored URI wins.
		req.AddCookie(&http.Cookie{Name: "mobile_redirect_uri", Value: "%zz-not-decodable"})
		rec := httptest.NewRecorder()

		ok := handler.redirectMobileError(rec, req,
			&MobileOAuthData{CSRFToken: csrfToken, RedirectURI: mobileURI}, "access_denied", "")
		assert.True(t, ok)
		assert.Equal(t, http.StatusFound, rec.Code)
		assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), mobileURI+"?"))
		assertMobileCookiesCleared(t, rec)
	})
}
