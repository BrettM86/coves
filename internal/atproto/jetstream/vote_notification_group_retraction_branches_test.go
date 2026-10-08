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

func blockedVoteGroup(t *testing.T) (upvoteGroupFixture, *VoteEventConsumer, string, string, *JetstreamEvent, int64, time.Time) {
	t.Helper()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	created := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revB, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	groupID, sortAt := maintenanceGroup(t, fixture)
	ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM user_blocks
		WHERE blocker_did = $1 AND blocked_did = $2`, fixture.author, voter))
	exists, active := voteRowState(t, fixture.db, uri)
	require.True(t, exists && active, "fixture: A's blocked upvote must still be live")
	return fixture, consumer, voter, uri, created, groupID, sortAt
}

func requireVoteGroupUnchanged(t *testing.T, fixture upvoteGroupFixture, groupID int64, sortAt time.Time) {
	t.Helper()
	storedID, storedSort := maintenanceGroup(t, fixture)
	require.Equal(t, groupID, storedID, "the original group row must survive")
	require.Truef(t, storedSort.Equal(sortAt), "group sort_at changed from %s to %s", sortAt, storedSort)
}

func TestVoteConsumer_UpvoteGroupDeleteVoteFailureRollsBack(t *testing.T) {
	t.Parallel()
	fixture, workingConsumer, voter, uri, created, groupID, sortAt := blockedVoteGroup(t)
	countQuery := `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`
	before := readSubjectCounts(t, fixture.db, countQuery, fixture.post)
	require.Equal(t, 1, before.Upvotes)
	injected := errors.New("injected vote deletion group write failure")
	repository := &failingUpvoteGroupRepository{Repository: postgres.NewNotificationRepository(fixture.db), failure: injected}
	deleted := revCommitEvent(voter, "social.coves.feed.vote", "delete", created.Commit.RKey, revC, "", created.TimeUS+1_000_000, nil)
	err := fixture.consumer(WithVoteNotifications(repository)).HandleEvent(context.Background(), deleted)
	require.ErrorIs(t, err, injected)
	require.Equal(t, []notifications.UpvoteGroupIntent{{Action: notifications.UpvoteGroupDeleteIfEmpty,
		RecipientDID: fixture.author, SubjectURI: fixture.post}}, repository.intents)
	exists, active := voteRowState(t, fixture.db, uri)
	assert.True(t, exists && active, "the failed delete must leave the vote live")
	assert.Equal(t, before, readSubjectCounts(t, fixture.db, countQuery, fixture.post), "count decrement must roll back")
	assert.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM jetstream_record_revs
		WHERE record_uri = $1 AND rev = $2`, uri, revB), "delete rev claim must roll back to the create rev")
	requireVoteGroupUnchanged(t, fixture, groupID, sortAt)
	require.NoError(t, workingConsumer.HandleEvent(context.Background(), deleted), "identical redrive must succeed")
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post), "positive control: the blocked vote leaves no qualifying upvotes")
}

func TestVoteConsumer_UpvoteGroupDeleteVoteNoOpBranchesLeaveGroup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"stale_rev", "already_deleted", "not_found"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			fixture, consumer, voter, uri, created, groupID, sortAt := blockedVoteGroup(t)
			key, rev := created.Commit.RKey, revC
			switch scenario {
			case "stale_rev":
				rev = revA
			case "already_deleted":
				result, err := fixture.db.Exec(`UPDATE votes SET deleted_at = NOW() WHERE uri = $1`, uri)
				require.NoError(t, err)
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows)
			case "not_found":
				key = testkit.TID()
				require.NotEqual(t, created.Commit.RKey, key)
				require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes
					WHERE voter_did = $1 AND rkey = $2`, voter, key))
			}
			require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
				voter, "social.coves.feed.vote", "delete", key, rev, "", created.TimeUS+1_000_000, nil)))
			requireVoteGroupUnchanged(t, fixture, groupID, sortAt)
			exists, active := voteRowState(t, fixture.db, uri)
			require.True(t, exists)
			if scenario == "already_deleted" {
				require.False(t, active)
			} else {
				require.True(t, active, "the no-op must leave A's vote live")
			}
			require.Equal(t, 1, readSubjectCounts(t, fixture.db,
				`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes)
		})
	}
}

