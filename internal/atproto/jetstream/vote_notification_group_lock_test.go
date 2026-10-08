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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// At READ COMMITTED, a delete-if-empty that waits on a group row being bumped
// re-checks only that row after the bump commits, so it would delete a group
// whose new qualifying upvote is live. It is correct only because the consumer
// first takes the subject's posts row, which a concurrent vote's count update
// also holds. Here D's uncommitted vote holds P's row and the group row; A's
// replacement downvote must wait on P's row and then see D's upvote.
func TestVoteConsumer_UpvoteGroupDeleteIfEmptyWaitsOnSubjectRowLock(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	oldURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
	groupID, sortAt := maintenanceGroup(t, fixture)
	companion := fixture.voter(t)
	companionKey := testkit.TID()
	companionURI := "at://" + companion + "/social.coves.feed.vote/" + companionKey
	replacement := upvoteGroupEvent(voter, fixture.post, "down", fixture.createdAt, testkit.TID())
	replacementURI := "at://" + voter + "/social.coves.feed.vote/" + replacement.Commit.RKey
	require.NotEqual(t, oldURI, replacementURI, "the replacement must have a new record key")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // registered before the fixture transaction, which may still be open on failure
	results := make(chan error, 1)
	started, finished := false, false
	voteGroupResultsCleanup(t, ctx, results, &started, &finished, "HandleEvent(A's replacement vote)")
	transaction, fixtureProcessID := mentionEditRowTransaction(t, ctx, fixture.db)
	_, err := transaction.ExecContext(ctx, `INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafupvotegroupvote', $2, $3, $4, 'bafupvotesubject', 'up', $5, NOW())`,
		companionURI, companionKey, companion, fixture.post, fixture.createdAt)
	require.NoError(t, err)
	counted, err := transaction.ExecContext(ctx, `UPDATE posts
		SET upvote_count = upvote_count + 1,
		    score = upvote_count + 1 - downvote_count + bridged_upvote_count - bridged_downvote_count
		WHERE uri = $1`, fixture.post)
	require.NoError(t, err)
	countedRows, err := counted.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, countedRows, "D's count update must hold P's posts row")
	require.NoError(t, postgres.NewNotificationRepository(fixture.db).ApplyUpvoteGroupTx(ctx, transaction,
		notifications.UpvoteGroupIntent{
			Action:       notifications.UpvoteGroupBump,
			RecipientDID: fixture.author, SubjectURI: fixture.post, RootPostURI: fixture.post,
		}),
		"D's group bump must hold the group row")

	started = true
	go func() { results <- consumer.HandleEvent(ctx, replacement) }()
	// Wait for any block on the fixture, then record which row it is, so a
	// consumer that reaches the group row first still runs to the lost group.
	var waitingQuery string
	var waitingOnPostsRow, waitingOnGroupRow bool
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return false, err
		}
		err := transaction.QueryRowContext(ctx, `SELECT waiter.query,
			EXISTS (SELECT 1 FROM pg_locks row_lock WHERE row_lock.pid = waiter.pid
				AND row_lock.locktype = 'tuple' AND row_lock.relation = 'posts'::regclass),
			EXISTS (SELECT 1 FROM pg_locks row_lock WHERE row_lock.pid = waiter.pid
				AND row_lock.locktype = 'tuple' AND row_lock.relation = 'notifications'::regclass)
			FROM pg_stat_activity waiter
			WHERE waiter.datname = current_database() AND waiter.pid <> $1
				AND waiter.wait_event_type = 'Lock'
				AND $1 = ANY(pg_blocking_pids(waiter.pid))
			LIMIT 1`, fixtureProcessID).Scan(&waitingQuery, &waitingOnPostsRow, &waitingOnGroupRow)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("A's replacement vote blocked by D's uncommitted vote transaction"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(A's replacement vote)")
	finished = true
	var storedID int64
	var storedSort time.Time
	groupErr := fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		fixture.author, fixture.post).Scan(&storedID, &storedSort)
	assert.NoError(t, groupErr, "group deleted although D's committed qualifying upvote is live")
	assert.True(t, waitingOnPostsRow, "A's replacement must wait on P's posts row")
	assert.False(t, waitingOnGroupRow, "A's replacement must not reach the group row while D holds P's row")
	assert.Contains(t, waitingQuery, "UPDATE posts SET upvote_count = GREATEST(0, upvote_count - 1)",
		"the stale-vote decrement is the replacement's first lock on P's row")
	require.NoError(t, groupErr)
	assert.Equal(t, groupID, storedID, "the surviving group must be the original row")
	assert.False(t, storedSort.Before(sortAt), "the group's sort_at must not move backwards")
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))

	exists, active := voteRowState(t, fixture.db, oldURI)
	assert.True(t, exists && !active, "A's old upvote must be soft-deleted")
	exists, active = voteRowState(t, fixture.db, replacementURI)
	assert.True(t, exists && active, "A's replacement downvote must be live")
	exists, active = voteRowState(t, fixture.db, companionURI)
	assert.True(t, exists && active, "D's upvote must be live")
	counts := readSubjectCounts(t, fixture.db, `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post)
	assert.Equal(t, 1, counts.Upvotes, "D's increment and A's stale decrement must both apply")
	assert.Equal(t, 1, counts.Downvotes, "A's replacement downvote must be counted")
}
