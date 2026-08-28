package oauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleWebCallback_PostLoginRedirectRejectsExternalTargets is the acceptance
// test for the post-login open redirect, at the READ site.
//
// HandleLogin accepts a `?redirect=` query parameter and stores it in the
// oauth_redirect cookie; handleWebCallback reads that cookie back and issues a
// 302 to it once the OAuth round trip completes. Guarding the value with
// nothing but `value[0] == '/'` is not a test for "local path": a leading `//`
// (or `/\`, which browsers normalize to `//`) is a scheme-relative URL, so the
// browser reads everything after it as an authority and navigates off-origin,
// carrying the user straight from a legitimate Coves login into an attacker's
// page with the freshly minted coves_session cookie already set.
//
// This test drives handleWebCallback directly with a planted cookie, which is
// exactly the attacker's position: the cookie is same-site, HttpOnly, and lives
// for five minutes, so it is planted by a prior `/oauth/login?redirect=...`
// visit and lies in wait for the victim's next real login.
//
// The cookie is transmitted percent-encoded (see TestPostLoginRedirectCookie),
// so the callback must unescape BEFORE validating — and a value that does not
// decode is refused rather than guessed at.
//
// coves:allow-host-literal: attacker.example is an RFC 2606 reserved name used
// as the off-origin redirect target; nothing resolves or dials it.
func TestHandleWebCallback_PostLoginRedirectRejectsExternalTargets(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		PLCURL:          testPLCURL,
		DevMode:         true, // no PublicURL prefix on the redirect target
		AllowPrivateIPs: true,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	handler := NewOAuthHandler(client, oauth.NewMemStore())

	accountDID, err := syntax.ParseDID("did:plc:test123456")
	require.NoError(t, err)

	tests := []struct {
		name string
		// plantedRedirect is the raw oauth_redirect cookie value the attacker
		// leaves behind before the victim logs in.
		plantedRedirect string
		// wantLocation is the only acceptable Location header.
		wantLocation string
		// skipCookieClearedAssertion drops the clearing check for a value
		// net/http refuses to parse at all: the handler never sees a cookie, so
		// it has none to clear, and asserting either way would only pin
		// stdlib cookie-parser internals.
		skipCookieClearedAssertion bool
	}{
		{
			name:            "scheme-relative target is not a local path",
			plantedRedirect: "//attacker.example",
			wantLocation:    "/",
		},
		{
			// Browsers normalize backslashes to forward slashes in the
			// authority position, so `/\host` navigates off-origin exactly as
			// `//host` does.
			name:                       "backslash authority is not a local path",
			plantedRedirect:            "/\\attacker.example",
			wantLocation:               "/",
			skipCookieClearedAssertion: true,
		},
		{
			name:            "triple slash authority is not a local path",
			plantedRedirect: "///attacker.example",
			wantLocation:    "/",
		},
		{
			// The write site percent-encodes, so this is the shape a real
			// login actually plants.
			name:            "encoded safe path is decoded and honored",
			plantedRedirect: url.QueryEscape("/delete-account"),
			wantLocation:    "/delete-account",
		},
		{
			// Decoding must happen BEFORE validation, never instead of it.
			name:            "encoded scheme-relative target is decoded then refused",
			plantedRedirect: url.QueryEscape("//attacker.example"),
			wantLocation:    "/",
		},
		{
			name:            "undecodable value is refused, not guessed at",
			plantedRedirect: "%zz",
			wantLocation:    "/",
		},
		{
			// Unescaping is a no-op here, so an unencoded legacy cookie still
			// works.
			name:            "unencoded safe path is still honored",
			plantedRedirect: "/delete-account",
			wantLocation:    "/delete-account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
			// Set the header directly rather than via req.AddCookie: some of
			// these values carry bytes net/http will not serialize.
			req.Header.Set("Cookie", "oauth_redirect="+tt.plantedRedirect)
			rec := httptest.NewRecorder()

			sessData := &oauth.ClientSessionData{
				AccountDID: accountDID,
				SessionID:  "test-session-open-redirect",
			}

			handler.handleWebCallback(rec, req, sessData)

			require.Equal(t, http.StatusFound, rec.Code, "web callback should complete with a 302")
			assert.Equal(t, tt.wantLocation, rec.Header().Get("Location"),
				"planted oauth_redirect %q must not steer the post-login redirect off-origin",
				tt.plantedRedirect)

			if !tt.skipCookieClearedAssertion {
				assert.True(t, redirectCookieCleared(rec),
					"the one-shot oauth_redirect cookie must be expired on the way out")
			}
		})
	}
}

