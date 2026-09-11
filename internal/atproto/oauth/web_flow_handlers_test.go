package oauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func webBindingCookie(t *testing.T, response *httptest.ResponseRecorder, binding WebOAuthData) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Value == binding.BrowserNonce {
			return cookie
		}
	}
	t.Fatal("login must set the initiating browser's server-bound nonce cookie")
	return nil
}

func assertWebLoginError(t *testing.T, response *httptest.ResponseRecorder, code, redirect string) {
	t.Helper()
	require.Equal(t, http.StatusFound, response.Code, "terminal web OAuth errors must redirect to login")
	target, err := url.Parse(response.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/login", target.Path)
	assert.Equal(t, code, target.Query().Get("error"))
	assert.Equal(t, redirect, target.Query().Get("redirect"))
	assert.Empty(t, target.Query().Get("error_description"))
	for _, cookie := range response.Result().Cookies() {
		assert.NotEqual(t, "coves_session", cookie.Name)
	}
}

func TestWebOAuth_LoginPersistsSafeDestinationAndBrowserBinding(t *testing.T) {
	for _, entry := range []struct{ input, want string }{
		{"/c/gardening?sort=new#comments", "/c/gardening?sort=new#comments"},
		{"", "/"}, {"//attacker.example/steal", "/"}, {"https://attacker.example/steal", "/"},
		{"/\\attacker.example/steal", "/"}, {"/c/back\\slash", "/"}, {"/\tattacker.example", "/"},
		{"/c/\x00unsafe", "/"}, {"/c/\x7funsafe", "/"},
	} {
		t.Run(entry.input, func(t *testing.T) {
			handler, store := newWebFlowHandler(t)
			before := time.Now()
			response := startWebLogin(t, handler, entry.input)
			require.Len(t, store.states, 1)
			binding, ok := store.bindings[store.states[0]]
			require.True(t, ok, "return destination and browser proof must persist against the OAuth state before redirect")
			assert.Equal(t, entry.want, binding.ReturnURL)
			require.GreaterOrEqual(t, len(binding.BrowserNonce), 32, "browser nonce needs cryptographic entropy")
			assert.NotEqual(t, store.states[0], binding.BrowserNonce, "OAuth state alone must not authorize another browser")
			assert.WithinDuration(t, before.Add(10*time.Minute), binding.ExpiresAt, 5*time.Second)
			cookie := webBindingCookie(t, response, binding)
			assert.True(t, cookie.HttpOnly)
			assert.True(t, cookie.Secure, "production cookies must be TLS-only")
			assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
			assert.Equal(t, "/oauth", cookie.Path, "binding cookie must only travel to Go OAuth routes")
			assert.Equal(t, 600, cookie.MaxAge)
			assert.Empty(t, cookie.Domain, "binding cookie must be host-only")
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == "oauth_redirect" {
					assert.Less(t, cookie.MaxAge, 0, "a return destination must never be carried in an unbound browser cookie")
				}
			}
		})
	}
}

func TestWebOAuth_LoginBindingPersistenceFailureStopsAuthorization(t *testing.T) {
	handler, store := newWebFlowHandler(t)
	store.bindingError = errors.New("private database details")
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {"https://example.com"}, "redirect": {"/saved?sort=new#comments"}}.Encode(), nil))
	assertWebLoginError(t, response, "server_error", "/saved?sort=new#comments")
	assert.NotContains(t, response.Body.String(), "private database details")
	assert.NotContains(t, response.Header().Get("Location"), "private")
	for _, cookie := range response.Result().Cookies() {
		assert.Less(t, cookie.MaxAge, 0, "failed initialization must not emit an active transaction cookie")
	}
}

func TestWebOAuth_StartFailureReturnsKnownErrorPage(t *testing.T) {
	handler, _ := newWebFlowHandler(t)
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?handle=invalid-identifier&redirect=%2Fsaved", nil))
	assertWebLoginError(t, response, "invalid_request", "/saved")
	assert.NotContains(t, response.Body.String(), "invalid-identifier")
}

