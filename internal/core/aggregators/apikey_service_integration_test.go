//go:build integration

package aggregators_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/aggregators"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// apiKeyFixture is a registered aggregator with the service wired to the real
// aggregator repository and the real coordinated OAuth session store.
type apiKeyFixture struct {
	db            *sql.DB
	repo          aggregators.Repository
	store         oauth.ClientAuthStore
	app           *oauth.ClientApp
	service       *aggregators.APIKeyService
	aggregatorDID syntax.DID
}

// newAPIKeyFixture registers an aggregator. client is the HTTP client the OAuth
// app uses; GenerateKey itself makes no HTTP requests.
func newAPIKeyFixture(t *testing.T, clientID string, client *http.Client) *apiKeyFixture {
	t.Helper()
	return newAPIKeyFixtureWithCoordinationConnections(t, clientID, client, 3)
}

// newAPIKeyFixtureWithCoordinationConnections is newAPIKeyFixture with the
// session coordination pool capped at coordinationConnections.
func newAPIKeyFixtureWithCoordinationConnections(t *testing.T, clientID string, client *http.Client, coordinationConnections int) *apiKeyFixture {
	t.Helper()
	ctx := context.Background()
	db := testkit.DB(t)

	var name string
	require.NoError(t, db.QueryRow("SELECT current_database()").Scan(&name))
	coordination, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(name))
	require.NoError(t, err)
	coordination.SetMaxOpenConns(coordinationConnections)
	t.Cleanup(func() { _ = coordination.Close() })
	store := covesoauth.NewCoordinatedPostgresOAuthStore(db, coordination, 0)

	repo := postgres.NewAggregatorRepository(db, credentialciphertest.Fixed())
	aggregatorDID := "did:plc:" + testkit.UniqueID(t)
	require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "API Key Aggregator",
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   "at://" + aggregatorDID + "/social.coves.aggregator.service/self",
		RecordCID:   "bafyservice",
	}))

	app := oauth.NewClientApp(&oauth.ClientConfig{ClientID: clientID}, store)
	if client != nil {
		app.Client = client
	}
	return &apiKeyFixture{
		db:            db,
		repo:          repo,
		store:         store,
		app:           app,
		service:       aggregators.NewAPIKeyService(repo, app),
		aggregatorDID: syntax.DID(aggregatorDID),
	}
}

// sessionFieldDifferences names the fields in which two sessions differ,
// without printing their values (tokens and keys).
func sessionFieldDifferences(want, got oauth.ClientSessionData) []string {
	var differences []string
	check := func(field string, equal bool) {
		if !equal {
			differences = append(differences, field)
		}
	}
	check("AccountDID", want.AccountDID == got.AccountDID)
	check("SessionID", want.SessionID == got.SessionID)
	check("HostURL", want.HostURL == got.HostURL)
	check("AuthServerURL", want.AuthServerURL == got.AuthServerURL)
	check("AuthServerTokenEndpoint", want.AuthServerTokenEndpoint == got.AuthServerTokenEndpoint)
	check("AuthServerRevocationEndpoint", want.AuthServerRevocationEndpoint == got.AuthServerRevocationEndpoint)
	check("Scopes", len(want.Scopes) == len(got.Scopes) && func() bool {
		for i := range want.Scopes {
			if want.Scopes[i] != got.Scopes[i] {
				return false
			}
		}
		return true
	}())
	check("AccessToken", want.AccessToken == got.AccessToken)
	check("RefreshToken", want.RefreshToken == got.RefreshToken)
	check("DPoPAuthServerNonce", want.DPoPAuthServerNonce == got.DPoPAuthServerNonce)
	check("DPoPHostNonce", want.DPoPHostNonce == got.DPoPHostNonce)
	check("DPoPPrivateKeyMultibase", want.DPoPPrivateKeyMultibase == got.DPoPPrivateKeyMultibase)
	return differences
}

