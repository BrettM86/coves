//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestCommentConsumer_ResurrectionErasureFirstWaitsOnAdvisoryLockBeforeCommentRow(t *testing.T) {
	t.Parallel()
	for _, parent := range []string{"same_parent", "different_parent"} {
		t.Run(parent, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			fixture := newCommentErasureFixture(t, ctx, db)
			originalEvent, commentURI := fixture.reply()
			require.NoError(t, fixture.consumer.HandleEvent(ctx, originalEvent))
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1 AND deleted_at IS NULL`, commentURI))
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
				WHERE record_uri = $1 AND reason = 'postReply' AND recipient_did = $2`, commentURI, fixture.recipientDID))
			deleteRevision := testkit.TID()
			require.Less(t, originalEvent.Commit.Rev, deleteRevision)
			require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
				fixture.actorDID, CommentCollection, "delete", originalEvent.Commit.RKey,
				deleteRevision, "", time.Now().UnixMicro(), nil)))
			var deletedAt sql.NullTime
			require.NoError(t, db.QueryRowContext(ctx, `SELECT deleted_at FROM comments WHERE uri = $1`, commentURI).Scan(&deletedAt))
			require.True(t, deletedAt.Valid, "Y must be soft-deleted before the re-create")
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
				"author delete keeps the reply pending erasure")

			parentURI, parentCID := fixture.postURI, fixture.postCID
			if parent == "different_parent" {
				parentID := testkit.UniqueID(t)
				parentDID := "did:plc:" + parentID + "parent"
				insertBridgedUserOnPDS(t, db, parentDID, parentID+"parent.test", bridgedTestNativePDS)
				parentKey := testkit.TID()
				parentURI = "at://" + parentDID + "/" + CommentCollection + "/" + parentKey
				parentCID = "bafyresurrectionerasureparent"
				parentRecord := revCommentRecord("C replies to B's post", fixture.postURI, fixture.postCID,
					fixture.postURI, fixture.postCID)
				parentRecord["createdAt"] = fixture.createdAt
				require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
					parentDID, CommentCollection, "create", parentKey, testkit.TID(), parentCID,
					time.Now().UnixMicro(), parentRecord)))
				require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, parentURI))
			}
			recreateRevision := testkit.TID()
			require.Less(t, deleteRevision, recreateRevision)
			const recreatedCID = "bafyresurrectionerasurecreated"
			record := revCommentRecord("A re-creates Y after account erasure", fixture.postURI, fixture.postCID, parentURI, parentCID)
			record["createdAt"] = fixture.createdAt
			recreateEvent := revCommitEvent(fixture.actorDID, CommentCollection, "create",
				originalEvent.Commit.RKey, recreateRevision, recreatedCID, time.Now().UnixMicro(), record)

			results := make(chan error, 1)
			started, finished := false, false
			// Registered before the transaction: cleanup first releases its locks,
			// then this drains the blocked consumer, then cancels its context.
			t.Cleanup(func() {
				if started && !finished {
					commentErasureResult(t, ctx, results, "HandleEvent(resurrection) after fixture rollback")
				}
			})
			transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, fixture.actorDID)
			_, err := transaction.ExecContext(ctx,
				"SELECT pg_advisory_xact_lock("+postgres.ErasureLockKeySQL+")", fixture.actorDID)
			require.NoError(t, err)
			_, err = transaction.ExecContext(ctx,
				`INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, fixture.actorDID)
			require.NoError(t, err)
			removed, err := transaction.ExecContext(ctx, `DELETE FROM comments WHERE commenter_did = $1`, fixture.actorDID)
			require.NoError(t, err)
			removedRows, err := removed.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, removedRows, "erasure must delete and lock the soft-deleted Y")
			_, err = transaction.ExecContext(ctx, `DELETE FROM notifications WHERE actor_did = $1`, fixture.actorDID)
			require.NoError(t, err)

			started = true
			go func() { results <- fixture.consumer.HandleEvent(ctx, recreateEvent) }()
			consumerProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID,
				"pg_advisory_xact_lock_shared")
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
				"resurrection must wait for erasure's advisory lock without holding or waiting on Y's row")

			require.NoError(t, transaction.Commit(), "finish fixture account erasure")
			commentErasureResult(t, ctx, results, "HandleEvent(resurrection) after account erasure")
			finished = true
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, fixture.actorDID))
			require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
				"erasure hard-deleted Y, so the re-create takes the insert path; erased A must not generate notifications there")
			// Known content-indexing gap: erasure removed the old row, but the
			// re-create indexes a new live comment for erased A without notifying.
			var storedCID, storedParentURI string
			var finalDeletedAt sql.NullTime
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT cid, parent_uri, deleted_at FROM comments WHERE uri = $1`, commentURI).
				Scan(&storedCID, &storedParentURI, &finalDeletedAt))
			require.Equal(t, recreatedCID, storedCID, "the accepted indexing gap writes the new CID")
			require.Equal(t, parentURI, storedParentURI)
			require.False(t, finalDeletedAt.Valid, "the erased author's new row is live but has no notifications")
		})
	}
}

