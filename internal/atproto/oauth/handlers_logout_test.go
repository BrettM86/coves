package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLogoutTestHandler mirrors the Postgres store's missing-row sentinel; the
// raw in-memory store returns an unclassified error for a missing session.
func newLogoutTestHandler(t *testing.T) (*OAuthHandler, func(string) string) {
	t.Helper()
	return newLogoutTestHandlerWithStore(t, &logoutFaultStore{MemStore: oauth.NewMemStore()})
}

func newLogoutTestHandlerWithStore(t *testing.T, store oauth.ClientAuthStore) (*OAuthHandler, func(string) string) {
	t.Helper()
	client, err := NewOAuthClient(&OAuthConfig{
		PublicURL:  "https://coves.example",
		Scopes:     []string{"atproto"},
		PLCURL:     testPLCURL,
		SealSecret: "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}, store)
	require.NoError(t, err)
	return NewOAuthHandler(client, store), func(sessionID string) string {
		t.Helper()
		privateKey, err := atcrypto.GeneratePrivateKeyP256()
		require.NoError(t, err)
		did, err := syntax.ParseDID("did:plc:logouttestaccount")
		require.NoError(t, err)
		// No revocation endpoint: exercise real session deletion without network calls.
		require.NoError(t, store.SaveSession(context.Background(), oauth.ClientSessionData{
			AccountDID:              did,
			SessionID:               sessionID,
			DPoPPrivateKeyMultibase: privateKey.Multibase(),
		}))
		token, err := client.SealSession(did.String(), sessionID, time.Hour)
		require.NoError(t, err)
		return token
	}
}

func TestHandleLogoutRevokesSession(t *testing.T) {
	t.Parallel()
	for _, credential := range []string{"bearer", "cookie"} {
		t.Run(credential, func(t *testing.T) {
			t.Parallel()
			handler, saveSession := newLogoutTestHandler(t)
			token := saveSession("logging-out")
			otherToken := saveSession("other-device")
			request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
			if credential == "bearer" {
				request.Header.Set("Authorization", "Bearer "+token)
			} else {
				request.AddCookie(&http.Cookie{Name: "coves_session", Value: token})
			}
			_, err := handler.GetSessionFromRequest(request)
			require.NoError(t, err, "credential must authenticate before logout")

			response := httptest.NewRecorder()
			handler.HandleLogout(response, request)
			assertLogoutSuccess(t, response)
			_, err = handler.GetSessionFromRequest(request)
			assert.Error(t, err, "replaying a signed-out token must no longer authenticate")

			otherRequest := httptest.NewRequest(http.MethodGet, "/oauth/session", nil)
			otherRequest.Header.Set("Authorization", "Bearer "+otherToken)
			otherSession, err := handler.GetSessionFromRequest(otherRequest)
			require.NoError(t, err, "logout must preserve another session for the same account")
			assert.Equal(t, "other-device", otherSession.SessionID)

			response = httptest.NewRecorder()
			handler.HandleLogout(response, request)
			assertLogoutSuccess(t, response)
		})
	}
}

func TestHandleLogoutInvalidCredentials(t *testing.T) {
	t.Parallel()
	for _, credential := range []string{"missing", "invalid bearer", "invalid cookie", "expired bearer", "invalid DID"} {
		t.Run(credential, func(t *testing.T) {
			t.Parallel()
			handler, saveSession := newLogoutTestHandler(t)
			token := saveSession("retained-session")
			request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
			switch credential {
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer invalid-token")
			case "invalid cookie":
				request.AddCookie(&http.Cookie{Name: "coves_session", Value: "invalid-token"})
			case "expired bearer", "invalid DID":
				did, ttl := "did:plc:logouttestaccount", -time.Hour
				if credential == "invalid DID" {
					did, ttl = "not-a-did", time.Hour
				}
				invalidToken, err := handler.client.SealSession(did, "retained-session", ttl)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+invalidToken)
			}
			response := httptest.NewRecorder()
			handler.HandleLogout(response, request)
			assertLogoutSuccess(t, response)
			retainedRequest := httptest.NewRequest(http.MethodGet, "/oauth/session", nil)
			retainedRequest.Header.Set("Authorization", "Bearer "+token)
			_, err := handler.GetSessionFromRequest(retainedRequest)
			require.NoError(t, err, "invalid credentials must not revoke an existing session")
		})
	}
}

func assertLogoutSuccess(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"logged_out"}`, response.Body.String())
	cookies := response.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "coves_session", cookies[0].Name)
	assert.Empty(t, cookies[0].Value)
	assert.Equal(t, "/", cookies[0].Path)
	assert.Equal(t, -1, cookies[0].MaxAge)
}

// Preserve the real in-memory session behavior, but match Postgres's missing-row
// sentinel and inject storage outages independently of the underlying session.
type logoutFaultStore struct {
	*oauth.MemStore
	lookupError          error
	deleteError          error
	transientDeleteError bool
	wrapNotFound         bool
}

func (s *logoutFaultStore) missingSession() error {
	if s.wrapNotFound {
		return fmt.Errorf("store operation: %w", ErrSessionNotFound)
	}
	return ErrSessionNotFound
}

func (s *logoutFaultStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	if s.lookupError != nil {
		return nil, s.lookupError
	}
	session, err := s.MemStore.GetSession(ctx, did, sessionID)
	if err != nil {
		return nil, s.missingSession()
	}
	return session, nil
}

func (s *logoutFaultStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	if s.deleteError != nil {
		err := s.deleteError
		if s.transientDeleteError {
			s.deleteError = nil
		}
		return err
	}
	if _, err := s.MemStore.GetSession(ctx, did, sessionID); err != nil {
		return s.missingSession()
	}
	return s.MemStore.DeleteSession(ctx, did, sessionID)
}

func TestHandleLogoutStorageFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                              string
		corruptKey, lookupFailure, deleteFailure, transientDelete, absent bool
		wantFailure                                                       bool
	}{
		{name: "corrupt DPoP key", corruptKey: true},
		{name: "lookup outage", lookupFailure: true, wantFailure: true},
		{name: "lookup and deletion outage", lookupFailure: true, deleteFailure: true, wantFailure: true},
		{name: "persistent deletion outage", deleteFailure: true, wantFailure: true},
		{name: "transient deletion outage", deleteFailure: true, transientDelete: true, wantFailure: true},
		{name: "wrapped missing session", absent: true},
		{name: "lookup outage on missing session", absent: true, lookupFailure: true, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := &logoutFaultStore{MemStore: oauth.NewMemStore(), wrapNotFound: true}
			handler, saveSession := newLogoutTestHandlerWithStore(t, store)
			token := saveSession("logging-out")
			request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			session, err := handler.GetSessionFromRequest(request)
			require.NoError(t, err)
			if test.corruptKey {
				session.DPoPPrivateKeyMultibase = "invalid-key"
				require.NoError(t, store.SaveSession(context.Background(), *session))
			}
			if test.absent {
				require.NoError(t, store.MemStore.DeleteSession(context.Background(), session.AccountDID, session.SessionID))
			}
			if test.lookupFailure {
				store.lookupError = errors.New("storage read unavailable")
			}
			if test.deleteFailure {
				store.deleteError = errors.New("storage delete unavailable")
			}
			store.transientDeleteError = test.transientDelete
			response := httptest.NewRecorder()
			handler.HandleLogout(response, request)
			if test.wantFailure {
				assert.Equal(t, http.StatusServiceUnavailable, response.Code)
				assert.NotContains(t, response.Body.String(), "logged_out")
				assert.NotContains(t, response.Body.String(), "storage", "internal detail must not leak")
				assert.Empty(t, response.Header().Values("Set-Cookie"), "retain credentials so logout can be retried")
			} else {
				assertLogoutSuccess(t, response)
			}
			store.lookupError = nil
			store.deleteError = nil
			_, err = handler.GetSessionFromRequest(request)
			if test.wantFailure && !test.absent {
				require.NoError(t, err, "failed revocation leaves token usable")
			} else {
				require.Error(t, err, "successful logout must invalidate replay")
			}
		})
	}
}

func TestLogoutAuthorizationPrecedence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, header               string
		staleCookie, invalidHeader bool
		missingBearer              bool
	}{
		{name: "bearer overrides live cookie", header: "Bearer "},
		{name: "bearer overrides stale cookie", header: "Bearer ", staleCookie: true},
		{name: "case insensitive scheme", header: "bEaReR "},
		{name: "whitespace around token", header: "Bearer   "},
		{name: "invalid bearer with live cookie", header: "Bearer invalid", invalidHeader: true},
		{name: "malformed header with live cookie", header: "Basic invalid", invalidHeader: true},
		{name: "empty bearer with live cookie", header: "Bearer ", invalidHeader: true},
		{name: "missing bearer session with live cookie", header: "Bearer ", missingBearer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, saveSession := newLogoutTestHandler(t)
			cookieToken := saveSession("cookie-session")
			bearerToken := saveSession("bearer-session")
			request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
			cookieValue := cookieToken
			if test.staleCookie {
				var err error
				cookieValue, err = handler.client.SealSession("did:plc:logouttestaccount", "missing-session", time.Hour)
				require.NoError(t, err)
			}
			request.AddCookie(&http.Cookie{Name: "coves_session", Value: cookieValue})
			header := test.header
			if !test.invalidHeader {
				selectedToken := bearerToken
				if test.missingBearer {
					var err error
					selectedToken, err = handler.client.SealSession("did:plc:logouttestaccount", "missing-session", time.Hour)
					require.NoError(t, err)
				}
				header += selectedToken + "  "
			}
			request.Header.Set("Authorization", header)
			session, err := handler.GetSessionFromRequest(request)
			if test.invalidHeader || test.missingBearer {
				assert.Error(t, err, "invalid headers must not fall back to the cookie")
			} else if assert.NoError(t, err) {
				assert.Equal(t, "bearer-session", session.SessionID)
			}
			response := httptest.NewRecorder()
			handler.HandleLogout(response, request)
			assertLogoutSuccess(t, response)
			for _, token := range []string{cookieToken, bearerToken} {
				replay := httptest.NewRequest(http.MethodGet, "/oauth/session", nil)
				replay.Header.Set("Authorization", "Bearer "+token)
				_, err := handler.GetSessionFromRequest(replay)
				if token == bearerToken && !test.invalidHeader && !test.missingBearer {
					assert.Error(t, err, "selected bearer must be revoked")
				} else {
					assert.NoError(t, err, "unselected sessions must remain valid")
				}
			}
		})
	}
}
