//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise PostgresStateStore against a private testkit clone of
// the migrated template, so each test starts from an empty schema and nothing
// it writes is visible to any other test.

func allRetryableDeadLetters(consumer string, maxAttempts int) DeadLetterPageQuery {
	return DeadLetterPageQuery{
		ConsumerName: consumer,
		MaxAttempts:  maxAttempts,
		ThroughID:    1<<63 - 1,
		Limit:        100,
	}
}

func TestPostgresStateStore_CursorLifecycle(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
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
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-dlq"

	// Add two dead letters — one valid JSON, one malformed payload (the
	// BYTEA column must accept both).
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 5_000, []byte(`{"kind":"commit","time_us":5000}`), "postgres blip", 0))
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 0, []byte("not json at all"), "parse failure", 0))

	rows, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, 10))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, int64(5_000), rows[0].EventTimeUS, "oldest first")
	assert.Equal(t, `{"kind":"commit","time_us":5000}`, string(rows[0].EventData))
	assert.Equal(t, "postgres blip", rows[0].LastError)
	assert.Equal(t, 0, rows[0].Attempts)
	assert.Equal(t, "not json at all", string(rows[1].EventData))

	// Mark a redrive attempt: attempts increments, error updates.
	require.NoError(t, store.MarkRedriveAttempt(ctx, rows[0].ID, "still failing"))
	updated, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, 10))
	require.NoError(t, err)
	require.Len(t, updated, 2)
	assert.Equal(t, 1, updated[0].Attempts)
	assert.Equal(t, "still failing", updated[0].LastError)

	// Rows at the attempt budget stop being listed but remain counted.
	require.NoError(t, store.MarkRedriveAttempt(ctx, rows[0].ID, "still failing"))
	retryable, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, 2))
	require.NoError(t, err)
	require.Len(t, retryable, 1, "exhausted row must not be listed")
	assert.Equal(t, rows[1].ID, retryable[0].ID)

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), counts[consumer], "backlog counts include exhausted rows")

	// Delete a redriven row.
	require.NoError(t, store.DeleteDeadLetter(ctx, rows[1].ID))
	remaining, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, 10))
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, rows[0].ID, remaining[0].ID)
}

// A byte-corrupt Jetstream frame (NUL bytes, invalid UTF-8) must be storable,
// otherwise the failed dead-letter write tears down the connection without
// advancing the cursor and the consumer replays the same frame forever.
func TestPostgresStateStore_DeadLetterBinaryPayload(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-binary"

	payload := []byte{0x00, 0xff, 0xfe}
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 7_000, payload, "byte-corrupt frame", 0),
		"BYTEA column must accept NUL bytes and invalid UTF-8")

	rows, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, MaxRedriveAttempts))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, payload, rows[0].EventData, "binary payload must round-trip byte-for-byte")
}

// A poison event replayed by the reconnect rewind (or an unparseable frame
// that never advances the cursor) must not insert a fresh row — and a fresh
// redrive budget — on every reconnect.
func TestPostgresStateStore_DeadLetterDedup(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-dedup"

	payload := []byte(`{"kind":"commit","time_us":9000}`)
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 9_000, payload, "first failure", 0))
	// The duplicate must be a no-op success (already captured = the cursor
	// may advance), not an error and not a second row.
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 9_000, payload, "second failure", 0))

	rows, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, MaxRedriveAttempts))
	require.NoError(t, err)
	require.Len(t, rows, 1, "duplicate dead letter must not create a second row")
	assert.Equal(t, "first failure", rows[0].LastError, "original row must be untouched")
}

// Permanent failures are inserted with their redrive budget already
// exhausted so the redriver never touches them.
func TestPostgresStateStore_PermanentDeadLetterInsertedExhausted(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-permanent"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 11_000, []byte(`{"kind":"commit","time_us":11000}`), "permanent rejection", MaxRedriveAttempts))

	retryable, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, MaxRedriveAttempts))
	require.NoError(t, err)
	assert.Empty(t, retryable, "permanent dead letter must never be listed for redrive")

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts[consumer], "permanent dead letter still counts in the backlog")
}

