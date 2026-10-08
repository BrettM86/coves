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

func TestVoteConsumer_UpvoteGroupDeleteThenCreateFlipStaysGone(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	ctx := context.Background()
	firstKey := testkit.TID()
	firstURI := "at://" + voter + "/social.coves.feed.vote/" + firstKey
	first := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, firstKey)
	require.NoError(t, consumer.HandleEvent(ctx, first))
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
		"A's first qualifying upvote must create B's group")

	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		voter, "social.coves.feed.vote", "delete", firstKey, revB, "", first.TimeUS+1_000_000, nil)))
	exists, active := voteRowState(t, fixture.db, firstURI)
	require.True(t, exists && !active, "A's original vote must be soft-deleted")
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"deleting the last qualifying upvote must remove B's group")

	secondKey := testkit.TID()
	require.NotEqual(t, firstKey, secondKey, "A's later upvote must use a new rkey")
	secondURI := "at://" + voter + "/social.coves.feed.vote/" + secondKey
	second := maintenanceVoteAtKey(voter, fixture.post, time.Now().UTC().Format(time.RFC3339Nano), revC, secondKey)
	require.NoError(t, consumer.HandleEvent(ctx, second))
	exists, active = voteRowState(t, fixture.db, secondURI)
	require.True(t, exists && active, "A's new-rkey upvote must be indexed and live")
	require.Zero(t, groupCount(t, fixture.db, fixture.author, fixture.post),
		"A's later upvote on the same subject must not recreate the deleted group")
}

func TestVoteConsumer_UpvoteGroupDeleteVoteDeleteFirstWaitsOnErasureLock(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	consumer := fixture.consumer()
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	created := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
	deleteEvent := revCommitEvent(voter, "social.coves.feed.vote", "delete", key, revB, "", created.TimeUS+1_000_000, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // registered before the fixture transaction, which may still be open on failure
	deleteResults, consumerResults := make(chan error, 1), make(chan error, 1)
	deleteStarted, deleteFinished := false, false
	consumerStarted, consumerFinished := false, false
	voteGroupResultsCleanup(t, ctx, deleteResults, &deleteStarted, &deleteFinished, "Delete(A)")
	voteGroupResultsCleanup(t, ctx, consumerResults, &consumerStarted, &consumerFinished, "HandleEvent(A's vote delete)")
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, fixture.db, voter)
	deleteStarted = true
	go func() { deleteResults <- postgres.NewUserRepository(fixture.db).Delete(ctx, voter) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")

	consumerStarted = true
	go func() { consumerResults <- consumer.HandleEvent(ctx, deleteEvent) }()
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
				AND NOT EXISTS (SELECT 1 FROM pg_locks other WHERE other.pid = waiter.pid
					AND NOT other.granted AND other.locktype IN ('tuple', 'transactionid'))
				AND NOT EXISTS (SELECT 1 FROM pg_locks content WHERE content.pid = waiter.pid
					AND content.locktype = 'relation' AND content.relation IN
					('votes'::regclass, 'posts'::regclass, 'comments'::regclass))
		)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("vote delete consumer waiting for A's erasure advisory lock before any vote/post/comment row lock"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	deleteFinished = true
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(A's vote delete)")
	consumerFinished = true
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM deleted_accounts WHERE did = $1`, voter))
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM votes WHERE uri = $1`, uri),
		"erasure must hard-delete A's original vote")
}

func TestVoteConsumer_UpvoteGroupDeleteVoteRequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	voter := fixture.voter(t)
	consumer := fixture.consumer()
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	created := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))

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
		"control: a transaction without options must inherit the overridden default")

	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		voter, "social.coves.feed.vote", "delete", key, revB, "", created.TimeUS+1_000_000, nil)))
	exists, active := voteRowState(t, fixture.db, uri)
	require.True(t, exists && !active, "the delete must soft-delete A's live upvote")
}

