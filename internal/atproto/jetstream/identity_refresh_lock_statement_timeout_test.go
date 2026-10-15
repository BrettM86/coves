//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/tests/testkit"

	_ "github.com/lib/pq"
)

// The lock pool's DSN can carry a session statement_timeout shorter than the
// lock wait (DB_STATEMENT_TIMEOUT, or an explicit value in DATABASE_URL). A
// wait that outlasts it must still end as the deferred busy error from
// lock_timeout, keeping its connection, not as a cancelled statement.
func TestIdentityRefreshLockBusyUnderShortStatementTimeout(t *testing.T) {
	const did = "did:plc:lockbusyshortstatementtimeout"
	const lockWait = 2 * time.Second

	observer := testkit.DB(t)
	var database string
	if err := observer.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("read clone name: %v", err)
	}
	pg := testkit.Endpoints().Postgres
	openPool := func(dsn string) *sql.DB {
		t.Helper()
		pool, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open pool on %s: %v", pg.Redacted(database), err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		if err := pool.PingContext(context.Background()); err != nil {
			t.Fatalf("ping pool on %s: %v", pg.Redacted(database), err)
		}
		return pool
	}
	holderPool := openPool(pg.URL(database))
	waiterPool := openPool(pg.URL(database) + "&statement_timeout=500")

	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- NewPostgresIdentityRefreshLock(holderPool, lockWait).WithDIDLock(context.Background(), did,
			func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
	}()
	releaseOnce := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(releaseOnce)
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatalf("holder returned before taking the lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not take the lock within 5s")
	}

	called := false
	err := NewPostgresIdentityRefreshLock(waiterPool, lockWait).WithDIDLock(context.Background(), did,
		func(context.Context) error {
			called = true
			return nil
		})

	if called {
		t.Errorf("waiter callback ran although another refresh held the lock")
	}
	for _, target := range []error{ErrIdentityRefreshBusy, ErrUnresolvedReference, errRedriveDeferred} {
		if !errors.Is(err, target) {
			t.Errorf("waiter error = %v, want one wrapping %v", err, target)
		}
	}
	if open := waiterPool.Stats().OpenConnections; open != 1 {
		t.Errorf("waiter pool open connections = %d, want 1 (the lock connection returned to the pool)", open)
	}

	releaseOnce()
	select {
	case err := <-holderDone:
		if err != nil {
			t.Errorf("holder: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not return within 5s of release")
	}
}
