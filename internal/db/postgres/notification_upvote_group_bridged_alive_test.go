//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func bridgedGroupSubject(t *testing.T, fixture *unreadVisibilityFixture, kind string) string {
	t.Helper()
	if kind == "comment" {
		return seedActorComment(t, fixture.db, fixture.recipient, fixture.root, testkit.TID(), fixture.sortAt)
	}
	return fixture.post(t, posts.AdmissionStatusAccepted, false)
}

func setBridgedGroupTotals(t *testing.T, db *sql.DB, kind, subject string, upvotes, downvotes int) {
	t.Helper()
	table := "posts"
	if kind == "comment" {
		table = "comments"
	}
	result, err := db.Exec(`UPDATE `+table+` SET bridged_upvote_count = $2,
		bridged_downvote_count = $3, bridged_stats_as_of = NOW() WHERE uri = $1`, subject, upvotes, downvotes)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "the subject must exist before maintenance")
}

func bridgedGroupDisqualifiedVotes(t *testing.T, db *sql.DB, recipient, subject string, at time.Time) {
	t.Helper()
	votes := qualifyingUpvoteFixture{db: db}
	voter := func() string { return "did:plc:" + testkit.UniqueID(t) }
	votes.insertVote(t, recipient, subject, "up", at, false)
	erased := voter()
	_, err := db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, erased)
	require.NoError(t, err)
	votes.insertVote(t, erased, subject, "up", at, false)
	aggregator := voter()
	_, err = db.Exec(`INSERT INTO aggregators (did, display_name, record_uri, record_cid)
		VALUES ($1, 'Aggregator voter', $2, 'bafybridgedaggregator')`, aggregator,
		"at://"+aggregator+"/social.coves.aggregator.service/self")
	require.NoError(t, err)
	votes.insertVote(t, aggregator, subject, "up", at, false)
	blocked := voter()
	votes.insertBlock(t, recipient, blocked)
	votes.insertVote(t, blocked, subject, "up", at, false)
	blocking := voter()
	votes.insertBlock(t, blocking, recipient)
	votes.insertVote(t, blocking, subject, "up", at, false)
	votes.insertVote(t, voter(), subject, "up", at, true)
	votes.insertVote(t, voter(), subject, "down", at, false)
	require.False(t, votes.qualifies(t, subject, recipient), "none of the seven votes qualifies")
}

func bridgedRetentionSweeper(t *testing.T, db *sql.DB) notifications.RetentionSweeper {
	t.Helper()
	sweeper, ok := NewNotificationRepository(db, WithBridgedUpvoteTotals()).(notifications.RetentionSweeper)
	require.True(t, ok)
	return sweeper
}

func TestNotificationRepository_BridgedGroupMaintenanceAlive(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"post", "comment"} {
		for _, operation := range []string{"delete_if_empty", "sweep"} {
			for _, state := range []string{"bridged_upvotes", "downvotes_and_disqualified_votes"} {
				t.Run(kind+"/"+operation+"/"+state, func(t *testing.T) {
					t.Parallel()
					fixture := newUnreadVisibilityFixture(t)
					subject := bridgedGroupSubject(t, fixture, kind)
					at := time.Date(2026, time.September, 30, 12, 0, 7, 123456000, time.UTC).Truncate(time.Microsecond)
					group := insertDeleteTestGroup(t, upvoteGroupFixture{ctx: context.Background(), db: fixture.db, rootPostURI: fixture.root}, fixture.recipient, subject, at)
					if state == "bridged_upvotes" {
						setBridgedGroupTotals(t, fixture.db, kind, subject, 3, 0)
					} else {
						setBridgedGroupTotals(t, fixture.db, kind, subject, 0, 4)
						bridgedGroupDisqualifiedVotes(t, fixture.db, fixture.recipient, subject, at)
					}
					repository := NewNotificationRepository(fixture.db, WithBridgedUpvoteTotals())
					if operation == "delete_if_empty" {
						transaction, err := fixture.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
						require.NoError(t, err)
						defer transaction.Rollback()
						require.NoError(t, repository.ApplyUpvoteGroupTx(context.Background(), transaction, deleteIfEmptyIntent(fixture.recipient, subject)))
						require.NoError(t, transaction.Commit())
					} else {
						deleted, err := bridgedRetentionSweeper(t, fixture.db).SweepEmptyUpvoteGroups(context.Background())
						require.NoError(t, err)
						if state == "bridged_upvotes" {
							require.EqualValues(t, 0, deleted)
						} else {
							require.EqualValues(t, 1, deleted)
						}
					}
					var count int
					var storedSort sql.NullTime
					require.NoError(t, fixture.db.QueryRow(`SELECT count(*), min(sort_at) FROM notifications WHERE id = $1`, group).Scan(&count, &storedSort))
					if state == "bridged_upvotes" {
						require.Equal(t, 1, count, "bridged upvotes alone keep the group")
						require.True(t, storedSort.Valid && storedSort.Time.Equal(at), "maintenance must preserve sort_at")
					} else {
						require.Equal(t, 0, count, "downvotes and disqualified votes cannot keep the group")
					}
				})
			}
		}
	}
}

