package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	coreerrors "Coves/internal/core/errors"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const completionDID = "did:plc:abcdefghijklmnopqrstuvwx"

type completionDirectory struct {
	identity.Directory
	calls            int
	failVerification bool
}

func (d *completionDirectory) LookupDID(ctx context.Context, did syntax.DID) (*identity.Identity, error) {
	d.calls++
	if d.failVerification && d.calls > 1 {
		return nil, errors.New("private directory details")
	}
	return d.Directory.LookupDID(ctx, did)
}

// T0 provider fixture: exercise Indigo's real token exchange and session store,
// with all HTTP traffic confined to the in-process TLS listener.
func newCompletionHandler(t *testing.T) (*OAuthHandler, *webFlowRecordingStore, *completionDirectory, *string) {
	t.Helper()
	server, _ := newOAuthFlowServer(t)
	original := server.Config.Handler
	registeredCallback := ""
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"DPoP","expires_in":3600,"scope":"atproto blob:*/*","sub":%q}`, completionDID)
		case "/.well-known/oauth-protected-resource":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resource":"https://example.com","authorization_servers":["https://example.com"]}`))
		case "/par":
			if !assert.NoError(t, r.ParseForm()) {
				http.Error(w, "invalid fixture PAR form", http.StatusBadRequest)
				return
			}
			registeredCallback = r.Form.Get("redirect_uri")
			original.ServeHTTP(w, r)
		default:
			original.ServeHTTP(w, r)
		}
	})
	store := &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}
	client, err := NewOAuthClient(guardTestConfig("https://plc.example.invalid", true), NewMobileAwareStoreWrapper(store), resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: in-process TLS fixture
	require.NoError(t, err)
	routeGuardedClientToTLSServer(t, client.ClientApp.Resolver.Client, server)
	routeGuardedClientToTLSServer(t, client.ClientApp.Client, server)
	directory := identity.NewMockDirectory()
	directory.Insert(identity.Identity{DID: syntax.DID(completionDID), Handle: syntax.Handle("alice.example.invalid"), Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: "https://example.com"}}})
	wrapped := &completionDirectory{Directory: directory}
	client.ClientApp.Dir = wrapped
	return NewOAuthHandler(client, store), store, wrapped, &registeredCallback
}

func completeLogin(t *testing.T, handler *OAuthHandler, state string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{"state": {state}, "code": {"fixture-code"}, "iss": {"https://example.com"}, "redirect": {"//attacker.example/steal"}}.Encode(), nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.HandleCallback(response, request)
	return response
}

func TestWebOAuth_SuccessfulCallbackUsesBoundDestination(t *testing.T) {
	handler, store, directory, _ := newCompletionHandler(t)
	const destination = "/c/orchids?sort=top#reply"
	started := startWebLogin(t, handler, destination)
	state := store.states[0]
	cookies := append(started.Result().Cookies(), &http.Cookie{Name: "oauth_redirect", Value: url.QueryEscape("//attacker.example/planted")})
	response := completeLogin(t, handler, state, cookies)
	require.Equal(t, http.StatusFound, response.Code, response.Body.String())
	require.Equal(t, destination, response.Header().Get("Location"))
	assert.Equal(t, 1, store.claims)
	assert.Equal(t, 2, directory.calls, "both Indigo and handler must verify the identity")
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "coves_session" {
			session = cookie
		}
	}
	require.NotNil(t, session)
	unsealed, err := handler.client.UnsealSession(session.Value)
	require.NoError(t, err)
	assert.Equal(t, completionDID, unsealed.DID)
	assert.Equal(t, state, unsealed.SessionID)
	replay := completeLogin(t, handler, state, cookies)
	assertWebLoginError(t, replay, "invalid_request", "/")
}

func TestWebOAuth_TerminalCompletionFailureReturnsSafeRetry(t *testing.T) {
	for _, failure := range []string{"DID lookup", "handle mismatch", "reinstatement", "sealing"} {
		t.Run(failure, func(t *testing.T) {
			handler, store, directory, _ := newCompletionHandler(t)
			const destination = "/c/orchids?sort=top#reply"
			started := startWebLogin(t, handler, destination)
			switch failure {
			case "DID lookup":
				directory.failVerification = true
			case "handle mismatch":
				directory.Directory.(*identity.MockDirectory).Insert(identity.Identity{DID: syntax.DID(completionDID), Handle: syntax.Handle("handle.invalid"), Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: "https://example.com"}}})
			case "reinstatement":
				handler.userIndexer = &stubIndexer{err: fmt.Errorf("private restoration details: %w", coreerrors.ErrReinstateFailed)}
			case "sealing":
				handler.client.SealSecret = nil
			}
			response := completeLogin(t, handler, store.states[0], started.Result().Cookies())
			require.Equal(t, 2, directory.calls, "failure must occur after successful token exchange")
			assertWebLoginError(t, response, "server_error", destination)
			assert.NotContains(t, response.Body.String(), "private")
		})
	}
}

func TestWebOAuth_ExternalRedirectURIChangesNeitherRegisteredCallbackNorReturn(t *testing.T) {
	handler, store, _, registered := newCompletionHandler(t)
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {"https://example.com"}, "redirect": {"/saved#reply"}, "redirect_uri": {"https://attacker.example/callback"}}.Encode(), nil))
	require.Equal(t, http.StatusFound, response.Code)
	assert.Equal(t, handler.client.ClientApp.Config.CallbackURL, *registered)
	completed := completeLogin(t, handler, store.states[0], response.Result().Cookies())
	assert.Equal(t, "/saved#reply", completed.Header().Get("Location"))
}

func TestWebOAuth_MobileFallbackCannotMintUnboundWebSession(t *testing.T) {
	for _, scenario := range []string{"missing cookies", "bad binding", "wrong server CSRF", "valid mobile", "provider denied"} {
		t.Run(scenario, func(t *testing.T) {
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
			if scenario == "bad binding" {
				for _, cookie := range cookies {
					if cookie.Name == "mobile_redirect_binding" {
						cookie.Value = "wrong-binding"
					}
				}
			}
			if scenario == "wrong server CSRF" {
				for _, cookie := range cookies {
					if cookie.Name == "oauth_csrf" {
						cookie.Value = "different-csrf"
					}
					if cookie.Name == "mobile_redirect_binding" {
						cookie.Value = generateMobileRedirectBinding("different-csrf", redirect)
					}
				}
			}
			if scenario == "missing cookies" {
				cookies = nil
			}

			response := httptest.NewRecorder()
			if scenario == "provider denied" {
				handler.HandleCallback(response, httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"private provider details"}}.Encode(), nil))
			} else {
				response = completeLogin(t, handler, state, cookies)
			}
			for _, cookie := range response.Result().Cookies() {
				assert.NotEqual(t, "coves_session", cookie.Name)
			}
			assert.Zero(t, store.claims)
			if scenario != "valid mobile" && scenario != "provider denied" {
				assertWebLoginError(t, response, "invalid_request", "/")
				return
			}
			require.Equal(t, http.StatusFound, response.Code, response.Body.String())
			target, err := url.Parse(response.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, "mobile.example.invalid", target.Host)
			assert.Equal(t, "/app/oauth/callback", target.Path)
			if scenario == "provider denied" {
				assert.Equal(t, "access_denied", target.Query().Get("error"))
				assert.Empty(t, target.Query().Get("token"))
				return
			}
			unsealed, err := handler.client.UnsealSession(target.Query().Get("token"))
			require.NoError(t, err)
			assert.Equal(t, state, unsealed.SessionID)
			assert.Equal(t, completionDID, unsealed.DID)
		})
	}
}
