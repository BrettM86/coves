//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostConsumer_NotificationErasure_DeleteFirstWaitsBeforeContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // Register before the fixture transaction, so cleanup rolls it back first.
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	event := pv2Event(pv2Author, "create", testkit.TID(), testkit.TID(),
		"bafyreiposterasure", time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))
	deleteResults, consumerResults := make(chan error, 1), make(chan error, 1)
	deleteStarted, deleteFinished := false, false
	consumerStarted, consumerFinished := false, false
	voteGroupResultsCleanup(t, ctx, deleteResults, &deleteStarted, &deleteFinished, "Delete(post author)")
	voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(post)")

	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, pv2Author)
	deleteStarted = true
	go func() { deleteResults <- postgres.NewUserRepository(db).Delete(ctx, pv2Author) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")

	consumerStarted = true
	go func() { consumerResults <- consumer.HandleEvent(ctx, event) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		var waitingBeforeContent bool
		err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks waiter
			JOIN pg_locks holder ON holder.locktype = waiter.locktype
				AND holder.database = waiter.database AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
			WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
				AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
				AND NOT waiter.granted AND waiter.mode = 'ShareLock'
				AND $1 = ANY(pg_blocking_pids(waiter.pid))
				AND NOT EXISTS (SELECT 1 FROM pg_locks content
					WHERE content.pid = waiter.pid AND content.locktype = 'relation'
						AND content.relation = 'posts'::regclass)
		)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("post consumer waits on Delete's erasure advisory lock before touching posts"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(post author)")
	deleteFinished = true
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(post)")
	consumerFinished = true
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, pv2Author),
		"Delete-first post ingestion must leave no notifications from the erased author")
}

func TestPostConsumer_NotificationErasure_GateRequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	ctx := context.Background()
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	var database string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	_, err := db.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(database)+" SET default_transaction_isolation = 'repeatable read'")
	require.NoError(t, err)
	db.SetMaxIdleConns(0) // New sessions must observe the changed database default.
	control, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var defaultIsolation string
	require.NoError(t, control.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&defaultIsolation))
	require.NoError(t, control.Rollback())
	require.Equal(t, "repeatable read", defaultIsolation,
		"control: a transaction without isolation options must inherit the database default")

	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, testkit.TID(),
		"bafyreipostisolation", time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE reason = 'mention' AND record_uri = $1 AND recipient_did = $2 AND actor_did = $3`,
		uri, recipients[0], pv2Author), "the post mention must pass the erasure gate under READ COMMITTED")
}
