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
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func retentionBatchReplies(t *testing.T, db *sql.DB, recipient, prefix string, oldCount, recentCount int, oldAt, recentAt, newestAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, sort_at)
		SELECT $1, 'postReply', $2 || number::text, 'bafyretentionbatch', $1, $3, $3,
			CASE WHEN number <= $4::integer THEN $5::timestamptz
				WHEN number = $4::integer + $6::integer + 1 THEN $8::timestamptz ELSE $7::timestamptz END
		FROM generate_series(1, $4::integer + $6::integer + 1) AS number`, recipient, prefix,
		"at://"+recipient+"/social.coves.community.postv2/batch", oldCount, oldAt, recentCount, recentAt, newestAt)
	require.NoError(t, err)
}

func retentionBatchCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(query, args...).Scan(&count))
	return count
}

type retentionBatchFixture struct {
	eligibleQuery string
	eligibleArgs  []any
	controlsQuery string
	controlsArgs  []any
	lockedID      int64
}

func retentionBatchSetup(t *testing.T, db *sql.DB, sweep string, oldCount int) retentionBatchFixture {
	t.Helper()
	recipient := retentionUser(t, db)
	prefix := "at://" + recipient + "/social.coves.community.comment/batch-"
	now := time.Date(2026, time.December, 1, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	fixture := retentionBatchFixture{}
	switch sweep {
	case "read", "unread_cap":
		oldAt := now.Add(-721 * time.Hour)
		recentAt := now.Add(-719 * time.Hour)
		newestAt := now.Add(time.Hour)
		recentCount := 1
		if sweep == "read" {
			retentionSeenAt(t, db, recipient, now)
		} else {
			retentionSeenAt(t, db, recipient, now.Add(-5000*time.Hour))
			oldAt = now.Add(-4800 * time.Hour)
			recentAt = now.Add(-time.Hour)
			newestAt = now
			recentCount = 501
		}
		retentionBatchReplies(t, db, recipient, prefix, oldCount, recentCount, oldAt, recentAt, newestAt)
		fixture.eligibleQuery = `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND record_uri LIKE $2 AND sort_at = $3`
		fixture.eligibleArgs = []any{recipient, prefix + "%", oldAt}
		fixture.controlsQuery = `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND record_uri LIKE $2 AND sort_at <> $3`
		fixture.controlsArgs = []any{recipient, prefix + "%", oldAt}
		require.NoError(t, db.QueryRow(`SELECT id FROM notifications WHERE record_uri = $1`, prefix+"1").Scan(&fixture.lockedID))
	case "empty_groups":
		subjectPrefix := "at://" + recipient + "/social.coves.community.postv2/batch-"
		_, err := db.Exec(`INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
			SELECT $1, 'upvote', $2 || number::text, $2 || number::text, $3
			FROM generate_series(1, $4) AS number`, recipient, subjectPrefix, now, oldCount)
		require.NoError(t, err)
		votes := qualifyingUpvoteFixture{db: db}
		ids := make(map[string]int64)
		for _, label := range []string{"kept_first", "kept_second"} {
			retentionRow(t, db, ids, label, recipient, "upvote", now)
			votes.insertVote(t, "did:plc:"+testkit.UniqueID(t),
				"at://"+recipient+"/social.coves.community.postv2/"+label, "up", now, false)
		}
		fixture.eligibleQuery = `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri LIKE $2`
		fixture.eligibleArgs = []any{recipient, subjectPrefix + "%"}
		fixture.controlsQuery = `SELECT count(*) FROM notifications WHERE id = ANY($1)`
		fixture.controlsArgs = []any{pq.Array([]int64{ids["kept_first"], ids["kept_second"]})}
		require.NoError(t, db.QueryRow(`SELECT id FROM notifications WHERE recipient_did = $1 AND subject_uri = $2`,
			recipient, subjectPrefix+"1").Scan(&fixture.lockedID))
	case "hidden_references":
		// Every reference is unindexed, so each row reads hidden; only age
		// (database time) separates the eligible rows from the two controls.
		_, err := db.Exec(`INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
			SELECT $1, 'postReply', $2 || CASE WHEN number <= $3::integer THEN 'old-' ELSE 'recent-' END || number::text,
				'bafyretentionbatch', $1, $4, $4, now(),
				now() - CASE WHEN number <= $3::integer THEN interval '200 hours' ELSE interval '100 hours' END
			FROM generate_series(1, $3::integer + 2) AS number`,
			recipient, prefix, oldCount, "at://"+recipient+"/social.coves.community.postv2/batch")
		require.NoError(t, err)
		fixture.eligibleQuery = `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND record_uri LIKE $2`
		fixture.eligibleArgs = []any{recipient, prefix + "old-%"}
		fixture.controlsQuery = `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND record_uri LIKE $2`
		fixture.controlsArgs = []any{recipient, prefix + "recent-%"}
		require.NoError(t, db.QueryRow(`SELECT id FROM notifications WHERE record_uri = $1`, prefix+"old-1").Scan(&fixture.lockedID))
	default:
		t.Fatalf("unknown retention sweep %q", sweep)
	}
	return fixture
}

func retentionBatchSweeps() []struct {
	name  string
	sweep func(notifications.RetentionSweeper, context.Context) (int64, error)
} {
	return []struct {
		name  string
		sweep func(notifications.RetentionSweeper, context.Context) (int64, error)
	}{
		{"read", notifications.RetentionSweeper.SweepReadNotifications},
		{"unread_cap", notifications.RetentionSweeper.SweepUnreadOverflow},
		{"empty_groups", notifications.RetentionSweeper.SweepEmptyUpvoteGroups},
		{"hidden_references", notifications.RetentionSweeper.SweepHiddenReferenceNotifications},
	}
}

func TestNotificationRetention_PerStatementBatchCap(t *testing.T) {
	for _, test := range retentionBatchSweeps() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			fixture := retentionBatchSetup(t, db, test.name, 10001)
			controls := 2
			if test.name == "unread_cap" {
				controls = 502
			}
			require.Equal(t, 10001, retentionBatchCount(t, db, fixture.eligibleQuery, fixture.eligibleArgs...))
			require.Equal(t, controls, retentionBatchCount(t, db, fixture.controlsQuery, fixture.controlsArgs...))
			sweeper := retentionSweeper(t, db)

			deleted, err := test.sweep(sweeper, context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 10000, deleted, "one statement must delete at most 10000 eligible rows")
			require.Equal(t, 1, retentionBatchCount(t, db, fixture.eligibleQuery, fixture.eligibleArgs...))
			require.Equal(t, controls, retentionBatchCount(t, db, fixture.controlsQuery, fixture.controlsArgs...))

			deleted, err = test.sweep(sweeper, context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, deleted)
			require.Zero(t, retentionBatchCount(t, db, fixture.eligibleQuery, fixture.eligibleArgs...))
			require.Equal(t, controls, retentionBatchCount(t, db, fixture.controlsQuery, fixture.controlsArgs...))
		})
	}
}

func TestNotificationRetention_SkipsLockedRows(t *testing.T) {
	for _, test := range retentionBatchSweeps() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			fixture := retentionBatchSetup(t, db, test.name, 2)
			controls := 2
			if test.name == "unread_cap" {
				controls = 502
			}
			require.Equal(t, 2, retentionBatchCount(t, db, fixture.eligibleQuery, fixture.eligibleArgs...))
			require.Equal(t, controls, retentionBatchCount(t, db, fixture.controlsQuery, fixture.controlsArgs...))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			transaction, _ := notificationRaceTransaction(t, db, ctx)
			var lockedID int64
			require.NoError(t, transaction.QueryRowContext(ctx,
				`SELECT id FROM notifications WHERE id = $1 FOR UPDATE`, fixture.lockedID).Scan(&lockedID))
			require.Equal(t, fixture.lockedID, lockedID)

			deleted, err := test.sweep(retentionSweeper(t, db), ctx)
			require.NoError(t, err, "sweep must not wait for another writer's notification row lock")
			require.EqualValues(t, 1, deleted, "the unlocked eligible row must be deleted")
			require.Equal(t, 1, retentionBatchCount(t, db, fixture.eligibleQuery, fixture.eligibleArgs...))
			require.Equal(t, controls, retentionBatchCount(t, db, fixture.controlsQuery, fixture.controlsArgs...))
			var survivor int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE id = $1`, lockedID).Scan(&survivor))
			require.Equal(t, lockedID, survivor)
		})
	}
}

