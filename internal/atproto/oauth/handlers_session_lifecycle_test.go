package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleRefreshSessionLifecycle(t *testing.T) {
	for _, testcase := range []struct {
		name         string
		status       int
		body         string
		storeFailure string
		terminal     bool
	}{
		{name: "revoked_grant", status: 400, body: `{"error":"invalid_grant"}`, terminal: true},
		{name: "authorization_server_unavailable", status: 503, body: `{"error":"temporarily_unavailable"}`},
		{name: "malformed_authorization_response", status: 400, body: `{`},
		{name: "oversized_authorization_response", status: 400, body: `{"error":"invalid_grant","detail":"` + strings.Repeat("x", 65*1024) + `"}`},
		{name: "database_read_failure", status: 200, storeFailure: "get"},
		{name: "database_save_failure", status: 200, storeFailure: "save"},
		{name: "invalidation_failure", status: 400, body: `{"error":"invalid_grant"}`, storeFailure: "delete"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			store := &refreshFailureStore{ClientAuthStore: indigooauth.NewMemStore()}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testcase.status)
				body := testcase.body
				if testcase.status == 200 {
					body = `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"DPoP"}`
				}
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			key, err := atcrypto.GeneratePrivateKeyP256()
			require.NoError(t, err)
			data := indigooauth.ClientSessionData{AccountDID: syntax.DID("did:plc:refreshlifecycle"), SessionID: "browser", HostURL: server.URL, AuthServerURL: server.URL, AuthServerTokenEndpoint: server.URL + "/token", AccessToken: "initial-access", RefreshToken: "initial-refresh", DPoPPrivateKeyMultibase: key.Multibase()}
			require.NoError(t, store.SaveSession(context.Background(), data))
			other := data
			other.SessionID = "other-browser"
			require.NoError(t, store.SaveSession(context.Background(), other))
			app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: server.URL + "/client.json"}, store)
			app.Client = server.Client()
			handler := lifecycleHandler(app)
			store.failure = testcase.storeFailure
			logged := captureLogs(t)
			response := httptest.NewRecorder()
			handler.HandleRefresh(response, lifecycleRefreshRequest(t, handler, data, context.Background()))
			if testcase.terminal {
				assert.Equal(t, 401, response.Code)
				assert.Contains(t, logged.String(), "sentinel=ErrRefreshRejected", "rejected refresh must name its sentinel")
			} else {
				assert.GreaterOrEqual(t, response.Code, 500, "infrastructure failure must remain retryable")
				assert.Less(t, response.Code, 600)
				assert.Contains(t, logged.String(), "level=ERROR", "transient refresh failure must be logged")
				assert.Contains(t, logged.String(), "error=", "transient refresh failure must log its cause")
			}
			assert.Contains(t, logged.String(), "did="+data.AccountDID.String())
			assert.Contains(t, logged.String(), "session_id="+data.SessionID)
			store.failure = ""
			_, err = store.GetSession(context.Background(), data.AccountDID, data.SessionID)
			if testcase.terminal {
				assert.Error(t, err, "rejected refresh must invalidate exactly its stored session")
			} else {
				assert.NoError(t, err, "transient failure must leave the session stored")
			}
			_, err = store.GetSession(context.Background(), data.AccountDID, other.SessionID)
			require.NoError(t, err, "unrelated browser must survive")
		})
	}
}