func TestVoteConsumer_UpvoteGroupDeleteVoteZeroRowSoftDeleteLeavesGroup(t *testing.T) {
	t.Parallel()
	fixture, consumer, voter, uri, created, groupID, sortAt := blockedVoteGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, 1)
	started, finished := false, false
	voteGroupResultsCleanup(t, ctx, results, &started, &finished, "HandleEvent(A's zero-row vote delete)")
	transaction, fixtureProcessID := mentionEditRowTransaction(t, ctx, fixture.db)
	result, err := transaction.ExecContext(ctx, `UPDATE votes SET deleted_at = NOW() WHERE uri = $1`, uri)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "fixture must hold A's vote row")
	started = true
	go func() {
		results <- consumer.HandleEvent(ctx, revCommitEvent(voter, "social.coves.feed.vote", "delete",
			created.Commit.RKey, revC, "", created.TimeUS+1_000_000, nil))
	}()
	commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "UPDATE votes")
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(A's zero-row vote delete)")
	finished = true
	requireVoteGroupUnchanged(t, fixture, groupID, sortAt)
	exists, active := voteRowState(t, fixture.db, uri)
	require.True(t, exists && !active)
	require.Equal(t, 1, readSubjectCounts(t, fixture.db,
		`SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post).Upvotes,
		"the consumer must not decrement a vote already soft-deleted by the fixture")
}

func TestVoteConsumer_UpvoteGroupDeleteVoteWaitsOnSubjectRowBeforeMaintenance(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	created := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	groupID, sortAt := maintenanceGroup(t, fixture)
	companion := fixture.voter(t)
	companionKey := testkit.TID()
	companionURI := "at://" + companion + "/social.coves.feed.vote/" + companionKey

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, 1)
	started, finished := false, false
	voteGroupResultsCleanup(t, ctx, results, &started, &finished, "HandleEvent(A's vote delete)")
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
	require.EqualValues(t, 1, countedRows, "D's count update must hold the posts row")
	require.NoError(t, postgres.NewNotificationRepository(fixture.db).ApplyUpvoteGroupTx(ctx, transaction,
		notifications.UpvoteGroupIntent{Action: notifications.UpvoteGroupBump,
			RecipientDID: fixture.author, SubjectURI: fixture.post, RootPostURI: fixture.post}),
		"D's group bump must hold the group row")
	started = true
	go func() {
		results <- consumer.HandleEvent(ctx, revCommitEvent(voter, "social.coves.feed.vote", "delete",
			key, revB, "", created.TimeUS+1_000_000, nil))
	}()
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
	}, testkit.WithDescription("A's vote delete blocked by D's uncommitted post vote"))
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(A's vote delete)")
	finished = true
	var storedID int64
	var storedSort time.Time
	groupErr := fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		fixture.author, fixture.post).Scan(&storedID, &storedSort)
	assert.NoError(t, groupErr, "group deleted although D's committed qualifying upvote is live")
	assert.True(t, waitingOnPostsRow, "A's delete must wait on the posts row")
	assert.False(t, waitingOnGroupRow, "A's delete must not reach the group row while D holds the post")
	assert.Contains(t, waitingQuery, "UPDATE posts", "A's count decrement must block before group maintenance")
	assert.Contains(t, waitingQuery, "upvote_count = GREATEST(0, upvote_count - 1)")
	require.NoError(t, groupErr)
	assert.Equal(t, groupID, storedID)
	assert.False(t, storedSort.Before(sortAt), "the group sort_at must not move backwards")
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	exists, active := voteRowState(t, fixture.db, uri)
	assert.True(t, exists && !active, "A's upvote must be soft-deleted")
	exists, active = voteRowState(t, fixture.db, companionURI)
	assert.True(t, exists && active, "D's upvote must be live")
	counts := readSubjectCounts(t, fixture.db, `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`, fixture.post)
	assert.Equal(t, 1, counts.Upvotes, "D's increment and A's decrement must both apply")
	assert.Zero(t, counts.Downvotes)
	assert.Equal(t, 1, counts.Score)
}
