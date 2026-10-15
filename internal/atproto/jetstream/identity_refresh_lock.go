package jetstream

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// identityRefreshUnlockTimeout bounds the release of a refresh lock. The
// release runs even after the refresh's own context has ended.
const identityRefreshUnlockTimeout = 5 * time.Second

// identityRefreshStatementTimeoutMargin keeps the lock connection's
// statement_timeout far enough above lock_timeout that a busy wait always ends
// as lock_timeout, while every statement on the connection stays bounded.
const identityRefreshStatementTimeoutMargin = 5 * time.Second

// IdentityRefreshLockWaitTimeout bounds how long a refresh waits for its DID's
// lock. A user refresh holds the lock for at most one identityResolveTimeout
// plus a few short writes. A community refresh also runs the hostedBy did:web
// verification inside the lock, bounded by wellKnownVerificationTimeout (the
// .well-known limiter wait plus the fetch). The wait covers the longer holder
// plus 5s for the writes. It stays bounded so a holder whose host is
// partitioned cannot stall a serial consumer lane until TCP keepalive notices.
const IdentityRefreshLockWaitTimeout = identityResolveTimeout + wellKnownVerificationTimeout + 5*time.Second

// ErrIdentityRefreshBusy means another refresh of the same DID held the lock
// past the wait bound. The refresh never started, so it is safe to retry later.
var ErrIdentityRefreshBusy = errors.New("identity refresh busy")

// lockNotAvailableCode is the SQLSTATE PostgreSQL reports when lock_timeout
// expires. The statement is canceled without the lock being granted, so the
// connection remains usable.
const lockNotAvailableCode = "55P03"

// IdentityRefreshLock serializes handle refreshes of one DID across every
// process that shares the database.
type IdentityRefreshLock interface {
	WithDIDLock(ctx context.Context, did string, fn func(context.Context) error) error
}

type postgresIdentityRefreshLock struct {
	db          *sql.DB
	waitTimeout time.Duration
}

// NewPostgresIdentityRefreshLock returns a lock that holds a session-scoped
// PostgreSQL advisory lock per DID on a dedicated connection from db. Every
// refresh, waiting or holding, keeps one connection from db until it finishes,
// so db should not be the pool the refresh writes through. waitTimeout bounds
// each wait for a DID's lock. A nil db or a non-positive waitTimeout (which
// PostgreSQL would read as no bound) returns a nil lock, which refresh callers
// refuse.
func NewPostgresIdentityRefreshLock(db *sql.DB, waitTimeout time.Duration) IdentityRefreshLock {
	if db == nil || waitTimeout <= 0 {
		return nil
	}
	return &postgresIdentityRefreshLock{db: db, waitTimeout: waitTimeout}
}

// identityRefreshLockKey derives the advisory lock key from a namespaced
// digest, so it cannot collide with the other advisory locks in the tree.
func identityRefreshLockKey(did string) int64 {
	digest := sha256.Sum256([]byte("coves/identity-refresh/" + did))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// WithDIDLock runs fn while holding the DID's advisory lock. The wait is
// bounded by the lock's wait timeout: when it runs out, fn does not run and the
// error wraps ErrIdentityRefreshBusy as a deferred ErrUnresolvedReference. If
// ctx has ended by the time the wait finishes, also when the lock was just
// granted, fn does not run and the error wraps ctx's error. Neither error is
// ErrPermanentEvent. The lock is released after fn returns or panics, also
// when ctx has ended by then.
func (l *postgresIdentityRefreshLock) WithDIDLock(ctx context.Context, did string, fn func(context.Context) error) error {
	connection, err := l.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get identity refresh lock connection for %s: %w", did, err)
	}
	defer func() { _ = connection.Close() }()

	lockKey := identityRefreshLockKey(did)
	if err := l.acquire(ctx, connection, lockKey, did); err != nil {
		return err
	}
	// lib/pq closes the connection, releasing the lock server-side, when ctx
	// ends just as the lock is granted, yet reports success. Discard the
	// connection rather than release a lock it may no longer hold.
	if err := ctx.Err(); err != nil {
		discardIdentityRefreshLockConnection(connection)
		return fmt.Errorf("acquire identity refresh lock for %s: %w", did, err)
	}
	defer releaseIdentityRefreshLock(ctx, connection, lockKey, did)
	return fn(ctx)
}

