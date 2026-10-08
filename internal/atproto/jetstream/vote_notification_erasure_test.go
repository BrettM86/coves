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

func voteGroupResultsCleanup(t *testing.T, ctx context.Context, results <-chan error, started, finished *bool, operation string) {
	t.Helper()
	t.Cleanup(func() {
		if *started && !*finished {
			commentErasureResult(t, ctx, results, operation+" after fixture rollback")
		}
	})
}

func TestVoteConsumer_UpvoteGroupRootUsesLockedComment(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	secondRoot := fixture.addPost(t)
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.author + "/" + CommentCollection + "/" + commentKey
	_, err := fixture.db.Exec(`INSERT INTO comments
		(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'bafupvoteroot', $4, 'bafupvoteroot', 'comment', NOW())`,
		commentURI, commentKey, fixture.author, fixture.post)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // registered before the open fixture transaction
	event := upvoteGroupEvent(fixture.voter(t), commentURI, "up", fixture.createdAt, testkit.TID())
	consumerResults := make(chan error, 1)
	consumerStarted, consumerFinished := false, false
	voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(comment vote)")
	transaction, fixtureProcessID := mentionEditRowTransaction(t, ctx, fixture.db)
	var initialRoot string
	require.NoError(t, transaction.QueryRowContext(ctx,
		`SELECT root_uri FROM comments WHERE uri = $1 FOR UPDATE`, commentURI).Scan(&initialRoot))
	require.Equal(t, fixture.post, initialRoot, "fixture: the unlocked root is P1")
	consumerStarted = true
	go func() { consumerResults <- fixture.consumer().HandleEvent(ctx, event) }()
	commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "comments")
	_, err = transaction.ExecContext(ctx, `UPDATE comments SET root_uri = $1 WHERE uri = $2`, secondRoot, commentURI)
	require.NoError(t, err)
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(comment vote)")
	consumerFinished = true
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND root_post_uri = $3`,
		fixture.author, commentURI, secondRoot),
		"the comment upvote group must navigate to P2, committed while the consumer waited on comments")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND root_post_uri = $3`,
		fixture.author, commentURI, fixture.post), "the stale unlocked root P1 must not be used")
}

func TestVoteConsumer_UpvoteGroupDeleteFirstWaitsBeforeContent(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // registered before the open fixture transaction
	deleteResults, consumerResults := make(chan error, 1), make(chan error, 1)
	deleteStarted, deleteFinished := false, false
	consumerStarted, consumerFinished := false, false
	voteGroupResultsCleanup(t, ctx, deleteResults, &deleteStarted, &deleteFinished, "Delete(A)")
	voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(A's vote)")
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, fixture.db, voter)
	deleteStarted = true
	go func() { deleteResults <- postgres.NewUserRepository(fixture.db).Delete(ctx, voter) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")
	consumerStarted = true
	event := upvoteGroupEvent(voter, fixture.post, "up", fixture.createdAt, testkit.TID())
	go func() { consumerResults <- fixture.consumer().HandleEvent(ctx, event) }()
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
				AND NOT EXISTS (SELECT 1 FROM pg_locks content WHERE content.pid = waiter.pid
					AND content.locktype = 'relation'
					AND content.relation IN ('votes'::regclass, 'posts'::regclass, 'comments'::regclass))
		)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("vote consumer never blocked on Delete(A)'s advisory lock before votes/posts/comments"))
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	deleteFinished = true
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(A's vote)")
	consumerFinished = true
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"Delete-first voter erasure must leave no group for B's post")
}

func TestVoteConsumer_UpvoteGroupEligibleVoterControl(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	deliverGroupVote(t, fixture.consumer(), fixture.voter(t), fixture.post, "up", fixture.createdAt)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
		"the same eligible voter without Delete must create the group")
}

