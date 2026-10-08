//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type failingPostEditNotificationRepository struct {
	notifications.Repository
	failure       error
	failAt        string
	applyCalled   bool
	erasureCalled bool
	intents       []notifications.Intent
}

func (repository *failingPostEditNotificationRepository) ApplyTx(ctx context.Context, tx *sql.Tx, intents []notifications.Intent) error {
	repository.applyCalled = true
	repository.intents = append(repository.intents, intents...)
	if repository.failAt == "apply" {
		return repository.failure
	}
	return repository.Repository.ApplyTx(ctx, tx, intents)
}

func (repository *failingPostEditNotificationRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, did string) (bool, error) {
	repository.erasureCalled = true
	if repository.failAt == "erasure" {
		return false, repository.failure
	}
	return repository.Repository.ErasureGateTx(ctx, tx, did)
}

func postEditStoredFacets(t *testing.T, db *sql.DB, uri string) sql.NullString {
	t.Helper()
	var facets sql.NullString
	require.NoError(t, db.QueryRow(`SELECT content_facets::text FROM posts WHERE uri = $1`, uri).Scan(&facets))
	return facets
}

func TestPostNotificationEdit_RepositoryFailuresRollBackContentFacetsAndRev(t *testing.T) {
	t.Parallel()
	for _, failurePoint := range []string{"apply", "erasure"} {
		t.Run(failurePoint, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := testkit.DB(t)
			post := newPV2Fixture(t, db)
			handles, recipients := postMentionRecipients(t, db, 2)
			createdAt := activatedCommentNotificationTime(t, db, ctx)
			key := testkit.TID()
			uri := pv2URI(pv2Author, key)
			revisions := increasingTIDs(t, 2)
			base := time.Now().UnixMicro()
			original := postMentionConsumer(db, post, postgres.NewNotificationRepository(db))
			require.NoError(t, original.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[0],
				"bafyreiposteditfailurecreate", base, postMentionRecord(t, createdAt, handles[:1], recipients[:1]))))
			before := readPostV2MechanismRow(t, db, uri)
			beforeFacets := postEditStoredFacets(t, db, uri)
			beforeRows := postMentionRows(t, db, uri)
			require.Len(t, beforeRows, 1, "fixture: B has a mention before the edit")
			require.Equal(t, revisions[0], readPostV2MechanismRev(t, db, uri))
			require.False(t, before.IndexedAt.After(time.UnixMicro(base+1_000_000)), "fixture: edit must pass the recency guard")

			injected := errors.New("injected post edit " + failurePoint + " failure")
			failing := &failingPostEditNotificationRepository{
				Repository: postgres.NewNotificationRepository(db), failure: injected, failAt: failurePoint,
			}
			consumer := postMentionConsumer(db, post, failing)
			err := consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revisions[1],
				"bafyreiposteditfailureupdate", base+1_000_000, postMentionRecord(t, createdAt, handles, recipients)))
			require.ErrorIs(t, err, injected, "failure must propagate from the winning edit")
			require.True(t, failing.erasureCalled, "the edit must reach the author erasure gate")
			require.Equal(t, failurePoint == "apply", failing.applyCalled, "only an edit past the gate reaches ApplyTx")
			if failurePoint == "apply" {
				require.Len(t, failing.intents, 1, "fixture: winning edit must offer E's mention to ApplyTx")
				require.Equal(t, recipients[1], failing.intents[0].RecipientDID)
				require.Equal(t, notifications.ReasonMention, failing.intents[0].Reason)
			}
			require.Equal(t, before, readPostV2MechanismRow(t, db, uri), "failed edit must roll back CID and content")
			require.Equal(t, beforeFacets, postEditStoredFacets(t, db, uri), "failed edit must roll back facets")
			require.Equal(t, revisions[0], readPostV2MechanismRev(t, db, uri), "failed edit must roll back the revision")
			require.Equal(t, beforeRows, postMentionRows(t, db, uri), "failed edit must preserve B's original row")
			require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[1]),
				"failed edit must not notify E")
		})
	}
}