func (l *postgresIdentityRefreshLock) acquire(ctx context.Context, connection *sql.Conn, lockKey int64, did string) error {
	// Both are session settings. The pool exists only for these locks, so it
	// is fine for them to outlive the refresh. PostgreSQL reads a bare integer
	// as milliseconds. The pool's DSN may carry a statement_timeout shorter
	// than the wait, which would cancel a busy wait as 57014 instead of
	// 55P03, so the connection's statement_timeout is raised above the wait.
	for _, setting := range []string{
		fmt.Sprintf("SET lock_timeout = %d", l.waitTimeout.Milliseconds()),
		fmt.Sprintf("SET statement_timeout = %d", (l.waitTimeout + identityRefreshStatementTimeoutMargin).Milliseconds()),
	} {
		if _, err := connection.ExecContext(ctx, setting); err != nil {
			discardIdentityRefreshLockConnection(connection)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("bound identity refresh lock wait for %s: %w", did, ctxErr)
			}
			return fmt.Errorf("bound identity refresh lock wait for %s: %w", did, err)
		}
	}
	if _, err := connection.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		var pqError *pq.Error
		if errors.As(err, &pqError) && pqError.Code == lockNotAvailableCode {
			// The lock was not granted, so the connection can go back to the pool.
			return deferRedrive(fmt.Errorf("%w: %w: another refresh of %s held the lock for %s",
				ErrUnresolvedReference, ErrIdentityRefreshBusy, did, l.waitTimeout))
		}
		// A cancelled wait can race the grant. Discard the connection so a lock
		// it may hold never goes back to the pool.
		discardIdentityRefreshLockConnection(connection)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("acquire identity refresh lock for %s: %w", did, ctxErr)
		}
		return fmt.Errorf("acquire identity refresh lock for %s: %w", did, err)
	}
	return nil
}

// releaseIdentityRefreshLock does not change the refresh's outcome: by the
// time it runs the handle write has committed. Discarding the connection
// closes its session, which releases the lock server-side.
func releaseIdentityRefreshLock(ctx context.Context, connection *sql.Conn, lockKey int64, did string) {
	unlockContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), identityRefreshUnlockTimeout)
	defer cancel()
	var released bool
	err := connection.QueryRowContext(unlockContext, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&released)
	if err == nil && released {
		return
	}
	discardIdentityRefreshLockConnection(connection)
	if err == nil {
		err = errors.New("identity refresh lock was not held")
	}
	slog.Error("failed to release identity refresh lock; discarding its connection",
		"error", err, "did", did)
}

func discardIdentityRefreshLockConnection(connection *sql.Conn) {
	// database/sql closes the physical connection when Raw returns ErrBadConn.
	_ = connection.Raw(func(any) error { return driver.ErrBadConn })
}

// WithUserIdentityRefreshLock sets the lock that serializes identity refreshes
// of one DID. Without it the consumer refuses identity refreshes.
func WithUserIdentityRefreshLock(lock IdentityRefreshLock) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.identityRefreshLock = lock
	}
}

// HasIdentityRefreshLock reports whether an identity refresh lock is set.
func (c *UserEventConsumer) HasIdentityRefreshLock() bool { return c.identityRefreshLock != nil }

// WithCommunityIdentityRefreshLock sets the lock that serializes identity
// refreshes of one community DID. Without it the consumer refuses identity
// refreshes.
func WithCommunityIdentityRefreshLock(lock IdentityRefreshLock) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) {
		c.identityRefreshLock = lock
	}
}

// HasIdentityRefreshLock reports whether an identity refresh lock is set.
func (c *CommunityEventConsumer) HasIdentityRefreshLock() bool { return c.identityRefreshLock != nil }