func TestNotificationRetention_EmptyGroupRechecksAfterCandidateLocks(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	recipient := retentionUser(t, db)
	voter := "did:plc:" + testkit.UniqueID(t)
	now := time.Date(2026, time.December, 1, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	ids := make(map[string]int64)
	retentionRow(t, db, ids, "blocked_group", recipient, "upvote", now)
	retentionRow(t, db, ids, "empty_group", recipient, "upvote", now)
	votes := qualifyingUpvoteFixture{db: db}
	votes.insertBlock(t, recipient, voter)
	votes.insertVote(t, voter, "at://"+recipient+"/social.coves.community.postv2/blocked_group", "up", now, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	holder, holderPID := notificationRaceTransaction(t, db, ctx)
	_, err := holder.ExecContext(ctx, `LOCK TABLE notifications IN SHARE MODE`)
	require.NoError(t, err)

	type result struct {
		deleted int64
		err     error
	}
	results := make(chan result, 1)
	sweeper := retentionSweeper(t, db)
	go func() {
		deleted, err := sweeper.SweepEmptyUpvoteGroups(ctx)
		results <- result{deleted, err}
	}()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case got := <-results:
			return false, fmt.Errorf("sweep returned (%d, %v) before its DELETE waited for the table lock", got.deleted, got.err)
		default:
		}
		var waiterPID int
		err := db.QueryRowContext(ctx, `SELECT pid FROM pg_locks
			WHERE relation = 'notifications'::regclass AND locktype = 'relation'
				AND mode = 'RowExclusiveLock' AND NOT granted
				AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, holderPID).Scan(&waiterPID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("the sweep's DELETE waiting for a relation lock after locking its candidates"))

	_, err = holder.ExecContext(ctx, `SAVEPOINT retention_probe`)
	require.NoError(t, err)
	var id int64
	err = holder.QueryRowContext(ctx, `SELECT id FROM notifications WHERE id = $1 FOR UPDATE NOWAIT`, ids["blocked_group"]).Scan(&id)
	var lockError *pq.Error
	require.ErrorAs(t, err, &lockError, "the sweep must already hold the blocked group's row lock")
	require.Equal(t, pq.ErrorCode("55P03"), lockError.Code, "the candidate must be locked before DELETE starts")
	_, err = holder.ExecContext(ctx, `ROLLBACK TO SAVEPOINT retention_probe`)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, `DELETE FROM user_blocks WHERE blocker_did = $1 AND blocked_did = $2`, recipient, voter)
	require.NoError(t, err)
	require.NoError(t, holder.Commit())

	select {
	case got := <-results:
		require.NoError(t, got.err)
		require.EqualValues(t, 1, got.deleted, "the newly qualifying group must be kept while the still-empty group is deleted")
	case <-ctx.Done():
		t.Fatalf("sweep did not finish after the holder committed: %v", ctx.Err())
	}
	require.Equal(t, map[string]bool{"blocked_group": true}, retentionSurvivors(t, db, ids))
}