func TestVoteConsumer_UpvoteGroupDeleteVoteMaintainsSubjectGroup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{
		"last_post_upvote", "last_comment_upvote", "eight_day_old_upvote",
		"pre_activation_upvote", "blocked_upvoter", "missing_post_row",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			consumer := fixture.consumer()
			voter := fixture.voter(t)
			recipient, subject := fixture.author, fixture.post
			if scenario == "last_comment_upvote" {
				id := testkit.UniqueID(t)
				recipient = "did:plc:" + id + "commenter"
				insertBridgedUserOnPDS(t, fixture.db, recipient, id+"commenter.test", bridgedTestNativePDS)
				commentKey := testkit.TID()
				subject = "at://" + recipient + "/" + CommentCollection + "/" + commentKey
				_, err := fixture.db.Exec(`INSERT INTO comments
					(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
					VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'bafupvoteroot', $4, 'bafupvoteroot', 'comment', NOW())`,
					subject, commentKey, recipient, fixture.post)
				require.NoError(t, err)
			}
			key := testkit.TID()
			uri := "at://" + voter + "/social.coves.feed.vote/" + key
			created := maintenanceVoteAtKey(voter, subject, fixture.createdAt, revA, key)
			require.NoError(t, consumer.HandleEvent(context.Background(), created))
			require.Equal(t, 1, groupCount(t, fixture.db, recipient, subject),
				"fixture: the first qualifying upvote must create the subject author's group")

			switch scenario {
			case "eight_day_old_upvote":
				_, err := fixture.db.Exec(`UPDATE votes SET created_at = NOW() - INTERVAL '8 days' WHERE uri = $1`, uri)
				require.NoError(t, err)
			case "pre_activation_upvote":
				_, err := fixture.db.Exec(`UPDATE votes SET created_at =
					(SELECT activated_at - INTERVAL '1 second' FROM notification_activation) WHERE uri = $1`, uri)
				require.NoError(t, err)
			case "blocked_upvoter":
				ineligibleUpvoteTime(t, fixture, "recipient_blocks_voter", voter)
			case "missing_post_row":
				result, err := fixture.db.Exec(`DELETE FROM posts WHERE uri = $1`, subject)
				require.NoError(t, err)
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows, "fixture: the subject row must be gone")
				require.Equal(t, 1, groupCount(t, fixture.db, recipient, subject),
					"fixture: deleting the post must leave its upvote group for maintenance")
			}

			require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
				voter, "social.coves.feed.vote", "delete", key, revB, "", created.TimeUS+1_000_000, nil)))
			exists, active := voteRowState(t, fixture.db, uri)
			require.True(t, exists && !active, "the vote must be soft-deleted before checking group maintenance")
			require.Zero(t, groupCount(t, fixture.db, recipient, subject),
				"deleting the last live qualifying upvote must remove the %s group", scenario)
		})
	}
}

func TestVoteConsumer_UpvoteGroupDeleteVotePreservesCompanionAndSort(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	consumer := fixture.consumer()
	voter := fixture.voter(t)
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	created := maintenanceVoteAtKey(voter, fixture.post, fixture.createdAt, revA, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	otherURI := deliverGroupVote(t, consumer, fixture.voter(t), fixture.post, "up", fixture.createdAt)
	groupID, _ := maintenanceGroup(t, fixture)
	past := ageMaintenanceGroup(t, fixture.db, groupID)

	require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
		voter, "social.coves.feed.vote", "delete", key, revB, "", created.TimeUS+1_000_000, nil)))
	exists, active := voteRowState(t, fixture.db, uri)
	require.True(t, exists && !active, "A's vote must be soft-deleted")
	exists, active = voteRowState(t, fixture.db, otherURI)
	require.True(t, exists && active, "D's qualifying upvote must remain live")
	var storedID int64
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT id, sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		fixture.author, fixture.post).Scan(&storedID, &storedSort), "D's vote must keep the group")
	require.Equal(t, groupID, storedID, "the surviving group must retain its id")
	require.Truef(t, storedSort.Equal(past), "deleting A's vote moved sort_at from %s to %s", past, storedSort)
	require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
}
