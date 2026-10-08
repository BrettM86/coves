//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notificationRaceTransaction(t *testing.T, db *sql.DB, ctx context.Context) (*sql.Tx, int) {
	t.Helper()
	connection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeError := connection.Close()
		require.True(t, closeError == nil || errors.Is(closeError, sql.ErrConnDone), "closing fixture connection: %v", closeError)
	})
	transaction, err := connection.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := transaction.Rollback()
		assert.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone), "rolling back fixture transaction: %v", rollbackError)
	})
	var processID int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID))
	return transaction, processID
}

// notificationRaceTryErasureLockShared tries the shared erasure lock for did on a
// backend other than excludedProcessID and reports whether it was granted. A held
// sql.Conn is never handed out twice, so when the first connection lands on the
// excluded backend the second one cannot. Both connections go back to the pool
// before returning; the xact lock taken in autocommit ends with its statement.
func notificationRaceTryErasureLockShared(t *testing.T, db *sql.DB, ctx context.Context, excludedProcessID int, did string) bool {
	t.Helper()
	closeConnection := func(connection *sql.Conn) {
		closeError := connection.Close()
		assert.True(t, closeError == nil || errors.Is(closeError, sql.ErrConnDone), "closing probe connection: %v", closeError)
	}
	backendProcessID := func(connection *sql.Conn) int {
		var processID int
		require.NoError(t, connection.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID))
		return processID
	}
	probeConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	defer closeConnection(probeConnection)
	if backendProcessID(probeConnection) == excludedProcessID {
		secondConnection, err := db.Conn(ctx)
		require.NoError(t, err)
		defer closeConnection(secondConnection)
		require.NotEqual(t, excludedProcessID, backendProcessID(secondConnection), "the probe connection must not share Delete's backend")
		probeConnection = secondConnection
	}
	var acquired bool
	require.NoError(t, probeConnection.QueryRowContext(ctx,
		"SELECT pg_try_advisory_xact_lock_shared("+ErasureLockKeySQL+")", did).Scan(&acquired))
	return acquired
}

func notificationRaceBlockedDelete(t *testing.T, db *sql.DB, ctx context.Context, blockingProcessID int, queryFragment string) {
	t.Helper()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var processID int
		err := db.QueryRowContext(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND $1 = ANY(pg_blocking_pids(pid))
				AND wait_event_type = 'Lock'
				AND query ILIKE $2
			LIMIT 1`, blockingProcessID, "%"+queryFragment+"%").Scan(&processID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("user deletion blocked by the fixture transaction at %s", queryFragment))
}

func notificationRaceRequirePending(t *testing.T, results <-chan error) {
	t.Helper()
	select {
	case err := <-results:
		t.Fatalf("Delete returned before the fixture transaction committed: %v", err)
	default:
	}
}

func notificationRaceDeadlock(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && databaseError.Code == "40P01"
}

func TestUserRepo_Delete_NotificationActorWaitsForErasureAdvisoryLock(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actorDID := "did:plc:notificationraceactor" + testkit.UniqueID(t)
	createTestUser(t, db, "notificationraceactor"+testkit.UniqueID(t)+".test", actorDID)
	transaction, holderProcessID := notificationRaceTransaction(t, db, ctx)
	_, err := transaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", actorDID)
	require.NoError(t, err)

	results := make(chan error, 1)
	go func() { results <- NewUserRepository(db).Delete(ctx, actorDID) }()

	var waiterProcessID int
	var waiterMode, holderMode string
	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		err := db.QueryRowContext(ctx, `
			SELECT waiter.pid, waiter.mode, holder.mode
			FROM pg_locks waiter
			JOIN pg_locks holder ON holder.locktype = waiter.locktype
				AND holder.database = waiter.database
				AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid
				AND holder.objsubid = waiter.objsubid
			WHERE holder.pid = $1 AND waiter.pid <> holder.pid
				AND holder.locktype = 'advisory'
				AND holder.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND waiter.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND holder.granted AND NOT waiter.granted
			LIMIT 1`, holderProcessID).Scan(&waiterProcessID, &waiterMode, &holderMode)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("actor deletion waiting for its erasure advisory lock before the marker"))
	require.Equal(t, "ExclusiveLock", waiterMode)
	require.Equal(t, "ShareLock", holderMode)
	var lockCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_locks
		WHERE pid = $1 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND relation = 'deleted_accounts'::regclass`, waiterProcessID).Scan(&lockCount))
	require.Zero(t, lockCount, "the erasure marker table must not yet be locked by Delete")
	notificationRaceRequirePending(t, results)

	require.NoError(t, transaction.Commit())
	require.NoError(t, <-results)
	var markerCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_accounts WHERE did = $1`, actorDID).Scan(&markerCount))
	require.Equal(t, 1, markerCount, "the erasure marker must be written once the advisory lock is released")
	require.True(t, notificationRaceTryErasureLockShared(t, db, ctx, waiterProcessID, actorDID), "the erasure lock must be transaction-scoped; a session-level lock would leak on the pooled connection")
	var userCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE did = $1`, actorDID).Scan(&userCount))
	require.Zero(t, userCount)
}