func TestHandleLogoutSessionLifecycle(t *testing.T) {
	for _, testcase := range []struct {
		name         string
		storeFailure string
		missing      bool
		corrupt      bool
	}{
		{name: "success"},
		{name: "already_logged_out", missing: true},
		{name: "corrupt_stored_key", corrupt: true},
		{name: "database_read_failure", storeFailure: "get"},
		{name: "database_delete_failure", storeFailure: "delete"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			store := &refreshFailureStore{ClientAuthStore: indigooauth.NewMemStore()}
			key, err := atcrypto.GeneratePrivateKeyP256()
			require.NoError(t, err)
			data := indigooauth.ClientSessionData{AccountDID: syntax.DID("did:plc:logoutlifecycle"), SessionID: "browser", HostURL: "https://pds.example", AuthServerURL: "https://pds.example", AccessToken: "initial-access", RefreshToken: "initial-refresh", DPoPPrivateKeyMultibase: key.Multibase()}
			sibling := data
			sibling.SessionID = "other-browser"
			require.NoError(t, store.SaveSession(context.Background(), sibling))
			if testcase.corrupt {
				data.DPoPPrivateKeyMultibase = "not-a-key"
			}
			if !testcase.missing {
				require.NoError(t, store.SaveSession(context.Background(), data))
			}
			handler := lifecycleHandler(indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: "https://coves.example/client.json"}, store))
			sealed, err := handler.client.SealSession(data.AccountDID.String(), data.SessionID, time.Hour)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
			request.AddCookie(&http.Cookie{Name: "coves_session", Value: sealed})
			store.failure = testcase.storeFailure
			logged := captureLogs(t)
			response := httptest.NewRecorder()
			handler.HandleLogout(response, request)
			store.failure = ""
			var cleared bool
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == "coves_session" && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			_, err = store.GetSession(context.Background(), data.AccountDID, data.SessionID)
			_, siblingErr := store.GetSession(context.Background(), sibling.AccountDID, sibling.SessionID)
			assert.NoError(t, siblingErr, "logging out one browser must not touch the account's other sessions")
			if testcase.storeFailure != "" {
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, "storage failure must be retryable, not reported as logged out")
				assert.NotContains(t, response.Body.String(), "logged_out")
				assert.NotContains(t, response.Body.String(), "database unavailable", "internal detail must not leak")
				assert.False(t, cleared, "cookie must survive so the client can retry logout")
				assert.NoError(t, err, "row must remain for the retry to delete")
				assert.Contains(t, logged.String(), "level=ERROR")
				assert.Contains(t, logged.String(), "error=")
			} else {
				assert.Equal(t, http.StatusOK, response.Code)
				assert.Contains(t, response.Body.String(), "logged_out")
				assert.True(t, cleared, "cookie must be cleared once the row is gone")
				assert.ErrorIs(t, err, ErrSessionNotFound)
				if testcase.missing || testcase.corrupt {
					assert.Contains(t, logged.String(), "already removed")
				}
			}
			if testcase.storeFailure != "" || testcase.missing || testcase.corrupt {
				assert.Contains(t, logged.String(), "did="+data.AccountDID.String())
				assert.Contains(t, logged.String(), "session_id="+data.SessionID)
			}
		})
	}
}

// captureLogs routes the default slog logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

func lifecycleHandler(app *indigooauth.ClientApp) *OAuthHandler {
	client := &OAuthClient{ClientApp: app, Config: &OAuthConfig{SealedTokenTTL: time.Hour}, SealSecret: bytes.Repeat([]byte{42}, 32)}
	return NewOAuthHandler(client, app.Store)
}

func lifecycleRefreshRequest(t *testing.T, handler *OAuthHandler, data indigooauth.ClientSessionData, ctx context.Context) *http.Request {
	t.Helper()
	sealed, err := handler.client.SealSession(data.AccountDID.String(), data.SessionID, time.Hour)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]string{"did": data.AccountDID.String(), "session_id": data.SessionID, "sealed_token": sealed})
	require.NoError(t, err)
	return httptest.NewRequest(http.MethodPost, "/oauth/refresh", bytes.NewReader(body)).WithContext(ctx)
}

type refreshFailureStore struct {
	indigooauth.ClientAuthStore
	failure string
}

func (s *refreshFailureStore) GetSession(ctx context.Context, did syntax.DID, id string) (*indigooauth.ClientSessionData, error) {
	if s.failure == "get" {
		return nil, errors.New("database unavailable")
	}
	session, err := s.ClientAuthStore.GetSession(ctx, did, id)
	if err != nil {
		// MemStore's only failure is a missing row; report it like the Postgres store.
		return nil, ErrSessionNotFound
	}
	return session, nil
}

func (s *refreshFailureStore) SaveSession(ctx context.Context, data indigooauth.ClientSessionData) error {
	if s.failure == "save" {
		return errors.New("database unavailable")
	}
	return s.ClientAuthStore.SaveSession(ctx, data)
}

func (s *refreshFailureStore) DeleteSession(ctx context.Context, did syntax.DID, id string) error {
	if s.failure == "delete" {
		return errors.New("database unavailable")
	}
	return s.ClientAuthStore.DeleteSession(ctx, did, id)
}