// Erasure normally hard-deletes A's comments, but the consumer still indexes an
// erased actor's events. A soft-deleted Y that survives erasure reaches the
// same-parent resurrection branch with the actor already erased.
func TestCommentConsumer_SameParentResurrectionByErasedActorWritesNoNotifications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newMentionEditFixture(t)
	postAuthor := fixture.postAuthor(t)
	mentioned := fixture.recipient(t)
	fixture.create(t)
	require.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"), "fixture: the original create must notify B")

	deleteRevision := testkit.TID()
	require.Less(t, fixture.revision, deleteRevision)
	require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
		fixture.gate.commenterDID, CommentCollection, "delete", fixture.key, deleteRevision, "",
		time.Now().UnixMicro(), nil)), "A deletes Y")
	var commentID int64
	var deletedAt sql.NullTime
	require.NoError(t, fixture.gate.db.QueryRowContext(ctx, `SELECT id, deleted_at FROM comments WHERE uri = $1`,
		fixture.uri).Scan(&commentID, &deletedAt))
	require.True(t, deletedAt.Valid, "fixture: author delete must keep Y as a soft-deleted row")
	keptRows := notificationRowsForRecordOrSubject(t, fixture.gate.db, fixture.uri)
	require.Len(t, keptRows, 1, "fixture: author delete must keep Y's reply notification")
	_, err := fixture.gate.db.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.gate.commenterDID)
	require.NoError(t, err, "fixture: mark A erased while keeping the soft-deleted comment row")

	recreateRevision := testkit.TID()
	require.Less(t, deleteRevision, recreateRevision)
	const recreatedCID = "bafyreisameparenterasedrecreated"
	require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
		fixture.gate.commenterDID, CommentCollection, "create", fixture.key, recreateRevision, recreatedCID,
		time.Now().UnixMicro(), fixture.record(t, fixture.createdAt, mentioned))),
		"erased A re-creates Y on the same parent and mentions E")
	var resurrectedID int64
	var resurrectedCID string
	var resurrectedDeletedAt sql.NullTime
	require.NoError(t, fixture.gate.db.QueryRowContext(ctx, `SELECT id, cid, deleted_at FROM comments WHERE uri = $1`,
		fixture.uri).Scan(&resurrectedID, &resurrectedCID, &resurrectedDeletedAt))
	require.Equal(t, commentID, resurrectedID, "the re-create must resurrect the kept row, not insert a new one")
	require.Equal(t, recreatedCID, resurrectedCID, "the resurrection must apply the new CID")
	require.False(t, resurrectedDeletedAt.Valid, "the resurrection must clear the delete")
	require.Equal(t, keptRows, notificationRowsForRecordOrSubject(t, fixture.gate.db, fixture.uri),
		"erased A's same-parent resurrection must keep B's row without notifying E")
}
