//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_ErasureGateTx_CommittedMarker(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	suffix := testkit.UniqueID(t)
	activeDID := "did:plc:notificationactive" + suffix
	erasedDID := "did:plc:notificationerased" + suffix
	_, err := db.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, erasedDID)
	require.NoError(t, err)

	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	repository := NewNotificationRepository(db)
	erased, err := repository.ErasureGateTx(ctx, transaction, activeDID)
	require.NoError(t, err)
	require.False(t, erased, "an actor without a deleted_accounts marker must pass the gate")
	erased, err = repository.ErasureGateTx(ctx, transaction, erasedDID)
	require.NoError(t, err)
	require.True(t, erased, "a committed deleted_accounts marker must stop the actor")
}

func TestNotificationRepository_ErasureGateTx_ChecksMarkerAfterWaitingForLock(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	actorDID := "did:plc:notificationgateactor" + testkit.UniqueID(t)
	fixtureTransaction, holderProcessID := notificationRaceTransaction(t, db, ctx)
	_, err := fixtureTransaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", actorDID)
	require.NoError(t, err)
	_, err = fixtureTransaction.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, actorDID)
	require.NoError(t, err)

	type gateStart struct {
		processID int
		err       error
	}
	type gateResult struct {
		erased bool
		err    error
	}
	started := make(chan gateStart, 1)
	results := make(chan gateResult, 1)
	go func() {
		transaction, beginError := db.BeginTx(ctx, nil)
		if beginError != nil {
			started <- gateStart{err: beginError}
			return
		}
		defer transaction.Rollback()
		var processID int
		if processError := transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID); processError != nil {
			started <- gateStart{err: processError}
			return
		}
		started <- gateStart{processID: processID}
		erased, gateError := NewNotificationRepository(db).ErasureGateTx(ctx, transaction, actorDID)
		results <- gateResult{erased: erased, err: gateError}
	}()

	var gateBackend gateStart
	select {
	case gateBackend = <-started:
	case <-ctx.Done():
		require.FailNow(t, "gate transaction did not start", ctx.Err().Error())
	}
	require.NoError(t, gateBackend.err)
	require.Positive(t, gateBackend.processID)

	returnedBeforeCommit := false
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case <-results:
			returnedBeforeCommit = true
			return true, nil
		default:
		}
		var waiting bool
		err := fixtureTransaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database
					AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid
					AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid = $2
					AND holder.locktype = 'advisory'
					AND holder.granted AND holder.mode = 'ExclusiveLock'
					AND NOT waiter.granted AND waiter.mode = 'ShareLock'
			)`, holderProcessID, gateBackend.processID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("ErasureGateTx waiting on the actor's exclusive erasure lock"))
	require.False(t, returnedBeforeCommit, "ErasureGateTx returned before acquiring the actor's shared erasure lock")
	select {
	case result := <-results:
		require.FailNowf(t, "ErasureGateTx returned before erasure committed", "result: erased=%t, error=%v", result.erased, result.err)
	default:
	}
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		select {
		case <-results:
			return false, nil
		default:
			return true, nil
		}
	}, testkit.WithDescription("ErasureGateTx remains blocked while erasure is uncommitted"))

	require.NoError(t, fixtureTransaction.Commit())
	select {
	case result := <-results:
		require.NoError(t, result.err)
		require.True(t, result.erased, "marker must be checked in a separate statement after the erasure lock is released")
	case <-ctx.Done():
		require.FailNow(t, "ErasureGateTx did not return after erasure committed", ctx.Err().Error())
	}
}

func TestNotificationRepository_ErasureGateTx_RejectsRepeatableRead(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	transaction, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer transaction.Rollback()
	_, err = NewNotificationRepository(db).ErasureGateTx(ctx, transaction,
		"did:plc:notificationrepeatable"+testkit.UniqueID(t))
	require.ErrorIs(t, err, ErrErasureGateRequiresReadCommitted,
		"a repeatable-read snapshot cannot observe a marker committed while waiting for the erasure lock")
}

func TestNotificationRepository_ErasureGateTx_RejectsRepeatableReadBeforeWaitingForLock(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	// Registered before the fixture so its rollback cleanup runs while ctx is
	// live. A deferred cancel fires first, and lib/pq then marks the still-open
	// fixture connection bad, so that rollback would race to driver.ErrBadConn.
	t.Cleanup(cancel)
	actorDID := "did:plc:notificationrepeatablewait" + testkit.UniqueID(t)
	fixtureTransaction, holderProcessID := notificationRaceTransaction(t, db, ctx)
	_, err := fixtureTransaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", actorDID)
	require.NoError(t, err)

	type gateStart struct {
		processID int
		err       error
	}
	started := make(chan gateStart, 1)
	results := make(chan error, 1)
	go func() {
		transaction, beginError := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if beginError != nil {
			started <- gateStart{err: beginError}
			return
		}
		defer transaction.Rollback()
		var processID int
		if processError := transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID); processError != nil {
			started <- gateStart{err: processError}
			return
		}
		started <- gateStart{processID: processID}
		_, gateError := NewNotificationRepository(db).ErasureGateTx(ctx, transaction, actorDID)
		results <- gateError
	}()

	var gateBackend gateStart
	select {
	case gateBackend = <-started:
	case <-ctx.Done():
		require.FailNow(t, "gate transaction did not start", ctx.Err().Error())
	}
	require.NoError(t, gateBackend.err)
	require.Positive(t, gateBackend.processID)

	// Ordering, not a deadline: the gate either returns or is seen queued behind
	// the fixture's erasure lock. The bound only stops a hang.
	var gateError error
	waitedOnLock := false
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		select {
		case gateError = <-results:
			return true, nil
		default:
		}
		err := fixtureTransaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database
					AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid
					AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid = $2
					AND holder.locktype = 'advisory'
					AND holder.granted AND NOT waiter.granted
			)`, holderProcessID, gateBackend.processID).Scan(&waitedOnLock)
		return waitedOnLock, err
	}, testkit.WithDescription("ErasureGateTx to return, or to queue behind the held erasure lock"))
	require.False(t, waitedOnLock,
		"ErasureGateTx waited on the held erasure lock; a repeatable-read transaction must be rejected before it takes the lock")
	require.ErrorIs(t, gateError, ErrErasureGateRequiresReadCommitted,
		"a repeatable-read transaction must be rejected before it waits on the held erasure lock")
	require.NoError(t, fixtureTransaction.Rollback(), "the fixture must still hold the erasure lock when the gate returns")
}