// RetireDeadLetter exhausts a row in one step (used for unparseable payloads
// discovered during redrive) while keeping it for forensics.
func TestPostgresStateStore_RetireDeadLetter(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-retire"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 0, []byte("not json"), "parse failure", 0))
	rows, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, MaxRedriveAttempts))
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.NoError(t, store.RetireDeadLetter(ctx, rows[0].ID, "unparseable event"))

	retryable, err := store.ListRetryable(ctx, allRetryableDeadLetters(consumer, MaxRedriveAttempts))
	require.NoError(t, err)
	assert.Empty(t, retryable, "retired row must not be listed for redrive")

	counts, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts[consumer], "retired row remains for forensics")
}

func TestPostgresStateStore_RedriveSnapshotExcludesNewArrivals(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-snapshot"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 1, []byte(`{"kind":"commit","time_us":1}`), "failure", 0))
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 2, []byte(`{"kind":"commit","time_us":2}`), "failure", 0))
	throughID, err := store.LatestDeadLetterID(ctx, consumer)
	require.NoError(t, err)
	require.NotZero(t, throughID)

	// This row arrived after the pass took its high-water mark and must wait
	// for the next pass even while the first snapshot is paged.
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 3, []byte(`{"kind":"commit","time_us":3}`), "failure", 0))

	first, err := store.ListRetryable(ctx, DeadLetterPageQuery{
		ConsumerName: consumer,
		MaxAttempts:  MaxRedriveAttempts,
		ThroughID:    throughID,
		Limit:        1,
	})
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, int64(1), first[0].EventTimeUS)

	second, err := store.ListRetryable(ctx, DeadLetterPageQuery{
		ConsumerName: consumer,
		MaxAttempts:  MaxRedriveAttempts,
		AfterID:      first[0].ID,
		ThroughID:    throughID,
		Limit:        1,
	})
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, int64(2), second[0].EventTimeUS)

	done, err := store.ListRetryable(ctx, DeadLetterPageQuery{
		ConsumerName: consumer,
		MaxAttempts:  MaxRedriveAttempts,
		AfterID:      second[0].ID,
		ThroughID:    throughID,
		Limit:        1,
	})
	require.NoError(t, err)
	assert.Empty(t, done, "a row inserted after the snapshot leaked into the current pass")

	nextThroughID, err := store.LatestDeadLetterID(ctx, consumer)
	require.NoError(t, err)
	next, err := store.ListRetryable(ctx, DeadLetterPageQuery{
		ConsumerName: consumer,
		MaxAttempts:  MaxRedriveAttempts,
		AfterID:      throughID,
		ThroughID:    nextThroughID,
		Limit:        1,
	})
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, int64(3), next[0].EventTimeUS)
}

func TestPostgresStateStore_PrunesRowsOutsideRetentionWindow(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "statestore-test-retention"

	require.NoError(t, store.AddDeadLetter(ctx, consumer, 1, []byte("old"), "old failure", MaxRedriveAttempts))
	require.NoError(t, store.AddDeadLetter(ctx, consumer, 2, []byte("recent"), "recent failure", MaxRedriveAttempts))
	_, err := db.ExecContext(ctx, `
		UPDATE jetstream_dead_letters
		SET updated_at = NOW() - INTERVAL '8 days'
		WHERE consumer_name = $1 AND event_time_us = 1`, consumer)
	require.NoError(t, err)

	pruned, err := store.PruneDeadLetters(ctx, time.Now().Add(-DeadLetterRetention))
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)

	var remaining int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jetstream_dead_letters WHERE consumer_name = $1`, consumer).Scan(&remaining))
	assert.Equal(t, 1, remaining)
}
