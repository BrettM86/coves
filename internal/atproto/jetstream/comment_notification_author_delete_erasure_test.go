//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestCommentConsumer_AuthorDeleteErasureFirstWaitsOnAdvisoryLockBeforeCommentRow(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeleteerasure",
		fixture.replyRecord("A replies to B before erasure", fixture.postURI, fixture.postCID))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'postReply'`, commentURI, revTestAuthor))
	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	deleteEvent := revCommitEvent(revTestCommenter, CommentCollection, "delete", key, deleteRevision,
		"", time.Now().UnixMicro(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, 1)
	started, finished := false, false
	// Register before the open transaction: its cleanup rolls back the fixture
	// lock before this cleanup waits for the blocked consumer to finish.
	t.Cleanup(func() {
		if started && !finished {
			commentErasureResult(t, ctx, results, "HandleEvent(delete) after fixture rollback")
		}
	})
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, fixture.db, revTestCommenter)
	_, err := transaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+postgres.ErasureLockKeySQL+")", revTestCommenter)
	require.NoError(t, err)
	_, err = transaction.ExecContext(ctx, `INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, revTestCommenter)
	require.NoError(t, err)
	commentDelete, err := transaction.ExecContext(ctx, `DELETE FROM comments WHERE commenter_did = $1`, revTestCommenter)
	require.NoError(t, err)
	commentRows, err := commentDelete.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, commentRows, "fixture erasure must hold Y's comment row")
	_, err = transaction.ExecContext(ctx, `DELETE FROM notifications WHERE actor_did = $1`, revTestCommenter)
	require.NoError(t, err)

	started = true
	go func() { results <- fixture.consumer.HandleEvent(ctx, deleteEvent) }()
	consumerProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "pg_advisory_xact_lock_shared")
	var waitingOnlyForErasure bool
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks waiter
		JOIN pg_locks holder ON holder.locktype = waiter.locktype
			AND holder.database = waiter.database AND holder.classid = waiter.classid
			AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
		WHERE waiter.pid = $1 AND holder.pid = $2
			AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
			AND NOT waiter.granted AND waiter.mode = 'ShareLock'
			AND NOT EXISTS (SELECT 1 FROM pg_locks other WHERE other.pid = $1
				AND NOT other.granted AND other.locktype IN ('tuple', 'transactionid'))
			AND NOT EXISTS (SELECT 1 FROM pg_locks content WHERE content.pid = $1
				AND content.relation = 'comments'::regclass)
	)`, consumerProcessID, fixtureProcessID).Scan(&waitingOnlyForErasure))
	require.True(t, waitingOnlyForErasure,
		"author delete must wait for erasure's exclusive advisory lock before holding or waiting on the comments row")

	require.NoError(t, transaction.Commit(), "finish fixture account erasure")
	commentErasureResult(t, ctx, results, "HandleEvent(delete) after account erasure")
	finished = true
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"erasure removed Y before the author delete resumed")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"erasure and the author delete must leave no notifications for Y")
}

func TestCommentConsumer_AuthorDeleteErasureGateRequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeleteisolation",
		fixture.replyRecord("A replies to B before deletion", fixture.postURI, fixture.postCID))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))

	ctx := context.Background()
	var database string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	_, err := fixture.db.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(database)+" SET default_transaction_isolation = 'repeatable read'")
	require.NoError(t, err)
	fixture.db.SetMaxIdleConns(0)
	control, err := fixture.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var defaultIsolation string
	require.NoError(t, control.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&defaultIsolation))
	require.NoError(t, control.Rollback())
	require.Equal(t, "repeatable read", defaultIsolation,
		"control: a transaction without explicit options must inherit the database default")

	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, deleteRevision, "", time.Now().UnixMicro(), nil,
	)), "an applied author delete must request read committed for its erasure gate")
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT deleted_at FROM comments WHERE uri = $1`, commentURI).Scan(&deletedAt))
	require.True(t, deletedAt.Valid, "author delete must soft-delete the comment")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"author delete must keep Y's notification under READ COMMITTED")
}
