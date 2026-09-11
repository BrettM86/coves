package oauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebOAuth_DevLoginMalformedPublicURLKeepsDestinationAndLogs(t *testing.T) {
	for name, publicURL := range map[string]string{"unparseable": "://coves.example", "wrong scheme": "ftp://coves.example", "no host": "http:///oauth"} {
		t.Run(name, func(t *testing.T) {
			handler, store := newWebFlowHandler(t)
			handler.client.Config.DevMode = true
			handler.client.Config.PublicURL = publicURL
			output := captureOAuthDiagnostics(t, true)
			response := httptest.NewRecorder()
			handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {"https://example.com"}, "redirect": {"/saved?sort=new#reply"}}.Encode(), nil))
			assertWebLoginError(t, response, "server_error", "/saved?sort=new#reply")
			assert.Empty(t, store.states, "misconfigured callback origin must fail before creating a transaction")
			assert.Empty(t, response.Result().Cookies())
			assertOAuthDiagnostic(t, output, "ERROR", "login_start")
			assert.False(t, strings.Contains(output.String(), "coves.example"), "diagnostic must not echo configuration values")
		})
	}
}

func TestWebOAuth_LoginHostMismatchWarnsWithoutHostValue(t *testing.T) {
	t.Run("dev redirects to configured origin", func(t *testing.T) {
		handler, store, _, _ := newCompletionHandler(t)
		handler.client.Config.DevMode = true
		handler.client.Config.PublicURL = "http://127.0.0.1:8080" // coves:allow-host-literal: configured local browser origin, no network request
		output := captureOAuthDiagnostics(t, true)
		response := httptest.NewRecorder()
		handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "http://localhost:8080/oauth/login?handle=https%3A%2F%2Fexample.com&redirect=%2Fsaved", nil)) // coves:allow-host-literal: request host input, never dialled
		require.Equal(t, http.StatusFound, response.Code)
		assert.Empty(t, store.states)
		assertOAuthDiagnostic(t, output, "WARN", "login_start", "host")
		assert.False(t, strings.Contains(output.String(), "localhost"), "diagnostic must not log the raw Host header")
	})
	t.Run("production continues but warns", func(t *testing.T) {
		handler, store := newWebFlowHandler(t)
		require.Equal(t, "https://coves.example", handler.client.Config.PublicURL)
		output := captureOAuthDiagnostics(t, true)
		startWebLogin(t, handler, "/saved")
		require.Len(t, store.states, 1, "production login must proceed despite a host mismatch")
		assertOAuthDiagnostic(t, output, "WARN", "login_start", "host")
		assert.False(t, strings.Contains(output.String(), "example.com"), "diagnostic must not log the raw Host header")
	})
	t.Run("matching host is silent", func(t *testing.T) {
		handler, _ := newWebFlowHandler(t)
		output := captureOAuthDiagnostics(t, true)
		query := url.Values{"handle": {"https://example.com"}, "redirect": {"/saved"}}
		response := httptest.NewRecorder()
		handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, handler.client.Config.PublicURL+"/oauth/login?"+query.Encode(), nil))
		require.Equal(t, http.StatusFound, response.Code, response.Body.String())
		assert.False(t, strings.Contains(output.String(), `"category":"host"`))
	})
}

func TestWebOAuth_BindingCookieScopedToOAuthPathOnSetAndExpire(t *testing.T) {
	handler, store, _, _ := newCompletionHandler(t)
	started := startWebLogin(t, handler, "/saved")
	state := store.states[0]
	set := webBindingCookie(t, started, store.bindings[state])
	assert.Equal(t, "/oauth", set.Path, "binding cookie must only travel to Go OAuth routes")
	completed := completeLogin(t, handler, state, started.Result().Cookies())
	require.Equal(t, http.StatusFound, completed.Code, completed.Body.String())
	var expired *http.Cookie
	for _, cookie := range completed.Result().Cookies() {
		if cookie.Name == webOAuthCookieName {
			expired = cookie
		}
	}
	require.NotNil(t, expired, "successful callback must expire the binding cookie")
	assert.Equal(t, "/oauth", expired.Path, "expiry must match the set path or the browser keeps the cookie")
	assert.Less(t, expired.MaxAge, 0)
	assert.Empty(t, expired.Value)
	assert.True(t, expired.HttpOnly)
	assert.True(t, expired.Secure)
	assert.Equal(t, http.SameSiteLaxMode, expired.SameSite)
}

func TestWebOAuth_UnboundWebCompletionWarnsAndDeletesPersistedSession(t *testing.T) {
	handler, store, _, _ := newCompletionHandler(t)
	handler = NewOAuthHandler(handler.client, NewMobileAwareStoreWrapper(store))
	const redirect = "https://mobile.example.invalid/app/oauth/callback"
	handler.allowedRedirectURIs[redirect] = true
	started := httptest.NewRecorder()
	handler.HandleMobileLogin(started, httptest.NewRequest(http.MethodGet, "/oauth/mobile/login?"+url.Values{"handle": {"https://example.com"}, "redirect_uri": {redirect}}.Encode(), nil))
	require.Equal(t, http.StatusFound, started.Code, started.Body.String())
	require.Len(t, store.states, 1)
	state := store.states[0]
	cookies := started.Result().Cookies()
	for _, cookie := range cookies {
		if cookie.Name == "mobile_redirect_binding" {
			cookie.Value = "wrong-binding"
		}
	}
	output := captureOAuthDiagnostics(t, true)
	response := completeLogin(t, handler, state, cookies)
	assertWebLoginError(t, response, "invalid_request", "/")
	assertOAuthDiagnostic(t, output, "WARN", "callback", "binding")
	assertNoPrivateDiagnostics(t, output, state)
	_, err := store.GetSession(t.Context(), syntax.DID(completionDID), state)
	require.Error(t, err, "rejected unbound completion must not leave Indigo's persisted tokens behind")
}