func TestNotificationRepository_ApplyTx_SkipsRecipientErasedDuringInsert(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	suffix := testkit.UniqueID(t)
	erasedRecipientDID := "did:plc:notificationrecipientb" + suffix
	remainingRecipientDID := "did:plc:notificationrecipientd" + suffix
	actorDID := "did:plc:notificationreplyactor" + suffix
	createTestUser(t, db, "notificationrecipientb"+suffix+".test", erasedRecipientDID)
	createTestUser(t, db, "notificationrecipientd"+suffix+".test", remainingRecipientDID)

	fixtureTransaction, holderProcessID := notificationRaceTransaction(t, db, ctx)
	deletion, err := fixtureTransaction.ExecContext(ctx, `DELETE FROM users WHERE did = $1`, erasedRecipientDID)
	require.NoError(t, err)
	deletedRows, err := deletion.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deletedRows)

	rootPostURI := "at://" + remainingRecipientDID + "/social.coves.community.postv2/root"
	intents := []notifications.Intent{
		{
			Reason: notifications.ReasonPostReply, RecipientDID: erasedRecipientDID, ActorDID: actorDID,
			RecordURI: "at://" + actorDID + "/social.coves.community.comment/replyb",
			RecordCID: "bafyreplyb", SubjectURI: "at://" + erasedRecipientDID + "/social.coves.community.postv2/parent",
			RootPostURI: "at://" + erasedRecipientDID + "/social.coves.community.postv2/parent", RecordCreatedAt: time.Now().UTC(),
		},
		{
			Reason: notifications.ReasonPostReply, RecipientDID: remainingRecipientDID, ActorDID: actorDID,
			RecordURI: "at://" + actorDID + "/social.coves.community.comment/replyd",
			RecordCID: "bafyreplyd", SubjectURI: rootPostURI,
			RootPostURI: rootPostURI, RecordCreatedAt: time.Now().UTC(),
		},
	}
	type insertStart struct {
		processID int
		err       error
	}
	type insertResult struct {
		applyError  error
		commitError error
	}
	started := make(chan insertStart, 1)
	results := make(chan insertResult, 1)
	go func() {
		transaction, beginError := db.BeginTx(ctx, nil)
		if beginError != nil {
			started <- insertStart{err: beginError}
			return
		}
		defer transaction.Rollback()
		var processID int
		if processError := transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID); processError != nil {
			started <- insertStart{err: processError}
			return
		}
		started <- insertStart{processID: processID}
		applyError := NewNotificationRepository(db).ApplyTx(ctx, transaction, intents)
		if applyError != nil {
			results <- insertResult{applyError: applyError}
			return
		}
		results <- insertResult{commitError: transaction.Commit()}
	}()

	var insertBackend insertStart
	select {
	case insertBackend = <-started:
	case <-ctx.Done():
		require.FailNow(t, "notification insertion transaction did not start", ctx.Err().Error())
	}
	require.NoError(t, insertBackend.err)
	require.Positive(t, insertBackend.processID)

	returnedBeforeCommit := false
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case <-results:
			returnedBeforeCommit = true
			return true, nil
		default:
		}
		// The fixture transaction owns the probe connection; a third connection
		// is not available when the insert and delete each hold one.
		_, err := fixtureTransaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`)
		if err != nil {
			return false, err
		}
		var waiting bool
		err = fixtureTransaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid = $2 AND wait_event_type = 'Lock'
					AND query ILIKE '%INSERT INTO notifications%'
					AND $1 = ANY(pg_blocking_pids(pid))
			)`, holderProcessID, insertBackend.processID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("ApplyTx insert blocked by the recipient's uncommitted deletion"))
	require.False(t, returnedBeforeCommit, "ApplyTx returned before the recipient deletion committed")
	select {
	case result := <-results:
		require.FailNowf(t, "ApplyTx returned before the deletion committed", "apply error: %v; commit error: %v", result.applyError, result.commitError)
	default:
	}
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		select {
		case <-results:
			return false, nil
		default:
			return true, nil
		}
	}, testkit.WithDescription("ApplyTx remains blocked while recipient deletion is uncommitted"))

	require.NoError(t, fixtureTransaction.Commit())
	select {
	case result := <-results:
		require.NoError(t, result.applyError, "recipient FK failure after erasure must skip only that intent")
		require.NoError(t, result.commitError, "the enclosing transaction must remain usable after the recipient FK failure")
	case <-ctx.Done():
		require.FailNow(t, "ApplyTx did not return after recipient deletion committed", ctx.Err().Error())
	}
	var erasedCount, remainingCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE recipient_did = $1`, erasedRecipientDID).Scan(&erasedCount))
	require.Zero(t, erasedCount, "the erased recipient must have no notification")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE recipient_did = $1`, remainingRecipientDID).Scan(&remainingCount))
	require.Equal(t, 1, remainingCount, "the later recipient must still receive the reply")
}