func TestWebOAuth_CallbackRejectsUnboundBrowserBeforeProcessing(t *testing.T) {
	for _, scenario := range []string{"missing", "mismatch", "expired", "stale attempt"} {
		t.Run(scenario, func(t *testing.T) {
			handler, store := newWebFlowHandler(t)
			started := startWebLogin(t, handler, "/saved?sort=new#comments")
			state := store.states[0]
			binding, ok := store.bindings[state]
			require.True(t, ok, "test requires a server-bound login")
			cookie := webBindingCookie(t, started, binding)
			callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{"state": {state}, "code": {"unused-code"}, "iss": {"https://example.com"}}.Encode(), nil)
			switch scenario {
			case "mismatch":
				cookie.Value = "another-browser-proof"
				callback.AddCookie(cookie)
			case "expired":
				binding.ExpiresAt = time.Now().Add(-time.Second)
				store.bindings[state] = binding
				callback.AddCookie(cookie)
			case "stale attempt":
				latest := startWebLogin(t, handler, "/latest")
				latestBinding, exists := store.bindings[store.states[1]]
				require.True(t, exists)
				latestCookie := webBindingCookie(t, latest, latestBinding)
				require.NotEqual(t, cookie.Value, latestCookie.Value)
				callback.AddCookie(latestCookie)
			}
			response := httptest.NewRecorder()
			handler.HandleCallback(response, callback)
			assertWebLoginError(t, response, "invalid_request", "/")
			assert.Zero(t, store.claims)
			_, err := store.GetAuthRequestInfo(t.Context(), state)
			require.NoError(t, err, "invalid browser proof must fail before Indigo consumes the request")
			for _, changed := range response.Result().Cookies() {
				assert.NotEqual(t, cookie.Name, changed.Name, "invalid callback cannot clear a different active transaction")
			}
		})
	}
}

func TestWebOAuth_BoundProviderErrorConsumesOnlyOnceAndClampsDetails(t *testing.T) {
	for _, providerError := range []string{"access_denied", "private-provider-error"} {
		t.Run(providerError, func(t *testing.T) {
			handler, store := newWebFlowHandler(t)
			started := startWebLogin(t, handler, "/saved?sort=new#comments")
			state := store.states[0]
			binding, ok := store.bindings[state]
			require.True(t, ok)
			cookie := webBindingCookie(t, started, binding)
			query := url.Values{"state": {state}, "error": {providerError}, "error_description": {"private-details"}}
			callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+query.Encode(), nil)
			callback.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.HandleCallback(response, callback)
			code := "access_denied"
			if strings.HasPrefix(providerError, "private") {
				code = "server_error"
			}
			assertWebLoginError(t, response, code, "/saved?sort=new#comments")
			assert.Equal(t, 1, store.claims)
			assert.NotContains(t, response.Header().Get("Location"), "private")
			replay := httptest.NewRecorder()
			handler.HandleCallback(replay, callback)
			assertWebLoginError(t, replay, "invalid_request", "/")
			assert.Equal(t, 1, store.claims)
			for _, changed := range replay.Result().Cookies() {
				assert.NotEqual(t, cookie.Name, changed.Name)
			}
		})
	}
}

func TestWebOAuth_CallbackRevalidatesStoredReturnDestination(t *testing.T) {
	for _, stored := range []string{"//attacker.example/steal", "/\\attacker.example/steal", "/c/\nunsafe", "/safe?sort=new#comments"} {
		t.Run(stored, func(t *testing.T) {
			handler, store := newWebFlowHandler(t)
			started := startWebLogin(t, handler, "/original")
			state := store.states[0]
			binding, ok := store.bindings[state]
			require.True(t, ok)
			cookie := webBindingCookie(t, started, binding)
			binding.ReturnURL = stored
			store.bindings[state] = binding
			callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{"state": {state}, "error": {"access_denied"}}.Encode(), nil)
			callback.AddCookie(cookie)
			callback.AddCookie(&http.Cookie{Name: "oauth_redirect", Value: url.QueryEscape("//attacker.example/planted")})
			response := httptest.NewRecorder()
			handler.HandleCallback(response, callback)
			want := "/"
			if stored == "/safe?sort=new#comments" {
				want = stored
			}
			assertWebLoginError(t, response, "access_denied", want)
			assert.Equal(t, 1, store.claims)
		})
	}
}

func TestWebOAuth_TokenExchangeFailureConsumesBindingAndPreservesRetryDestination(t *testing.T) {
	handler, store := newWebFlowHandler(t)
	started := startWebLogin(t, handler, "/saved?sort=new#comments")
	state := store.states[0]
	binding, ok := store.bindings[state]
	require.True(t, ok)
	cookie := webBindingCookie(t, started, binding)
	// The provider fixture has no token response, so ProcessCallback fails after
	// loading its saved PKCE data. The web binding must already be consumed.
	callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{"state": {state}, "code": {"provider-code"}, "iss": {"https://example.com"}}.Encode(), nil)
	callback.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.HandleCallback(response, callback)
	assertWebLoginError(t, response, "server_error", "/saved?sort=new#comments")
	assert.Equal(t, 1, store.claims)
	require.NotEmpty(t, store.readClaims, "test must reach Indigo's auth-request read")
	for _, claims := range store.readClaims {
		assert.Equal(t, 1, claims, "browser binding must be claimed BEFORE token exchange processing")
	}
}
