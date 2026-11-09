//go:build integration

package oauth_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleLogoutWithBearerRevokesMobileSession covers the mobile logout
// path end to end against the Postgres session store, wired as the server
// wires it: a client that sends only Authorization: Bearer <sealed> must be
// able to delete its oauth_sessions row and revoke its tokens on the
// authorization server, after which the same sealed token neither
// authenticates nor refreshes.
func TestHandleLogoutWithBearerRevokesMobileSession(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()

	var mu sync.Mutex
	var revoked []string
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/revoke":
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			revoked = append(revoked, r.PostForm.Get("token"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case "/token":
			// A refresh that reaches the authorization server would succeed, so
			// a post-logout 401 can only come from the deleted session row.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"DPoP"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(authorizationServer.Close)

	var databaseName string
	require.NoError(t, db.QueryRow("SELECT current_database()").Scan(&databaseName))
	// Session coordination gets its own pool, as the server wires it.
	coordinationDB, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(databaseName))
	require.NoError(t, err)
	coordinationDB.SetMaxOpenConns(3)
	t.Cleanup(func() { _ = coordinationDB.Close() })
	store := oauth.NewMobileAwareStoreWrapper(oauth.NewCoordinatedPostgresOAuthStore(db, coordinationDB, 0))

	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	did := syntax.DID("did:plc:bearerlogouttest")
	const sessionID = "mobile-device"
	require.NoError(t, store.SaveSession(ctx, indigooauth.ClientSessionData{
		AccountDID:                   did,
		SessionID:                    sessionID,
		HostURL:                      authorizationServer.URL,
		AuthServerURL:                authorizationServer.URL,
		AuthServerTokenEndpoint:      authorizationServer.URL + "/token",
		AuthServerRevocationEndpoint: authorizationServer.URL + "/revoke",
		AccessToken:                  "mobile-access",
		RefreshToken:                 "mobile-refresh",
		DPoPPrivateKeyMultibase:      key.Multibase(),
	}))

	app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: authorizationServer.URL + "/client.json"}, store)
	app.Client = authorizationServer.Client()
	client := &oauth.OAuthClient{
		ClientApp:  app,
		Config:     &oauth.OAuthConfig{SealedTokenTTL: time.Hour},
		SealSecret: bytes.Repeat([]byte{7}, 32),
	}
	handler := oauth.NewOAuthHandler(client, store)
	protected := middleware.NewOAuthAuthMiddleware(client, store).RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	sealed, err := client.SealSession(did.String(), sessionID, time.Hour)
	require.NoError(t, err)

	authenticated := func() int {
		request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getProfile", nil)
		request.Header.Set("Authorization", "Bearer "+sealed)
		response := httptest.NewRecorder()
		protected.ServeHTTP(response, request)
		return response.Code
	}
	require.Equal(t, http.StatusOK, authenticated(), "the Bearer token must authenticate before logout")

	logoutRequest := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
	logoutRequest.Header.Set("Authorization", "Bearer "+sealed)
	logoutResponse := httptest.NewRecorder()
	handler.HandleLogout(logoutResponse, logoutRequest)
	require.Equal(t, http.StatusOK, logoutResponse.Code)
	var logoutBody map[string]string
	require.NoError(t, json.Unmarshal(logoutResponse.Body.Bytes(), &logoutBody))
	assert.Equal(t, "logged_out", logoutBody["status"])

	var rows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM oauth_sessions WHERE did = $1 AND session_id = $2`, did.String(), sessionID).Scan(&rows))
	assert.Equal(t, 0, rows, "Bearer logout must delete the oauth_sessions row")

	mu.Lock()
	assert.ElementsMatch(t, []string{"mobile-access", "mobile-refresh"}, revoked,
		"Bearer logout must revoke both tokens on the authorization server")
	mu.Unlock()

	assert.Equal(t, http.StatusUnauthorized, authenticated(), "a logged-out Bearer token must not authenticate")

	refreshBody, err := json.Marshal(map[string]string{"did": did.String(), "session_id": sessionID, "sealed_token": sealed})
	require.NoError(t, err)
	refreshResponse := httptest.NewRecorder()
	handler.HandleRefresh(refreshResponse, httptest.NewRequest(http.MethodPost, "/oauth/refresh", bytes.NewReader(refreshBody)))
	assert.Equal(t, http.StatusUnauthorized, refreshResponse.Code, "a logged-out Bearer token must not refresh")
}