func TestPostNotificationEdit_DeleteFirstWaitsForErasureBeforePostLock(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			fixture := newPostTombstoneFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel) // Register before the still-open fixture transaction.
			deleteResults, consumerResults := make(chan error, 1), make(chan error, 1)
			deleteStarted, deleteFinished := false, false
			consumerStarted, consumerFinished := false, false
			voteGroupResultsCleanup(t, ctx, deleteResults, &deleteStarted, &deleteFinished, "Delete(post author)")
			voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(post "+operation+")")

			transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, fixture.db, pv2Author)
			deleteStarted = true
			go func() { deleteResults <- postgres.NewUserRepository(fixture.db).Delete(ctx, pv2Author) }()
			deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")

			var event *JetstreamEvent
			if operation == "update" {
				handles, recipients := postMentionRecipients(t, fixture.db, 1)
				event = pv2Event(pv2Author, "update", fixture.key, fixture.revisions[2],
					"bafyreiposteditdeletefirst", time.Now().Add(time.Second).UnixMicro(),
					postMentionRecord(t, fixture.createdAt,
						[]string{fixture.handle, handles[0]}, []string{fixture.recipient, recipients[0]}))
			} else {
				event = pv2Event(pv2Author, "delete", fixture.key, fixture.revisions[2],
					"", time.Now().UnixMicro(), nil)
			}
			consumerStarted = true
			go func() { consumerResults <- fixture.consumer.HandleEvent(ctx, event) }()
			testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
				if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
					return false, err
				}
				var waitingBeforePost bool
				err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_locks waiter
					JOIN pg_locks holder ON holder.locktype = waiter.locktype
						AND holder.database = waiter.database AND holder.classid = waiter.classid
						AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
					WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
						AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
						AND NOT waiter.granted AND waiter.mode = 'ShareLock'
						AND $1 = ANY(pg_blocking_pids(waiter.pid))
						AND NOT EXISTS (SELECT 1 FROM pg_locks postlock
							WHERE postlock.pid = waiter.pid AND postlock.locktype = 'relation'
								AND postlock.relation = 'posts'::regclass)
						AND NOT EXISTS (SELECT 1 FROM pg_locks rowwait
							WHERE rowwait.pid = waiter.pid AND NOT rowwait.granted
								AND rowwait.locktype IN ('tuple', 'transactionid'))
				)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforePost)
				return waitingBeforePost, err
			}, testkit.WithDescription("post %s waits on Delete's erasure advisory lock without locking posts", operation))

			require.NoError(t, transaction.Commit())
			commentErasureResult(t, ctx, deleteResults, "Delete(post author)")
			deleteFinished = true
			commentErasureResult(t, ctx, consumerResults, "HandleEvent(post "+operation+")")
			consumerFinished = true
			require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, pv2Author))
			require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, pv2Author),
				"Delete-first must leave no notifications from the erased author")
		})
	}
}

func TestPostNotificationEdit_ErasedAuthorMarkerSkipsEditBeforeContentUpdate(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	ctx := context.Background()
	handles, recipients := postMentionRecipients(t, fixture.db, 1)
	before := readPostV2MechanismRow(t, fixture.db, fixture.uri)
	beforeFacets := postEditStoredFacets(t, fixture.db, fixture.uri)
	beforeRows := postMentionRows(t, fixture.db, fixture.uri)
	require.Len(t, beforeRows, 1, "fixture: B was notified before the marker")
	markAccountDeleted(t, fixture.db, pv2Author)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM users WHERE did = $1`, pv2Author),
		"fixture: author user row remains, so only the erasure marker causes the skip")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM posts WHERE uri = $1`, fixture.uri),
		"fixture: post row remains")
	require.NoError(t, fixture.consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", fixture.key,
		fixture.revisions[2], "bafyreiposteditmarkedauthor", time.Now().Add(time.Second).UnixMicro(),
		postMentionRecord(t, fixture.createdAt,
			[]string{fixture.handle, handles[0]}, []string{fixture.recipient, recipients[0]}))))
	require.Equal(t, before, readPostV2MechanismRow(t, fixture.db, fixture.uri),
		"the author-post precheck skips the entire erased-author event before the edit transaction")
	require.Equal(t, beforeFacets, postEditStoredFacets(t, fixture.db, fixture.uri))
	require.Equal(t, fixture.revisions[1], readPostV2MechanismRev(t, fixture.db, fixture.uri))
	require.Equal(t, beforeRows, postMentionRows(t, fixture.db, fixture.uri))
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, recipients[0]),
		"an erased author's skipped edit must not notify E")
}

