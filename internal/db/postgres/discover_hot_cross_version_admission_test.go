//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotBuildAndCandidateAdmissionSerializesAcrossAlgorithmVersions(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "cross-version-build-admission", []int{200, 100})
	lockKey := discoverHotCrossVersionAdmissionLockKey("snapshot-admission")
	lockConnection, lockHolderPID := holdDiscoverHotAdmissionLock(t, db, lockKey)

	results := make(chan discoverHotRequestResult, 1)
	go func() {
		feed, cursor, err := NewDiscoverRepository(db, "cross-version-build-admission-secret").GetDiscover(
			context.Background(), discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
		results <- discoverHotRequestResult{feed: discoverURIs(feed), cursor: cursor, err: err}
	}()

	requireDiscoverHotRequestWaitingForAdmissionLock(t, db, lockHolderPID, results,
		"a repository using another algorithm version's global build/candidate admission lock")
	releaseDiscoverHotAdmissionLock(t, lockConnection, lockKey)

	result := <-results
	require.NoError(t, result.err)
	assert.Equal(t, expected, result.feed)
	assert.Nil(t, result.cursor)
}

func TestDiscoverRepo_HotCheckpointAdmissionSerializesAcrossAlgorithmVersions(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "cross-version-checkpoint-admission", []int{300, 200, 100})
	repository := NewDiscoverRepository(db, "cross-version-checkpoint-admission-secret")
	firstPage, inputCursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, expected[:1], discoverURIs(firstPage))
	require.NotNil(t, inputCursor)

	lockKey := discoverHotCrossVersionAdmissionLockKey("checkpoint-admission")
	lockConnection, lockHolderPID := holdDiscoverHotAdmissionLock(t, db, lockKey)
	results := make(chan discoverHotRequestResult, 1)
	go func() {
		feed, cursor, err := NewDiscoverRepository(db, "cross-version-checkpoint-admission-secret").GetDiscover(
			context.Background(), discover.GetDiscoverRequest{Sort: "hot", Limit: 1, Cursor: inputCursor})
		results <- discoverHotRequestResult{feed: discoverURIs(feed), cursor: cursor, err: err}
	}()

	requireDiscoverHotRequestWaitingForAdmissionLock(t, db, lockHolderPID, results,
		"a repository using another algorithm version's global checkpoint admission lock")
	releaseDiscoverHotAdmissionLock(t, lockConnection, lockKey)

	result := <-results
	require.NoError(t, result.err)
	assert.Equal(t, expected[1:2], result.feed)
	require.NotNil(t, result.cursor)
}

type discoverHotRequestResult struct {
	feed   []string
	cursor *string
	err    error
}

func discoverHotCrossVersionAdmissionLockKey(name string) int64 {
	sum := sha256.Sum256([]byte("discover-hot-" + name))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func holdDiscoverHotAdmissionLock(t *testing.T, db *sql.DB, lockKey int64) (*sql.Conn, int) {
	t.Helper()

	connection, err := db.Conn(context.Background())
	require.NoError(t, err)
	locked := true
	t.Cleanup(func() {
		if locked {
			_, _ = connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
		}
		require.NoError(t, connection.Close())
	})
	_, err = connection.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, lockKey)
	require.NoError(t, err)

	var backendPID int
	require.NoError(t, connection.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&backendPID))
	t.Cleanup(func() { locked = false })
	return connection, backendPID
}

func releaseDiscoverHotAdmissionLock(t *testing.T, connection *sql.Conn, lockKey int64) {
	t.Helper()

	var released bool
	require.NoError(t, connection.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey).Scan(&released))
	require.True(t, released)
}

func requireDiscoverHotRequestWaitingForAdmissionLock(
	t *testing.T,
	db *sql.DB,
	lockHolderPID int,
	results <-chan discoverHotRequestResult,
	description string,
) {
	t.Helper()

	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		select {
		case result := <-results:
			return false, fmt.Errorf("GetDiscover completed without %s: feed=%v cursor=%v err=%v",
				description, result.feed, result.cursor, result.err)
		default:
		}

		var waiting bool
		err := db.QueryRowContext(context.Background(), `
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
	}, testkit.WithDescription("%s", description))
}
