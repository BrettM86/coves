//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type commentErasureFixture struct {
	consumer     *CommentEventConsumer
	postURI      string
	postCID      string
	createdAt    string
	actorDID     string
	recipientDID string
}

func newCommentErasureFixture(t *testing.T, ctx context.Context, db *sql.DB) commentErasureFixture {
	t.Helper()
	_, postURI, postCID := setupRevFixtures(t, db)
	return commentErasureFixture{
		consumer: NewCommentEventConsumer(
			postgres.NewCommentRepository(db), db,
			WithCommentNotifications(postgres.NewNotificationRepository(db)),
		),
		postURI: postURI, postCID: postCID,
		createdAt: activatedCommentNotificationTime(t, db, ctx),
		actorDID:  revTestCommenter, recipientDID: revTestAuthor,
	}
}

func (fixture commentErasureFixture) reply() (*JetstreamEvent, string) {
	key := testkit.TID()
	record := revCommentRecord("A replies to B's post", fixture.postURI, fixture.postCID, fixture.postURI, fixture.postCID)
	record["createdAt"] = fixture.createdAt
	return revCommitEvent(fixture.actorDID, CommentCollection, "create", key,
			testkit.TID(), "bafycommenterasure", time.Now().UnixMicro(), record),
		"at://" + fixture.actorDID + "/" + CommentCollection + "/" + key
}

// The held transaction is also the observation connection: the clone pool has
// three connections, all occupied once Delete and HandleEvent have started.
func commentErasureLockTransaction(t *testing.T, ctx context.Context, db *sql.DB, did string) (*sql.Tx, int) {
	t.Helper()
	connection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	// Keep the fixture lock alive through worker-context cancellation, so test
	// cleanup can roll it back before returning the connection to the pool.
	transaction, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := transaction.Rollback()
		require.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone),
			"rolling back fixture transaction: %v", rollbackError)
	})
	var processID int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID))
	var lockedDID string
	require.NoError(t, transaction.QueryRowContext(ctx,
		`SELECT did FROM users WHERE did = $1 FOR UPDATE`, did).Scan(&lockedDID))
	require.Equal(t, did, lockedDID)
	return transaction, processID
}

func commentErasureBlockedByFixture(t *testing.T, ctx context.Context, transaction *sql.Tx, fixtureProcessID int, queryFragment string) int {
	t.Helper()
	var blockedProcessID int
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		// pg_stat_activity's snapshot persists inside this transaction otherwise.
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		err := transaction.QueryRowContext(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> $1
				AND wait_event_type = 'Lock'
				AND query ILIKE $2
				AND $1 = ANY(pg_blocking_pids(pid))
			LIMIT 1`, fixtureProcessID, "%"+queryFragment+"%").Scan(&blockedProcessID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("%s backend blocked by fixture user row lock", queryFragment))
	return blockedProcessID
}

func commentErasureResult(t *testing.T, ctx context.Context, results <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-results:
		var databaseError *pq.Error
		require.False(t, errors.As(err, &databaseError) && databaseError.Code == "40P01", "%s deadlocked: %v", operation, err)
		require.NoError(t, err, "%s failed", operation)
	case <-ctx.Done():
		require.FailNowf(t, operation+" did not finish", "context deadline: %v", ctx.Err())
	}
}

func TestCommentConsumer_NotificationErasure_RedriveErasedActor(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	fixture := newCommentErasureFixture(t, ctx, db)
	require.NoError(t, postgres.NewUserRepository(db).Delete(ctx, fixture.actorDID))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, fixture.actorDID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM users WHERE did = $1`, fixture.actorDID))

	event, commentURI := fixture.reply()
	require.NoError(t, fixture.consumer.HandleEvent(ctx, event))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"the accepted content-indexing gap still indexes an erased actor's comment")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, fixture.actorDID),
		"redriving an erased actor's comment must not notify the recipient")
}

func TestCommentConsumer_NotificationErasure_DeleteFirstWaitsBeforeContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newCommentErasureFixture(t, ctx, db)
	priorEvent, priorURI := fixture.reply()
	require.NoError(t, fixture.consumer.HandleEvent(ctx, priorEvent))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, priorURI),
		"fixture: A has content before deletion starts")

	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, fixture.actorDID)
	deleteResults := make(chan error, 1)
	go func() { deleteResults <- postgres.NewUserRepository(db).Delete(ctx, fixture.actorDID) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")

	event, _ := fixture.reply()
	consumerResults := make(chan error, 1)
	go func() { consumerResults <- fixture.consumer.HandleEvent(ctx, event) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var waitingBeforeContent bool
		err := transaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
					AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
					AND NOT waiter.granted AND waiter.mode = 'ShareLock'
					AND NOT EXISTS (SELECT 1 FROM pg_locks other
						WHERE other.pid = waiter.pid AND NOT other.granted
							AND other.locktype IN ('tuple', 'transactionid'))
					AND NOT EXISTS (SELECT 1 FROM pg_locks content
						WHERE content.pid = waiter.pid
							AND content.relation IN ('comments'::regclass, 'posts'::regclass))
			)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("consumer waiting for Delete's erasure advisory lock before reading comments or posts"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(A)")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, fixture.actorDID),
		"Delete-first interleaving must leave no reply notifications from erased A")
}

func TestCommentConsumer_NotificationErasure_ConsumerFirstPinsDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := newCommentErasureFixture(t, ctx, db)
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, fixture.recipientDID)
	event, commentURI := fixture.reply()
	consumerResults := make(chan error, 1)
	go func() { consumerResults <- fixture.consumer.HandleEvent(ctx, event) }()
	consumerProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "INSERT INTO notifications")

	deleteResults := make(chan error, 1)
	go func() { deleteResults <- postgres.NewUserRepository(db).Delete(ctx, fixture.actorDID) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var deleteWaitsForConsumer bool
		err := transaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
					AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ShareLock'
					AND NOT waiter.granted AND waiter.mode = 'ExclusiveLock'
			)`, consumerProcessID, fixtureProcessID).Scan(&deleteWaitsForConsumer)
		return deleteWaitsForConsumer, err
	}, testkit.WithDescription("Delete(A) waiting on consumer's granted shared erasure advisory lock"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(A)")
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, fixture.actorDID),
		"Delete must remove A's committed reply notification")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"Delete must remove A's newly indexed comment")
}
