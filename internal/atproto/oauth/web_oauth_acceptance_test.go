package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This fixture observes the state Indigo persists during PAR, which need not
// appear in the browser's authorization URL. Both client and handler share it.
type webFlowRecordingStore struct {
	*fakeMobileAuthStore
	states       []string
	bindings     map[string]WebOAuthData
	bindingError error
	claims       int
	readClaims   []int
}

func (s *webFlowRecordingStore) SaveAuthRequestInfo(ctx context.Context, data indigooauth.AuthRequestData) error {
	if err := s.ClientAuthStore.SaveAuthRequestInfo(ctx, data); err != nil {
		return err
	}
	s.states = append(s.states, data.State)
	s.seedWebFlowRow(data.State)
	return nil
}

func newWebFlowHandler(t *testing.T) (*OAuthHandler, *webFlowRecordingStore) {
	t.Helper()
	server, _ := newOAuthFlowServer(t)
	store := &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}
	client, err := NewOAuthClient(guardTestConfig("https://plc.example.invalid", true), NewMobileAwareStoreWrapper(store),
		resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: routes OAuth fixture requests to the in-process TLS listener
	require.NoError(t, err)
	routeGuardedClientToTLSServer(t, client.ClientApp.Resolver.Client, server)
	routeGuardedClientToTLSServer(t, client.ClientApp.Client, server)
	return NewOAuthHandler(client, store), store
}

func startWebLogin(t *testing.T, handler *OAuthHandler, redirect string) *httptest.ResponseRecorder {
	t.Helper()
	query := url.Values{"handle": {"https://example.com"}, "redirect": {redirect}}
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+query.Encode(), nil))
	require.Equal(t, http.StatusFound, response.Code, response.Body.String())
	require.Contains(t, response.Header().Get("Location"), "/authorize")
	return response
}

// Outer acceptance: the return target survives a real handler-to-PAR-to-handler
// round trip, and provider denial ends on the login form with a safe retry URL.
func TestWebOAuth_AuthorizationDeniedReturnsToLoginWithSavedDestination(t *testing.T) {
	handler, store := newWebFlowHandler(t)
	const destination = "/c/gardening?sort=new#comments"
	started := startWebLogin(t, handler, destination)
	require.Len(t, store.states, 1)
	callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+url.Values{
		"state": {store.states[0]}, "error": {"access_denied"}, "error_description": {"private provider details"},
		"redirect": {"//attacker.example/steal"},
	}.Encode(), nil)
	for _, cookie := range started.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.HandleCallback(response, callback)
	require.Equal(t, http.StatusFound, response.Code)
	target, err := url.Parse(response.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/login", target.Path, "web OAuth errors must reach the frontend login page")
	assert.Equal(t, "access_denied", target.Query().Get("error"))
	assert.Equal(t, destination, target.Query().Get("redirect"))
	assert.Empty(t, target.Query().Get("oauth_error"))
	assert.Empty(t, target.Query().Get("error_description"))
	assert.NotContains(t, response.Body.String(), "private provider details")
	for _, cookie := range response.Result().Cookies() {
		assert.NotEqual(t, "coves_session", cookie.Name)
	}
}

// Only a test double: persistence/expiry/atomicity are separately exercised
// against the real PostgreSQL store, not inferred from this implementation.
func (s *webFlowRecordingStore) SaveWebOAuthData(_ context.Context, state string, data WebOAuthData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindingError != nil {
		return s.bindingError
	}
	s.bindings[state] = data
	return nil
}

func (s *webFlowRecordingStore) ClaimWebOAuthData(_ context.Context, state, nonce string) (*WebOAuthData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.bindings[state]
	if !ok || nonce == "" || data.BrowserNonce != nonce || !data.ExpiresAt.After(time.Now()) {
		return nil, ErrAuthRequestNotFound
	}
	delete(s.bindings, state)
	s.claims++
	return &data, nil
}

func (s *webFlowRecordingStore) GetAuthRequestInfo(ctx context.Context, state string) (*indigooauth.AuthRequestData, error) {
	s.readClaims = append(s.readClaims, s.claims)
	return s.ClientAuthStore.GetAuthRequestInfo(ctx, state)
}