// TestHandleLogin_PostLoginRedirectCookie is the acceptance test at the WRITE
// site, and it covers the half the read site cannot see: what HandleLogin does
// when it declines to store a target.
//
// Declining is not enough. The cookie lives for five minutes and survives
// across login attempts, so a stale oauth_redirect planted by an earlier visit
// is still sitting in the browser when the next login starts. If HandleLogin
// only ever ADDS a validated cookie, an attacker plants once and every
// subsequent login inherits the stale target. Whenever HandleLogin does not set
// a fresh validated cookie — no `?redirect=` at all, or one it refused — it
// must actively expire whatever is there.
//
// The test reaches the cookie block for real by driving HandleLogin all the way
// through StartAuthFlow against the local OAuth-server fixture, using the same
// seams as client_guard_test.go. Asserting the redirect lands on /authorize is
// how we know the cookie block was reached rather than the error return above
// it.
//
// coves:allow-host-literal: attacker.example is an RFC 2606 reserved name used
// as the rejected off-origin target; nothing resolves or dials it.
func TestHandleLogin_PostLoginRedirectCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// redirectParam is the raw ?redirect= value; empty means the parameter
		// is absent entirely.
		redirectParam string
		// wantStoredValue is the expected cookie value when a fresh cookie is
		// stored; empty means the handler must instead expire the cookie.
		wantStoredValue string
	}{
		{
			name:            "safe target is stored percent-encoded",
			redirectParam:   "/delete-account",
			wantStoredValue: url.QueryEscape("/delete-account"),
		},
		{
			name:          "refused target expires any stale cookie",
			redirectParam: "//attacker.example",
		},
		{
			name: "absent parameter expires any stale cookie",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, _ := newOAuthFlowServer(t)
			client, err := NewOAuthClient(
				guardTestConfig("https://plc.example.invalid", true), oauth.NewMemStore(),
				resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: the seam's DNS answer, routed to the TLS test listener
			require.NoError(t, err, "the test's own config must build a client")

			// Both clients: the resolver fetches the authorization-server
			// metadata, the app client posts the pushed authorization request.
			routeGuardedClientToTLSServer(t, client.ClientApp.Resolver.Client, server)
			routeGuardedClientToTLSServer(t, client.ClientApp.Client, server)

			handler := NewOAuthHandler(client, oauth.NewMemStore())

			query := url.Values{"handle": {"https://example.com"}}
			if tt.redirectParam != "" {
				query.Set("redirect", tt.redirectParam)
			}
			req := httptest.NewRequest(http.MethodGet, "/oauth/login?"+query.Encode(), nil)
			rec := httptest.NewRecorder()

			handler.HandleLogin(rec, req)

			// Prove we got past StartAuthFlow: the cookie block sits after it,
			// so an error return would make every cookie assertion below
			// vacuous.
			require.Equal(t, http.StatusFound, rec.Code,
				"HandleLogin must complete the auth flow, body: %s", rec.Body.String())
			require.Contains(t, rec.Header().Get("Location"), "/authorize",
				"the redirect must be the authorization request, got %q", rec.Header().Get("Location"))

			stored, expired := partitionRedirectCookies(rec)

			if tt.wantStoredValue == "" {
				assert.Empty(t, stored,
					"a target the handler refused must not be stored in any form")
				require.Len(t, expired, 1,
					"HandleLogin must expire the oauth_redirect cookie whenever it does not set a "+
						"fresh validated one — otherwise a target planted by an earlier visit "+
						"survives into this login")
				return
			}

			require.Len(t, stored, 1, "exactly one oauth_redirect cookie must be set")
			assert.Empty(t, expired, "a stored target must not also be expired in the same response")

			cookie := stored[0]
			assert.Equal(t, tt.wantStoredValue, cookie.Value,
				"the target must be transmitted percent-encoded")
			assert.True(t, cookie.Secure, "the cookie must be TLS-only outside dev mode")
			assert.True(t, cookie.HttpOnly, "the redirect target must not be readable from JS")
			assert.Equal(t, 300, cookie.MaxAge,
				"the cookie should outlive the OAuth round trip and no more")
		})
	}
}

// partitionRedirectCookies splits the response's oauth_redirect cookies into
// those carrying a value and those expiring the cookie. httptest records
// Set-Cookie verbatim, so MaxAge -1 goes out as Max-Age=0 and parses back to -1.
func partitionRedirectCookies(rec *httptest.ResponseRecorder) (stored, expired []*http.Cookie) {
	for _, c := range rec.Result().Cookies() {
		if c.Name != "oauth_redirect" {
			continue
		}
		if c.MaxAge < 0 || c.Value == "" {
			expired = append(expired, c)
			continue
		}
		stored = append(stored, c)
	}
	return stored, expired
}

// redirectCookieCleared reports whether the response expires the oauth_redirect
// cookie.
func redirectCookieCleared(rec *httptest.ResponseRecorder) bool {
	_, expired := partitionRedirectCookies(rec)
	return len(expired) > 0
}