// sharedRefreshTokens counts refresh tokens held by more than one
// oauth_sessions row.
func sharedRefreshTokens(t *testing.T, db *sql.DB) int {
	t.Helper()
	var shared int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM (
			SELECT refresh_token FROM oauth_sessions
			GROUP BY refresh_token HAVING count(*) > 1
		) duplicated`).Scan(&shared))
	return shared
}

// Creating an API key moves the operator's stored browser session to the API
// key session: one refresh-token lineage, one row. The request's copy of the
// session may be stale (a concurrent request rotated the tokens), so the row
// that moves, and the credentials the aggregator row records, are the stored
// ones.
func TestAPIKeyService_GenerateKey_MovesOperatorSession(t *testing.T) {
	rows := []struct {
		name               string
		existingAPIKeyRows bool
	}{
		{name: "first key"},
		{name: "key replacement", existingAPIKeyRows: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newAPIKeyFixture(t, "https://coves.example/oauth/client-metadata.json", nil)
			key, err := atcrypto.GeneratePrivateKeyP256()
			require.NoError(t, err)
			dpopKey := key.Multibase()
			operatorSessionID := "browser-" + testkit.UniqueID(t)

			stored := oauth.ClientSessionData{
				AccountDID:                   fixture.aggregatorDID,
				SessionID:                    operatorSessionID,
				HostURL:                      "https://pds.operator.example",
				AuthServerURL:                "https://auth.operator.example",
				AuthServerTokenEndpoint:      "https://auth.operator.example/oauth/token",
				AuthServerRevocationEndpoint: "https://auth.operator.example/oauth/revoke",
				Scopes:                       []string{"atproto", "transition:generic"},
				AccessToken:                  "stored-access-current",
				RefreshToken:                 "stored-refresh-current",
				DPoPAuthServerNonce:          "stored-authserver-nonce",
				DPoPHostNonce:                "stored-host-nonce",
				DPoPPrivateKeyMultibase:      dpopKey,
			}
			require.NoError(t, fixture.store.SaveSession(ctx, stored))

			if row.existingAPIKeyRows {
				previous := stored
				previous.SessionID = aggregators.DefaultSessionID
				previous.AccessToken = "previous-key-access"
				previous.RefreshToken = "previous-key-refresh"
				require.NoError(t, fixture.store.SaveSession(ctx, previous))
				require.NoError(t, fixture.repo.SetAPIKey(ctx, fixture.aggregatorDID.String(), "ckapi_previo", "previous-key-hash", &aggregators.OAuthCredentials{
					AccessToken:             "previous-key-access",
					RefreshToken:            "previous-key-refresh",
					TokenExpiresAt:          time.Now().Add(time.Hour),
					PDSURL:                  "https://pds.operator.example",
					AuthServerIss:           "https://auth.operator.example",
					AuthServerTokenEndpoint: "https://auth.operator.example/oauth/token",
					DPoPPrivateKeyMultibase: dpopKey,
				}))
			}

			stale := stored
			stale.AccessToken = "stale-access-spent"
			stale.RefreshToken = "stale-refresh-spent"
			stale.DPoPAuthServerNonce = "stale-authserver-nonce"
			stale.DPoPHostNonce = "stale-host-nonce"

			_, _, err = fixture.service.GenerateKey(ctx, fixture.aggregatorDID.String(), &stale)
			require.NoError(t, err)

			_, err = fixture.store.GetSession(ctx, fixture.aggregatorDID, operatorSessionID)
			require.ErrorIs(t, err, covesoauth.ErrSessionNotFound, "the operator's browser session row must be gone after the move")

			apiKeySession, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, aggregators.DefaultSessionID)
			require.NoError(t, err)
			want := oauth.ClientSessionData{
				AccountDID:                   fixture.aggregatorDID,
				SessionID:                    "apikey",
				HostURL:                      "https://pds.operator.example",
				AuthServerURL:                "https://auth.operator.example",
				AuthServerTokenEndpoint:      "https://auth.operator.example/oauth/token",
				AuthServerRevocationEndpoint: "https://auth.operator.example/oauth/revoke",
				Scopes:                       []string{"atproto", "transition:generic"},
				AccessToken:                  "stored-access-current",
				RefreshToken:                 "stored-refresh-current",
				DPoPAuthServerNonce:          "stored-authserver-nonce",
				DPoPHostNonce:                "stored-host-nonce",
				DPoPPrivateKeyMultibase:      dpopKey,
			}
			require.Empty(t, sessionFieldDifferences(want, *apiKeySession), "API key session fields that differ from the stored operator session")

			require.Zero(t, sharedRefreshTokens(t, fixture.db), "oauth_sessions rows sharing a refresh token")

			creds, err := fixture.repo.GetAggregatorCredentials(ctx, fixture.aggregatorDID.String())
			require.NoError(t, err)
			require.True(t, creds.OAuthAccessToken == "stored-access-current", "aggregator access token must be the stored operator session's")
			require.True(t, creds.OAuthRefreshToken == "stored-refresh-current", "aggregator refresh token must be the stored operator session's")
			require.Equal(t, "stored-authserver-nonce", creds.OAuthDPoPAuthServerNonce)
			require.Equal(t, "stored-host-nonce", creds.OAuthDPoPPDSNonce)
		})
	}
}

// The scheduled refresh job's window must exclude a token that was just
// recorded (about an hour left) and include one with twenty minutes left — and
// a token inside the window must actually be refreshed, not skipped by the
// request path's shorter buffer.
func TestAPIKeyService_RefreshExpiringTokens_Window(t *testing.T) {
	rows := []struct {
		name          string
		expiresIn     time.Duration // zero: keep the expiry GenerateKey recorded
		wantRefreshed int
		wantErrors    int
		wantRequests  int64
	}{
		{name: "fresh key is not refreshed", wantRefreshed: 0, wantErrors: 0, wantRequests: 0},
		{name: "twenty minutes left is refreshed", expiresIn: 20 * time.Minute, wantRefreshed: 0, wantErrors: 1, wantRequests: 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			// A token-endpoint tripwire: every refresh attempt is counted and
			// answered as unavailable, so an attempt is visible as an error.
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					requests.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			}))
			t.Cleanup(server.Close)

			fixture := newAPIKeyFixture(t, server.URL+"/client.json", server.Client())
			key, err := atcrypto.GeneratePrivateKeyP256()
			require.NoError(t, err)
			operator := oauth.ClientSessionData{
				AccountDID:              fixture.aggregatorDID,
				SessionID:               "browser-" + testkit.UniqueID(t),
				HostURL:                 server.URL,
				AuthServerURL:           server.URL,
				AuthServerTokenEndpoint: server.URL + "/token",
				Scopes:                  []string{"atproto"},
				AccessToken:             "window-access",
				RefreshToken:            "window-refresh",
				DPoPPrivateKeyMultibase: key.Multibase(),
			}
			require.NoError(t, fixture.store.SaveSession(ctx, operator))
			_, _, err = fixture.service.GenerateKey(ctx, fixture.aggregatorDID.String(), &operator)
			require.NoError(t, err)
			if row.expiresIn != 0 {
				require.NoError(t, fixture.repo.UpdateOAuthTokens(ctx, fixture.aggregatorDID.String(), "window-access", "window-refresh", time.Now().Add(row.expiresIn)))
			}

			refreshed, errs := fixture.service.RefreshExpiringTokens(ctx, aggregators.TokenRefreshWindow)

			require.Equal(t, row.wantRefreshed, refreshed, "aggregators reported refreshed")
			require.Len(t, errs, row.wantErrors, "refresh errors")
			require.Equal(t, row.wantRequests, requests.Load(), "token endpoint refresh requests")
			creds, err := fixture.repo.GetAggregatorCredentials(ctx, fixture.aggregatorDID.String())
			require.NoError(t, err)
			require.True(t, creds.OAuthAccessToken == "window-access" && creds.OAuthRefreshToken == "window-refresh", "aggregator tokens must be unchanged")
			apiKeySession, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, aggregators.DefaultSessionID)
			require.NoError(t, err)
			require.True(t, apiKeySession.AccessToken == "window-access" && apiKeySession.RefreshToken == "window-refresh", "API key session tokens must be unchanged")
		})
	}
}

// failingSetAPIKeyRepository fails every SetAPIKey call.
type failingSetAPIKeyRepository struct {
	aggregators.Repository
}

func (failingSetAPIKeyRepository) SetAPIKey(context.Context, string, string, string, *aggregators.OAuthCredentials) error {
	return errors.New("set API key failed")
}

// A failed key replacement must leave both the operator's browser session and
// the existing key's session exactly as they were: the old key still
// authenticates, so it must keep its own OAuth session.
func TestAPIKeyService_GenerateKey_FailedReplacementKeepsBothSessions(t *testing.T) {
	ctx := context.Background()
	fixture := newAPIKeyFixture(t, "https://coves.example/oauth/client-metadata.json", nil)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	operator := oauth.ClientSessionData{
		AccountDID:              fixture.aggregatorDID,
		SessionID:               "browser-" + testkit.UniqueID(t),
		HostURL:                 "https://pds.operator.example",
		AuthServerURL:           "https://auth.operator.example",
		AuthServerTokenEndpoint: "https://auth.operator.example/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "operator-access",
		RefreshToken:            "operator-refresh",
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	require.NoError(t, fixture.store.SaveSession(ctx, operator))
	previous := operator
	previous.SessionID = aggregators.DefaultSessionID
	previous.AccessToken = "previous-key-access"
	previous.RefreshToken = "previous-key-refresh"
	require.NoError(t, fixture.store.SaveSession(ctx, previous))

	service := aggregators.NewAPIKeyService(failingSetAPIKeyRepository{Repository: fixture.repo}, fixture.app)
	_, _, err = service.GenerateKey(ctx, fixture.aggregatorDID.String(), &operator)
	require.Error(t, err)
	require.NotErrorIs(t, err, covesoauth.ErrSessionMoveNotUndone)

	gotOperator, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, operator.SessionID)
	require.NoError(t, err, "the operator's browser session must be restored")
	require.Empty(t, sessionFieldDifferences(operator, *gotOperator), "operator session fields changed by the failed replacement")
	gotPrevious, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, aggregators.DefaultSessionID)
	require.NoError(t, err, "the existing key's session must survive the failed replacement")
	require.Empty(t, sessionFieldDifferences(previous, *gotPrevious), "existing key session fields changed by the failed replacement")
	require.Zero(t, sharedRefreshTokens(t, fixture.db), "oauth_sessions rows sharing a refresh token")
}

// waitForSessionLockWaiter returns once some connection to the test database
// is waiting for an advisory lock (nil), or once GenerateKey finishes first
// (errGenerateFinishedEarly, with its result kept in finished).
func waitForSessionLockWaiter(ctx context.Context, db *sql.DB, generateDone <-chan error, finished *error) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		).Scan(&waiting); err != nil {
			return fmt.Errorf("read pg_locks: %w", err)
		}
		if waiting > 0 {
			return nil
		}
		select {
		case err := <-generateDone:
			*finished = err
			return errGenerateFinishedEarly
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

var errGenerateFinishedEarly = errors.New("GenerateKey finished without waiting for the browser session operation")

// A refresh in flight on the operator's browser session must finish before
// the key creation reads the row, so the key gets the rotated tokens. While
// the key creation waits for the browser session it must not hold the API key
// session, so the existing key's own refresh is not blocked behind it.
func TestAPIKeyService_GenerateKey_WaitsForBrowserSessionOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newAPIKeyFixture(t, "https://coves.example/oauth/client-metadata.json", nil)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	operator := oauth.ClientSessionData{
		AccountDID:              fixture.aggregatorDID,
		SessionID:               "browser-" + testkit.UniqueID(t),
		HostURL:                 "https://pds.operator.example",
		AuthServerURL:           "https://auth.operator.example",
		AuthServerTokenEndpoint: "https://auth.operator.example/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "operator-access-before",
		RefreshToken:            "operator-refresh-before",
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	require.NoError(t, fixture.store.SaveSession(ctx, operator))
	previous := operator
	previous.SessionID = aggregators.DefaultSessionID
	previous.AccessToken = "previous-key-access"
	previous.RefreshToken = "previous-key-refresh"
	require.NoError(t, fixture.store.SaveSession(ctx, previous))

	generateDone := make(chan error, 1)
	var generateErr error
	generateFinished := false
	holderErr := covesoauth.RunSessionOperation(ctx, fixture.app, fixture.aggregatorDID, operator.SessionID, func(session *oauth.ClientSession) error {
		go func() {
			request := operator
			_, _, err := fixture.service.GenerateKey(ctx, fixture.aggregatorDID.String(), &request)
			generateDone <- err
		}()
		if err := waitForSessionLockWaiter(ctx, fixture.db, generateDone, &generateErr); err != nil {
			generateFinished = errors.Is(err, errGenerateFinishedEarly)
			return err
		}

		// The existing key refreshes while the key creation waits.
		keyContext, keyCancel := context.WithTimeout(ctx, 3*time.Second)
		defer keyCancel()
		if err := covesoauth.RunSessionOperation(keyContext, fixture.app, fixture.aggregatorDID, aggregators.DefaultSessionID, func(keySession *oauth.ClientSession) error {
			rotatedKey := *keySession.Data
			rotatedKey.AccessToken = "previous-key-access-rotated"
			rotatedKey.RefreshToken = "previous-key-refresh-rotated"
			keySession.PersistSessionCallback(keyContext, &rotatedKey)
			return nil
		}); err != nil {
			return fmt.Errorf("existing key's session operation while GenerateKey waited: %w", err)
		}

		// The browser session's refresh rotates its tokens.
		rotated := *session.Data
		rotated.AccessToken = "operator-access-rotated"
		rotated.RefreshToken = "operator-refresh-rotated"
		session.PersistSessionCallback(ctx, &rotated)
		return nil
	})
	require.NoError(t, holderErr)
	if !generateFinished {
		select {
		case generateErr = <-generateDone:
		case <-ctx.Done():
			t.Fatal("GenerateKey did not finish after the browser session operation ended")
		}
	}
	require.NoError(t, generateErr)

	_, err = fixture.store.GetSession(ctx, fixture.aggregatorDID, operator.SessionID)
	require.ErrorIs(t, err, covesoauth.ErrSessionNotFound, "the operator's browser session row must be gone after the move")
	apiKeySession, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, aggregators.DefaultSessionID)
	require.NoError(t, err)
	require.True(t, apiKeySession.AccessToken == "operator-access-rotated" && apiKeySession.RefreshToken == "operator-refresh-rotated",
		"API key session must hold the browser session's rotated tokens")
	var rows int
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT count(*) FROM oauth_sessions WHERE did = $1`, fixture.aggregatorDID.String()).Scan(&rows))
	require.Equal(t, 1, rows, "oauth_sessions rows for the aggregator")
	creds, err := fixture.repo.GetAggregatorCredentials(ctx, fixture.aggregatorDID.String())
	require.NoError(t, err)
	require.True(t, creds.OAuthAccessToken == "operator-access-rotated" && creds.OAuthRefreshToken == "operator-refresh-rotated",
		"aggregator credentials must be the browser session's rotated tokens")
}

// MoveSession nests the API key session's lock inside the browser session's
// on one coordination connection, so key creation works with a single
// coordination connection available.
func TestAPIKeyService_GenerateKey_SingleCoordinationConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fixture := newAPIKeyFixtureWithCoordinationConnections(t, "https://coves.example/oauth/client-metadata.json", nil, 1)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	operator := oauth.ClientSessionData{
		AccountDID:              fixture.aggregatorDID,
		SessionID:               "browser-" + testkit.UniqueID(t),
		HostURL:                 "https://pds.operator.example",
		AuthServerURL:           "https://auth.operator.example",
		AuthServerTokenEndpoint: "https://auth.operator.example/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "single-access",
		RefreshToken:            "single-refresh",
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	require.NoError(t, fixture.store.SaveSession(ctx, operator))

	_, _, err = fixture.service.GenerateKey(ctx, fixture.aggregatorDID.String(), &operator)
	require.NoError(t, err)
	apiKeySession, err := fixture.store.GetSession(ctx, fixture.aggregatorDID, aggregators.DefaultSessionID)
	require.NoError(t, err)
	require.True(t, apiKeySession.RefreshToken == "single-refresh", "API key session must hold the operator's tokens")
}
