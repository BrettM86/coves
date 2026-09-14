//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotContinuationPinsSnapshotAgainstConcurrentCleanup(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	expected := seedDiscoverHotSnapshotCandidates(t, db, "cleanup-race", []int{400, 300, 200, 100})
	repository := NewDiscoverRepository(db, "cleanup-race-secret").(*postgresDiscoverRepo)
	repository.discoverHotWorkDeadline = 10 * time.Second

	firstPage, inputCursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, expected[:1], discoverURIs(firstPage))
	require.NotNil(t, inputCursor)

	var snapshotID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM discover_hot_snapshots`).Scan(&snapshotID))
	lockConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lockConnection.Close()) })
	lockKey := discoverHotCheckpointAdmissionLockKey()
	_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey)
	require.NoError(t, err)
	locked := true
	t.Cleanup(func() {
		if locked {
			_, unlockErr := lockConnection.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
			require.NoError(t, unlockErr)
		}
	})
	var lockHolderPID int
	require.NoError(t, lockConnection.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&lockHolderPID))
	_, err = db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = clock_timestamp() + INTERVAL '3 seconds'
		WHERE id = $1
	`, snapshotID)
	require.NoError(t, err)

	type continuationResult struct {
		feed   []*discover.FeedViewPost
		cursor *string
		err    error
	}
	continuationResults := make(chan continuationResult, 1)
	go func() {
		feed, cursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{
			Sort:   "hot",
			Limit:  1,
			Cursor: inputCursor,
		})
		continuationResults <- continuationResult{feed: feed, cursor: cursor, err: err}
	}()

	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		var waiting bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks waiter
				INNER JOIN pg_locks holder
					ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database
					AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid
					AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1
					AND holder.granted
					AND NOT waiter.granted
			)
		`, lockHolderPID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("Discover Hot continuation waiting to persist its checkpoint"))
	testkit.WaitFor(t, 4*time.Second, func() (bool, error) {
		var expired bool
		err := db.QueryRowContext(ctx, `
			SELECT expires_at <= clock_timestamp()
			FROM discover_hot_snapshots
			WHERE id = $1
		`, snapshotID).Scan(&expired)
		return expired, err
	}, testkit.WithDescription("continuation snapshot to reach its absolute expiry"))

	cleanupContext, cancelCleanup := context.WithTimeout(ctx, time.Second)
	cleanupResults := make(chan error, 1)
	go func() {
		_, cleanupErr := repository.CleanupExpiredDiscoverHotState(cleanupContext, 100)
		cleanupResults <- cleanupErr
	}()
	<-cleanupResults
	cancelCleanup()

	_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey)
	require.NoError(t, err)
	locked = false
	result := <-continuationResults
	require.NoError(t, result.err,
		"cleanup must not delete a snapshot after its continuation was admitted")
	assert.Equal(t, expected[1:2], discoverURIs(result.feed))
	require.NotNil(t, result.cursor, "remaining snapshot candidates require a continuation cursor")

	updateResult, err := db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = clock_timestamp() + INTERVAL '1 hour'
		WHERE id = $1
	`, snapshotID)
	require.NoError(t, err)
	updated, err := updateResult.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated, "the admitted continuation must retain its snapshot")
	remaining, finalCursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{
		Sort:   "hot",
		Limit:  10,
		Cursor: result.cursor,
	})
	require.NoError(t, err, "the successful continuation must persist a usable cursor")
	assert.Equal(t, expected[2:], discoverURIs(remaining))
	assert.Nil(t, finalCursor)
}

func TestDiscoverRepo_HotContinuationRechecksExpiryAfterWaitingForSnapshotLock(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	expected := seedDiscoverHotSnapshotCandidates(t, db, "cleanup-expiry-lock", []int{400, 300, 200, 100})
	repository := NewDiscoverRepository(db, "cleanup-expiry-lock-secret").(*postgresDiscoverRepo)
	repository.discoverHotWorkDeadline = 10 * time.Second

	firstPage, inputCursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, expected[:1], discoverURIs(firstPage))
	require.NotNil(t, inputCursor)

	var snapshotID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM discover_hot_snapshots`).Scan(&snapshotID))
	_, err = db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = clock_timestamp() + INTERVAL '2 seconds'
		WHERE id = $1
	`, snapshotID)
	require.NoError(t, err)

	gateConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, gateConnection.Close()) })
	gateTransaction, err := gateConnection.BeginTx(ctx, nil)
	require.NoError(t, err)
	gateOpen := true
	t.Cleanup(func() {
		if gateOpen {
			require.NoError(t, gateTransaction.Rollback())
		}
	})
	_, err = gateTransaction.ExecContext(ctx, `LOCK TABLE discover_hot_checkpoints IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	type continuationResult struct {
		feed   []*discover.FeedViewPost
		cursor *string
		err    error
	}
	continuationResults := make(chan continuationResult, 1)
	go func() {
		feed, cursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{
			Sort:   "hot",
			Limit:  1,
			Cursor: inputCursor,
		})
		continuationResults <- continuationResult{feed: feed, cursor: cursor, err: err}
	}()

	var continuationPID int
	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		err := db.QueryRowContext(ctx, `
			SELECT pid
			FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND query LIKE '%FROM discover_hot_checkpoints checkpoint%'
				AND wait_event_type = 'Lock'
			LIMIT 1
		`).Scan(&continuationPID)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("Discover Hot continuation transaction to start before snapshot expiry"))
	require.Positive(t, continuationPID)

	testkit.WaitFor(t, 4*time.Second, func() (bool, error) {
		var expired bool
		err := db.QueryRowContext(ctx, `
			SELECT expires_at <= clock_timestamp()
			FROM discover_hot_snapshots
			WHERE id = $1
		`, snapshotID).Scan(&expired)
		return expired, err
	}, testkit.WithDescription("continuation snapshot to expire while its transaction is waiting"))

	cleanupConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanupConnection.Close()) })
	cleanupTransaction, err := cleanupConnection.BeginTx(ctx, nil)
	require.NoError(t, err)
	cleanupOpen := true
	t.Cleanup(func() {
		if cleanupOpen {
			require.NoError(t, cleanupTransaction.Rollback())
		}
	})
	var lockedSnapshotID int64
	require.NoError(t, cleanupTransaction.QueryRowContext(ctx, `
		SELECT id FROM discover_hot_snapshots WHERE id = $1 FOR UPDATE
	`, snapshotID).Scan(&lockedSnapshotID))
	require.Equal(t, snapshotID, lockedSnapshotID)
	_, err = cleanupTransaction.ExecContext(ctx, `
		DELETE FROM discover_hot_candidates
		WHERE snapshot_id = $1 AND uri = $2
	`, snapshotID, expected[1])
	require.NoError(t, err, "fixture cleanup must partially drain the expired snapshot before continuation locks it")

	require.NoError(t, gateTransaction.Commit())
	gateOpen = false
	require.NoError(t, cleanupTransaction.Commit())
	cleanupOpen = false

	result := <-continuationResults
	assert.ErrorIs(t, result.err, discover.ErrInvalidCursor,
		"expiry must be checked after acquiring the snapshot lock, not against the transaction's pre-wait timestamp")
	assert.Empty(t, result.feed, "an expired, partially-cleaned snapshot must never be paged")
	assert.Nil(t, result.cursor)
}
