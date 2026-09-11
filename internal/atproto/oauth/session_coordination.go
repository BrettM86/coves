package oauth

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

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/lib/pq"
)

// ErrSessionBusy means another operation held the same OAuth session for longer
// than sessionLockTimeout. The caller's work never started, so it is safe to
// retry once the earlier operation finishes.
var ErrSessionBusy = errors.New("OAuth session busy")

// sessionLockTimeout bounds how long a same-session operation waits for the
// session advisory lock. Request contexts carry no deadline, and the holder can
// spend a full PDS round trip (with refresh and retry) inside the lock, so an
// unbounded wait would pin a coordination connection for as long as the holder.
const sessionLockTimeout = 10 * time.Second

// lockNotAvailableCode is the SQLSTATE PostgreSQL reports when lock_timeout
// expires. The statement is canceled without the lock being granted, so the
// connection remains usable.
const lockNotAvailableCode = "55P03"

type sessionOperationCoordinator interface {
	withSessionOperation(context.Context, syntax.DID, string, func(context.Context) error) error
}

type (
	sessionConnectionKey struct{}
	sessionConnection    struct {
		database   *sql.DB
		connection *sql.Conn
	}
)

type sessionDatabase interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// sessionDatabase routes session reads and writes onto the locked coordination
// connection while an operation is in progress, so persistence of rotated
// credentials happens on the connection that holds the session lock.
func (s *PostgresOAuthStore) sessionDatabase(ctx context.Context) sessionDatabase {
	if owned, ok := ctx.Value(sessionConnectionKey{}).(sessionConnection); ok && owned.database == s.coordination {
		return owned.connection
	}
	return s.db
}

func sessionLockKey(did syntax.DID, sessionID string) int64 {
	digest := sha256.Sum256([]byte("coves/oauth-session/" + did.String() + "\x00" + sessionID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// Hold a session advisory lock on one dedicated connection across reload,
// refresh and persistence. Session writes autocommit before a resource retry;
// a transaction lock would leave rotated credentials uncommitted during writes.
//
// The connection comes from the coordination pool, never the request pool, so
// a slow PDS write or a queue of same-session waiters cannot starve session
// lookups and feed queries. Each operation costs one coordination connection
// for its whole duration; the wait for the lock is bounded by
// sessionLockTimeout and reported as ErrSessionBusy.
func (s *PostgresOAuthStore) withSessionOperation(ctx context.Context, did syntax.DID, sessionID string, operation func(context.Context) error) error {
	connection, err := s.coordination.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	lockKey := sessionLockKey(did, sessionID)
	if err := acquireSessionLock(ctx, connection, lockKey, s.lockTimeout); err != nil {
		return err
	}
	defer s.releaseSessionLock(ctx, connection, lockKey, did, sessionID)
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation(context.WithValue(ctx, sessionConnectionKey{}, sessionConnection{database: s.coordination, connection: connection}))
}

func acquireSessionLock(ctx context.Context, connection *sql.Conn, lockKey int64, lockTimeout time.Duration) error {
	// lock_timeout is a session setting. The coordination pool exists only for
	// this lock, so it is fine for the setting to outlive the operation.
	// PostgreSQL reads a bare integer as milliseconds.
	if _, err := connection.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
		discardSessionConnection(connection)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("bound OAuth session lock wait: %w", err)
	}
	if _, err := connection.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		var pqError *pq.Error
		if errors.As(err, &pqError) && pqError.Code == lockNotAvailableCode {
			return fmt.Errorf("%w: another operation held the session for %s", ErrSessionBusy, lockTimeout)
		}
		// Cancellation can race acquisition; discard this physical connection so
		// no possibly-held session lock can return to the connection pool.
		discardSessionConnection(connection)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("coordinate OAuth session: %w", err)
	}
	return nil
}

// releaseSessionLock never changes the operation's outcome. By the time it runs
// the PDS mutation has already happened, so surfacing a cleanup failure would
// turn a committed write into a retryable error. Discarding the connection
// closes it, which releases the session lock server-side regardless.
func (s *PostgresOAuthStore) releaseSessionLock(ctx context.Context, connection *sql.Conn, lockKey int64, did syntax.DID, sessionID string) {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var released bool
	err := connection.QueryRowContext(cleanupContext, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&released)
	if err == nil && released {
		return
	}
	discardSessionConnection(connection)
	if err == nil {
		err = errors.New("OAuth session lock was not held")
	}
	slog.Error("failed to release OAuth session coordination; discarding its connection",
		"error", err, "did", did, "session_id", sessionID)
}

func discardSessionConnection(connection *sql.Conn) {
	// database/sql closes the physical connection when Raw returns ErrBadConn.
	_ = connection.Raw(func(any) error { return driver.ErrBadConn })
}

func (w *MobileAwareStoreWrapper) withSessionOperation(ctx context.Context, did syntax.DID, sessionID string, operation func(context.Context) error) error {
	if coordinator, ok := w.ClientAuthStore.(sessionOperationCoordinator); ok {
		return coordinator.withSessionOperation(ctx, did, sessionID, operation)
	}
	return operation(ctx)
}