// Delete(B) removes the recipient's post before the recipient's notification.
// While the consumer holds the post row, Delete must therefore wait on that
// backend rather than reach the group row first and create a lock-order cycle.
func voteGroupDeleteWaitsForConsumer(t *testing.T, ctx context.Context, transaction *sql.Tx, consumerProcessID, fixtureProcessID int) {
	t.Helper()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		var deleteBlockedOnPost bool
		err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND pid NOT IN ($1, $2)
				AND wait_event_type = 'Lock' AND query ILIKE '%DELETE FROM posts%'
				AND $1 = ANY(pg_blocking_pids(pid))
		)`, consumerProcessID, fixtureProcessID).Scan(&deleteBlockedOnPost)
		return deleteBlockedOnPost, err
	}, testkit.WithDescription("Delete(B) blocked behind the vote consumer while deleting B's posts"))
}

func TestVoteConsumer_UpvoteGroupRecipientDeleteWaitsForConsumer(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"fresh_insert", "existing_group_bump"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			if kind == "existing_group_bump" {
				deliverGroupVote(t, fixture.consumer(), fixture.voter(t), fixture.post, "up", fixture.createdAt)
				require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
					"fixture: the first voter must create the group before the bump race")
			} else {
				require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel) // registered before the open fixture transaction
			consumerResults, deleteResults := make(chan error, 1), make(chan error, 1)
			consumerStarted, consumerFinished := false, false
			deleteStarted, deleteFinished := false, false
			voteGroupResultsCleanup(t, ctx, deleteResults, &deleteStarted, &deleteFinished, "Delete(B)")
			voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(B's post vote)")
			transaction, fixtureProcessID := mentionEditRowTransaction(t, ctx, fixture.db)
			if kind == "fresh_insert" {
				var lockedDID string
				require.NoError(t, transaction.QueryRowContext(ctx,
					`SELECT did FROM users WHERE did = $1 FOR UPDATE`, fixture.author).Scan(&lockedDID))
				require.Equal(t, fixture.author, lockedDID)
			} else {
				var lockedID int64
				require.NoError(t, transaction.QueryRowContext(ctx, `SELECT id FROM notifications
					WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 FOR UPDATE`,
					fixture.author, fixture.post).Scan(&lockedID))
			}
			event := upvoteGroupEvent(fixture.voter(t), fixture.post, "up", fixture.createdAt, testkit.TID())
			consumerStarted = true
			go func() { consumerResults <- fixture.consumer().HandleEvent(ctx, event) }()
			consumerProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "INSERT INTO notifications")
			deleteStarted = true
			go func() { deleteResults <- postgres.NewUserRepository(fixture.db).Delete(ctx, fixture.author) }()
			voteGroupDeleteWaitsForConsumer(t, ctx, transaction, consumerProcessID, fixtureProcessID)
			require.NoError(t, transaction.Commit())
			commentErasureResult(t, ctx, consumerResults, "HandleEvent(B's post vote)")
			consumerFinished = true
			commentErasureResult(t, ctx, deleteResults, "Delete(B)")
			deleteFinished = true
			require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE recipient_did = $1`, fixture.author),
				"recipient erasure must remove the upvote group")
			require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM users WHERE did = $1`, fixture.author),
				"Delete(B) must remove the recipient")
		})
	}
}

// The erasure gate refuses any transaction that is not READ COMMITTED, so the
// vote consumer must request that isolation explicitly rather than inherit the
// session default. The clone's default is moved to REPEATABLE READ; only an
// explicit BeginTx option lets the qualifying upvote through the gate.
func TestVoteConsumer_UpvoteGroupGateRequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	ctx := context.Background()
	var database string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	_, err := fixture.db.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(database)+" SET default_transaction_isolation = 'repeatable read'")
	require.NoError(t, err)
	// The new default reaches only new sessions. No pooled connection is idle
	// after this, so every later transaction opens a fresh session.
	fixture.db.SetMaxIdleConns(0)
	control, err := fixture.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var defaultIsolation string
	require.NoError(t, control.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&defaultIsolation))
	require.NoError(t, control.Rollback())
	require.Equal(t, "repeatable read", defaultIsolation,
		"control: a transaction begun without options must inherit the overridden default")
	deliverGroupVote(t, fixture.consumer(), voter, fixture.post, "up", fixture.createdAt)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
		"the qualifying upvote must pass the erasure gate and create the group")
}

// A vote's createdAt feeds the notification freshness window, and a far-future
// value would stay inside that window until wall-clock caught up. The vote is
// stored clamped to index time, the same clamp the post and comment consumers
// apply, so the value fan-out reads is never later than the index.
func TestVoteConsumer_FutureCreatedAtIsClampedToIndexTime(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	futureCreatedAt := time.Now().AddDate(10, 0, 0).UTC().Format(time.RFC3339)
	event := upvoteGroupEvent(voter, fixture.post, "up", futureCreatedAt, testkit.TID())
	require.NoError(t, fixture.consumer().HandleEvent(context.Background(), event))
	handledAt := time.Now()
	var storedCreatedAt time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT created_at FROM votes WHERE uri = $1`,
		"at://"+voter+"/social.coves.feed.vote/"+event.Commit.RKey).Scan(&storedCreatedAt))
	require.False(t, storedCreatedAt.After(handledAt),
		"a future vote createdAt must be clamped to index time: stored %s, HandleEvent returned at %s (record said %s)",
		storedCreatedAt.UTC().Format(time.RFC3339Nano), handledAt.UTC().Format(time.RFC3339Nano), futureCreatedAt)
}
