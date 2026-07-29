//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise PostgresStateStore against the local test database
// (same container as the rest of the package's DB tests, port 5434 by
// default / TEST_DATABASE_URL).

func setupStateStoreTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err, "Failed to connect to test database")
	// Registered before the first thing that can fail, so the handle is closed
	// even when Ping or the migration below calls FailNow.
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM jetstream_cursors WHERE consumer_name LIKE 'statestore-test%'")
		_, _ = db.Exec("DELETE FROM jetstream_dead_letters WHERE consumer_name LIKE 'statestore-test%'")
		_ = db.Close()
	})
	// Reaching this file at all means `-tags integration` was passed, which is
	// a request for Postgres. An absent database is a failed run, not a
	// shrunken one.
	require.NoError(t, db.Ping(),
		"test database not reachable at %s; bring it up with `make test-db-reset`", redactedDSN(dsn))
	require.NoError(t, goose.Up(db, "../../db/migrations"), "Failed to run migrations")

	return db
}

func TestPostgresStateStore_CursorLifecycle(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-cursor"

	// Unknown consumer → 0, no error (first run live-tails).
	cursor, err := store.GetCursor(ctx, consumer)
	require.NoError(t, err)
	assert.Equal(t, int64(0), cursor)

	// Save and read back.
	require.NoError(t, store.SaveCursor(ctx, consumer, 1_000_000))
	cursor, err = store.GetCursor(ctx, consumer)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000), cursor)

	// Advance.
	require.NoError(t, store.SaveCursor(ctx, consumer, 2_000_000))
	cursor, err = store.GetCursor(ctx, consumer)
	require.NoError(t, err)
	assert.Equal(t, int64(2_000_000), cursor)

	// Monotonic: an out-of-order older flush must not rewind the cursor.
	require.NoError(t, store.SaveCursor(ctx, consumer, 1_500_000))
	cursor, err = store.GetCursor(ctx, consumer)
	require.NoError(t, err)
	assert.Equal(t, int64(2_000_000), cursor, "cursor must never move backwards")
}

func TestPostgresStateStore_DeadLetterLifecycle(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-dlq"

	// Add two dead letters — one valid JSON, one malformed payload (the
	// BYTEA column must accept both).
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 5_000, []byte(`{"kind":"commit","time_us":5000}`), "postgres blip", 0))
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 0, []byte("not json at all"), "parse failure", 0))

	rows, err := store.ListRetryable(ctx, consumer, 10, 100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, int64(5_000), rows[0].EventTimeUS, "oldest first")
	assert.Equal(t, `{"kind":"commit","time_us":5000}`, string(rows[0].EventData))
	assert.Equal(t, "postgres blip", rows[0].LastError)
	assert.Equal(t, 0, rows[0].Attempts)
	assert.Equal(t, "not json at all", string(rows[1].EventData))

	// Mark a redrive attempt: attempts increments, error updates.
	require.NoError(t, store.MarkRedriveAttempt(ctx, rows[0].ID, "still failing"))
	updated, err := store.ListRetryable(ctx, consumer, 10, 100)
	require.NoError(t, err)
	require.Len(t, updated, 2)
	assert.Equal(t, 1, updated[0].Attempts)
	assert.Equal(t, "still failing", updated[0].LastError)

	// Rows at the attempt budget stop being listed but remain counted.
	require.NoError(t, store.MarkRedriveAttempt(ctx, rows[0].ID, "still failing"))
	retryable, err := store.ListRetryable(ctx, consumer, 2, 100)
	require.NoError(t, err)
	require.Len(t, retryable, 1, "exhausted row must not be listed")
	assert.Equal(t, rows[1].ID, retryable[0].ID)

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), counts[consumer], "backlog counts include exhausted rows")

	// Delete a redriven row.
	require.NoError(t, store.DeleteDeadLetter(ctx, rows[1].ID))
	remaining, err := store.ListRetryable(ctx, consumer, 10, 100)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, rows[0].ID, remaining[0].ID)
}

// A byte-corrupt Jetstream frame (NUL bytes, invalid UTF-8) must be storable,
// otherwise the failed dead-letter write tears down the connection without
// advancing the cursor and the consumer replays the same frame forever.
func TestPostgresStateStore_DeadLetterBinaryPayload(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-binary"

	payload := []byte{0x00, 0xff, 0xfe}
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 7_000, payload, "byte-corrupt frame", 0),
		"BYTEA column must accept NUL bytes and invalid UTF-8")

	rows, err := store.ListRetryable(ctx, consumer, MaxRedriveAttempts, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, payload, rows[0].EventData, "binary payload must round-trip byte-for-byte")
}

// A poison event replayed by the reconnect rewind (or an unparseable frame
// that never advances the cursor) must not insert a fresh row — and a fresh
// redrive budget — on every reconnect.
func TestPostgresStateStore_DeadLetterDedup(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-dedup"

	payload := []byte(`{"kind":"commit","time_us":9000}`)
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 9_000, payload, "first failure", 0))
	// The duplicate must be a no-op success (already captured = the cursor
	// may advance), not an error and not a second row.
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 9_000, payload, "second failure", 0))

	rows, err := store.ListRetryable(ctx, consumer, MaxRedriveAttempts, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "duplicate dead letter must not create a second row")
	assert.Equal(t, "first failure", rows[0].LastError, "original row must be untouched")
}

// Permanent failures are inserted with their redrive budget already
// exhausted so the redriver never touches them.
func TestPostgresStateStore_PermanentDeadLetterInsertedExhausted(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-permanent"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 11_000, []byte(`{"kind":"commit","time_us":11000}`), "permanent rejection", MaxRedriveAttempts))

	retryable, err := store.ListRetryable(ctx, consumer, MaxRedriveAttempts, 100)
	require.NoError(t, err)
	assert.Empty(t, retryable, "permanent dead letter must never be listed for redrive")

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts[consumer], "permanent dead letter still counts in the backlog")
}

// RetireDeadLetter exhausts a row in one step (used for unparseable payloads
// discovered during redrive) while keeping it for forensics.
func TestPostgresStateStore_RetireDeadLetter(t *testing.T) {
	db := setupStateStoreTestDB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-retire"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 0, []byte("not json"), "parse failure", 0))
	rows, err := store.ListRetryable(ctx, consumer, MaxRedriveAttempts, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.NoError(t, store.RetireDeadLetter(ctx, rows[0].ID, "unparseable event"))

	retryable, err := store.ListRetryable(ctx, consumer, MaxRedriveAttempts, 100)
	require.NoError(t, err)
	assert.Empty(t, retryable, "retired row must not be listed for redrive")

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts[consumer], "retired row remains for forensics")
}