func TestNotificationRetention_BridgedTotalRecheckedAfterCandidateLocks(t *testing.T) {
	t.Parallel()
	fixture := newUnreadVisibilityFixture(t)
	label := testkit.TID()
	subject := seedVisibilityPost(t, fixture.db, fixture.community, fixture.recipient, label, "recheck", fixture.sortAt)
	seedVisibilityAdmission(t, fixture.db, fixture.community, subject, posts.AdmissionStatusAccepted, "", "")
	at := time.Date(2026, time.September, 30, 12, 0, 7, 123456000, time.UTC).Truncate(time.Microsecond)
	ids := make(map[string]int64)
	retentionRow(t, fixture.db, ids, label, fixture.recipient, "upvote", at)
	retentionRow(t, fixture.db, ids, "still_empty", fixture.recipient, "upvote", at)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	holder, holderPID := notificationRaceTransaction(t, fixture.db, ctx)
	_, err := holder.ExecContext(ctx, `LOCK TABLE notifications IN SHARE MODE`)
	require.NoError(t, err)
	type result struct {
		deleted int64
		err     error
	}
	results := make(chan result, 1)
	sweeper := bridgedRetentionSweeper(t, fixture.db)
	go func() {
		deleted, err := sweeper.SweepEmptyUpvoteGroups(ctx)
		results <- result{deleted, err}
	}()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case got := <-results:
			return false, fmt.Errorf("sweep returned (%d, %v) before its DELETE waited", got.deleted, got.err)
		default:
		}
		var waiterPID int
		err := fixture.db.QueryRowContext(ctx, `SELECT pid FROM pg_locks
			WHERE relation = 'notifications'::regclass AND locktype = 'relation'
				AND mode = 'RowExclusiveLock' AND NOT granted
				AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, holderPID).Scan(&waiterPID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("bridged sweep DELETE waiting after locking candidates"))
	_, err = holder.ExecContext(ctx, `SAVEPOINT bridged_probe`)
	require.NoError(t, err)
	var lockedID int64
	err = holder.QueryRowContext(ctx, `SELECT id FROM notifications WHERE id = $1 FOR UPDATE NOWAIT`, ids[label]).Scan(&lockedID)
	var lockError *pq.Error
	require.ErrorAs(t, err, &lockError, "the zero-total candidate must already be locked")
	require.Equal(t, pq.ErrorCode("55P03"), lockError.Code)
	_, err = holder.ExecContext(ctx, `ROLLBACK TO SAVEPOINT bridged_probe`)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, `UPDATE posts SET bridged_upvote_count = 2, bridged_stats_as_of = NOW() WHERE uri = $1`, subject)
	require.NoError(t, err)
	require.NoError(t, holder.Commit())
	select {
	case got := <-results:
		require.NoError(t, got.err)
		require.EqualValues(t, 1, got.deleted)
	case <-ctx.Done():
		t.Fatalf("sweep did not finish: %v", ctx.Err())
	}
	var storedSort time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT sort_at FROM notifications WHERE id = $1`, ids[label]).Scan(&storedSort), "newly bridged group must survive")
	require.True(t, storedSort.Equal(at))
	var emptyCount int
	require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, ids["still_empty"]).Scan(&emptyCount))
	require.Equal(t, 0, emptyCount)
}