func TestPostNotificationEdit_ErasureCommittedDuringGateSuppressesMention(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	handles, recipients := postMentionRecipients(t, fixture.db, 1)
	beforeRows := postMentionRows(t, fixture.db, fixture.uri)
	require.Len(t, beforeRows, 1, "fixture: B has a mention before E is added")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, pv2Author))
	const editCID = "bafyreiposteditracedmark"
	event := pv2Event(pv2Author, "update", fixture.key, fixture.revisions[2], editCID,
		time.Now().Add(time.Second).UnixMicro(), postMentionRecord(t, fixture.createdAt,
			[]string{fixture.handle, handles[0]}, []string{fixture.recipient, recipients[0]}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // Register before the fixture transaction, so its rollback releases the lock first.
	results := make(chan error, 1)
	started, finished := false, false
	voteGroupResultsCleanup(t, ctx, results, &started, &finished, "HandleEvent(post edit after erasure marker)")

	connection, err := fixture.db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	transaction, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := transaction.Rollback()
		require.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone),
			"rolling back erasure-marker fixture transaction: %v", rollbackError)
	})
	var fixtureProcessID int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&fixtureProcessID))
	_, err = transaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+postgres.ErasureLockKeySQL+")", pv2Author)
	require.NoError(t, err)
	_, err = transaction.ExecContext(ctx, `INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, pv2Author)
	require.NoError(t, err)
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, pv2Author),
		"the consumer precheck must not see the uncommitted erasure marker")

	started = true
	go func() { results <- fixture.consumer.HandleEvent(ctx, event) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		var waitingAtGate bool
		err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks waiter
			JOIN pg_locks holder ON holder.locktype = waiter.locktype
				AND holder.database = waiter.database AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
			JOIN pg_stat_activity activity ON activity.pid = waiter.pid
			WHERE holder.pid = $1 AND waiter.pid <> $1
				AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
				AND NOT waiter.granted AND waiter.mode = 'ShareLock'
				AND $1 = ANY(pg_blocking_pids(waiter.pid))
				AND activity.query ILIKE '%pg_advisory_xact_lock_shared%'
				AND NOT EXISTS (SELECT 1 FROM pg_locks postlock
					WHERE postlock.pid = waiter.pid AND postlock.locktype = 'relation'
						AND postlock.relation = 'posts'::regclass)
		)`, fixtureProcessID).Scan(&waitingAtGate)
		return waitingAtGate, err
	}, testkit.WithDescription("post edit passes the marker precheck and waits at the in-transaction erasure gate"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(post edit after erasure marker)")
	finished = true
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, pv2Author))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM users WHERE did = $1`, pv2Author))
	row := readPostV2MechanismRow(t, fixture.db, fixture.uri)
	require.Equal(t, editCID, row.CID, "edit must apply after the in-transaction gate sees erasure")
	require.Equal(t, "mentions @"+fixture.handle+" @"+handles[0], row.Content)
	requirePostMentionFacets(t, fixture.db, fixture.uri, fixture.recipient, recipients[0])
	require.Equal(t, fixture.revisions[2], readPostV2MechanismRev(t, fixture.db, fixture.uri))
	require.Equal(t, beforeRows, postMentionRows(t, fixture.db, fixture.uri), "erased-author edit must not add or retract mentions")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, recipients[0]),
		"the in-transaction erasure gate must suppress E's mention")
}
