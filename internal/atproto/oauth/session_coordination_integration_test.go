//go:build integration

package oauth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Independent ClientApps and store instances share only Postgres. The blocked
// owner's operation must exclude another operation for that same session, even
// through the mobile store wrapper, while a different browser remains usable.
func TestSessionOperationCoordinatesIndependentPostgresClients(t *testing.T) {
	db := testkit.DB(t)
	first, data := coordinationClient(t, db, false)
	second, _ := coordinationClient(t, db, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ownerContext, cancelOwner := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelOwner()
	ownerResult := make(chan error, 1)
	go func() {
		ownerResult <- RunSessionOperation(ownerContext, first, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error {
			close(entered)
			select {
			case <-release:
			case <-ownerContext.Done():
				return ownerContext.Err()
			}
			_, err := session.RefreshTokens(ownerContext)
			return err
		})
	}()
	select {
	case <-entered:
	case <-ownerContext.Done():
		t.Fatal("owner never entered operation")
	}

	waiterContext, cancelWaiter := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelWaiter()
	var waiterRan atomic.Bool
	err := RunSessionOperation(waiterContext, second, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error { waiterRan.Store(true); return nil })
	assert.ErrorIs(t, err, context.DeadlineExceeded, "same-session waiter must cancel while the owner remains inside its operation")
	assert.False(t, waiterRan.Load(), "canceled waiter must never execute its operation")

	unrelated := data
	unrelated.SessionID = "other-browser"
	require.NoError(t, second.Store.SaveSession(context.Background(), unrelated))
	unrelatedContext, cancelUnrelated := context.WithTimeout(context.Background(), time.Second)
	defer cancelUnrelated()
	require.NoError(t, RunSessionOperation(unrelatedContext, second, unrelated.AccountDID, unrelated.SessionID, func(*indigooauth.ClientSession) error { return nil }), "an unrelated session must not wait for the first session")
	// Canceling an owner must release coordination as well as its DB connection.
	cancelOwner()
	require.ErrorIs(t, <-ownerResult, context.Canceled)
	successorContext, cancelSuccessor := context.WithTimeout(context.Background(), time.Second)
	defer cancelSuccessor()
	require.NoError(t, RunSessionOperation(successorContext, second, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error {
		_, err := session.RefreshTokens(successorContext)
		return err
	}))
	saved, err := second.Store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.NoError(t, err)
	require.True(t, saved.AccessToken == "rotated-access" && saved.RefreshToken == "rotated-refresh", "successor must persist rotated credentials")
}

// A session operation must not hold the pool's only connection and then try to
// obtain a second one to read or persist credentials.
func TestSessionOperationSingleConnectionPool(t *testing.T) {
	db := testkit.DB(t)
	db.SetMaxOpenConns(1)
	app, data := coordinationClient(t, db, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, RunSessionOperation(ctx, app, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error { _, err := session.RefreshTokens(ctx); return err }))
	saved, err := app.Store.GetSession(ctx, data.AccountDID, data.SessionID)
	require.NoError(t, err)
	require.True(t, saved.RefreshToken == "rotated-refresh", "rotation must be saved before releasing the operation")
}

// The request pool is fully occupied for the whole test. Session operations,
// their lock waits and their credential persistence must proceed on the
// coordination pool alone, and a same-session waiter without a deadline must
// give up as busy instead of blocking forever.
func TestSessionOperationNeverConsumesRequestPool(t *testing.T) {
	db := testkit.DB(t)
	store := coordinatedStore(t, db)
	store.lockTimeout = 200 * time.Millisecond
	owner, data := coordinationClientWithStore(t, store)
	contender, _ := coordinationClientWithStore(t, NewMobileAwareStoreWrapper(store))
	unrelated := data
	unrelated.SessionID = "other-browser"
	require.NoError(t, store.SaveSession(context.Background(), unrelated))
	// From here on the request pool has nothing to give.
	db.SetMaxOpenConns(1)
	occupied, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	entered := make(chan struct{})
	release := make(chan struct{})
	ownerContext, cancelOwner := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelOwner()
	ownerResult := make(chan error, 1)
	go func() {
		ownerResult <- RunSessionOperation(ownerContext, owner, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error {
			close(entered)
			select {
			case <-release:
			case <-ownerContext.Done():
				return ownerContext.Err()
			}
			_, err := session.RefreshTokens(ownerContext)
			return err
		})
	}()
	select {
	case <-entered:
	case <-ownerContext.Done():
		t.Fatal("owner never entered operation")
	}

	started := time.Now()
	var waiterRan atomic.Bool
	err = RunSessionOperation(context.Background(), contender, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error { waiterRan.Store(true); return nil })
	require.ErrorIs(t, err, ErrSessionBusy, "a waiter without a deadline must be bounded by the lock timeout")
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.False(t, waiterRan.Load(), "busy waiter must never execute its operation")

	unrelatedContext, cancelUnrelated := context.WithTimeout(context.Background(), time.Second)
	defer cancelUnrelated()
	require.NoError(t, RunSessionOperation(unrelatedContext, contender, unrelated.AccountDID, unrelated.SessionID, func(session *indigooauth.ClientSession) error {
		_, err := session.RefreshTokens(unrelatedContext)
		return err
	}), "an unrelated session operation must not need the request pool")

	close(release)
	require.NoError(t, <-ownerResult)
	successorContext, cancelSuccessor := context.WithTimeout(context.Background(), time.Second)
	defer cancelSuccessor()
	require.NoError(t, RunSessionOperation(successorContext, contender, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error { return nil }), "the lock must be free once the owner finishes")
	require.NoError(t, occupied.Close())
	saved, err := store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.NoError(t, err)
	assert.True(t, saved.RefreshToken == "rotated-refresh", "credentials must persist while the request pool is exhausted")
}

// An unlock failure happens after the PDS mutation has already committed, so it
// must not change the operation's own outcome. The connection is discarded so
// the lock cannot leak.
func TestSessionOperationResultSurvivesUnlockFailure(t *testing.T) {
	db := testkit.DB(t)
	store := coordinatedStore(t, db)
	app, data := coordinationClientWithStore(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	operationFailure := errors.New("operation failed")
	for _, outcome := range []error{nil, operationFailure} {
		err := store.withSessionOperation(ctx, data.AccountDID, data.SessionID, func(ctx context.Context) error {
			// Steal the lock out from under the coordinator so its own unlock reports "not held".
			var released bool
			require.NoError(t, store.sessionDatabase(ctx).QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", sessionLockKey(data.AccountDID, data.SessionID)).Scan(&released))
			require.True(t, released)
			return outcome
		})
		require.Equal(t, outcome, err, "unlock cleanup failure must not alter the operation result")
		require.NoError(t, RunSessionOperation(ctx, app, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error { return nil }), "the session must be usable again after the discarded connection")
	}
}

// coordinatedStore opens a second handle to the test clone so session
// coordination has its own pool, as the server wires it.
func coordinatedStore(t *testing.T, db *sql.DB) *PostgresOAuthStore {
	t.Helper()
	var name string
	require.NoError(t, db.QueryRow("SELECT current_database()").Scan(&name))
	coordination, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(name))
	require.NoError(t, err)
	coordination.SetMaxOpenConns(3)
	t.Cleanup(func() { _ = coordination.Close() })
	store, ok := NewCoordinatedPostgresOAuthStore(db, coordination, 0).(*PostgresOAuthStore)
	require.True(t, ok)
	return store
}

func coordinationClient(t *testing.T, db *sql.DB, mobile bool) (*indigooauth.ClientApp, indigooauth.ClientSessionData) {
	t.Helper()
	store := NewPostgresOAuthStore(db, 0)
	if mobile {
		store = NewMobileAwareStoreWrapper(store)
	}
	return coordinationClientWithStore(t, store)
}

func coordinationClientWithStore(t *testing.T, store indigooauth.ClientAuthStore) (*indigooauth.ClientApp, indigooauth.ClientSessionData) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Error("unexpected authorization server request")
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"DPoP"}`))
	}))
	t.Cleanup(server.Close)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	data := indigooauth.ClientSessionData{AccountDID: syntax.DID("did:plc:coordinationtest"), SessionID: "browser", HostURL: server.URL, AuthServerURL: server.URL, AuthServerTokenEndpoint: server.URL + "/token", AccessToken: "initial-access", RefreshToken: "initial-refresh", DPoPPrivateKeyMultibase: key.Multibase()}
	// Only seed once; independently constructed apps must see the same stored session.
	if _, err := store.GetSession(context.Background(), data.AccountDID, data.SessionID); err != nil {
		require.ErrorIs(t, err, ErrSessionNotFound)
		require.NoError(t, store.SaveSession(context.Background(), data))
	}
	app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: server.URL + "/client.json"}, store)
	app.Client = server.Client()
	return app, data
}