func TestUserRepo_Delete_NotificationRecipientWaitsForConcurrentInsert(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	recipientDID := "did:plc:notificationracerecipient" + testkit.UniqueID(t)
	createTestUser(t, db, "notificationracerecipient"+testkit.UniqueID(t)+".test", recipientDID)
	transaction, transactionProcessID := notificationRaceTransaction(t, db, ctx)
	rootPostURI := "at://" + recipientDID + "/social.coves.community.post/root"
	_, err := transaction.ExecContext(ctx, `
		INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri)
		VALUES ($1, 'postReply', $2, 'bafyreply', $3, $4, $4)`,
		recipientDID, "at://did:plc:notificationother/social.coves.community.comment/reply",
		"did:plc:notificationother", rootPostURI)
	require.NoError(t, err)

	results := make(chan error, 1)
	go func() { results <- NewUserRepository(db).Delete(ctx, recipientDID) }()
	notificationRaceBlockedDelete(t, db, ctx, transactionProcessID, "DELETE FROM users")
	notificationRaceRequirePending(t, results)

	require.NoError(t, transaction.Commit())
	require.NoError(t, <-results)
	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE recipient_did = $1`, recipientDID).Scan(&remaining))
	require.Zero(t, remaining, "the committed notification must be erased")
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notifications n
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.did = n.recipient_did)`).Scan(&remaining))
	require.Zero(t, remaining, "no notification may outlive its recipient")
}

func TestUserRepo_Delete_NotificationGroupUpsertAfterContentLock(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	suffix := testkit.UniqueID(t)
	recipientDID := "did:plc:notificationraceowner" + suffix
	communityDID := "did:plc:notificationracecommunity" + suffix
	createTestUser(t, db, "notificationraceowner"+suffix+".test", recipientDID)
	createTestCommunity(t, db, communityDID, "c.notificationrace"+suffix, recipientDID)
	postURI := "at://" + recipientDID + "/social.coves.community.post/owned"
	_, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, upvote_count)
		VALUES ($1, 'bafyracepost', 'owned', $2, $3, 'Race post', NOW(), 0)`, postURI, recipientDID, communityDID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $2)`, recipientDID, postURI)
	require.NoError(t, err)

	transaction, transactionProcessID := notificationRaceTransaction(t, db, ctx)
	_, err = transaction.ExecContext(ctx, `UPDATE posts SET upvote_count = upvote_count + 1 WHERE uri = $1`, postURI)
	require.NoError(t, err)
	results := make(chan error, 1)
	go func() { results <- NewUserRepository(db).Delete(ctx, recipientDID) }()
	notificationRaceBlockedDelete(t, db, ctx, transactionProcessID, "DELETE FROM posts")
	notificationRaceRequirePending(t, results)

	_, upsertError := transaction.ExecContext(ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $2, NOW())
		ON CONFLICT (recipient_did, subject_uri) WHERE reason = 'upvote'
		DO UPDATE SET sort_at = GREATEST(notifications.sort_at, EXCLUDED.sort_at)`, recipientDID, postURI)
	require.NoErrorf(t, upsertError, "upsert failed (SQLSTATE 40P01 deadlock: %t)", notificationRaceDeadlock(upsertError))
	commitError := transaction.Commit()
	require.NoErrorf(t, commitError, "commit failed (SQLSTATE 40P01 deadlock: %t)", notificationRaceDeadlock(commitError))
	deleteError := <-results
	require.NoErrorf(t, deleteError, "Delete failed (SQLSTATE 40P01 deadlock: %t)", notificationRaceDeadlock(deleteError))
	var groupCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		recipientDID, postURI).Scan(&groupCount))
	require.Zero(t, groupCount, "recipient erasure must remove the upvote group after the consumer commits")
}
