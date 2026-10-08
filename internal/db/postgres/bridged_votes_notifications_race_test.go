//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type bridgedRaceResult struct {
	applied bool
	err     error
}

type bridgedRaceReached struct {
	processID int
	groupSort time.Time
}

func bridgedRaceApply(ctx context.Context, store *BridgedVotesRepository, uri string, total int, asOf time.Time) <-chan bridgedRaceResult {
	results := make(chan bridgedRaceResult, 1)
	go func() {
		applied, err := store.ApplyAggregate(ctx, bridgedvotes.Aggregate{URI: uri, Upvotes: total, AsOf: asOf})
		results <- bridgedRaceResult{applied, err}
	}()
	return results
}

func bridgedRaceResultWithin(t *testing.T, ctx context.Context, results <-chan bridgedRaceResult) bridgedRaceResult {
	t.Helper()
	select {
	case result := <-results:
		require.False(t, notificationRaceDeadlock(result.err), "ApplyAggregate deadlocked: %v", result.err)
		return result
	case <-ctx.Done():
		t.Fatalf("ApplyAggregate did not finish: %v", ctx.Err())
		return bridgedRaceResult{}
	}
}

func bridgedRaceReachedWithin(t *testing.T, ctx context.Context, reached <-chan bridgedRaceReached) bridgedRaceReached {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case state := <-reached:
		return state
	case <-wait.Done():
		t.Fatalf("never reached ApplyUpvoteGroupTx: %v", wait.Err())
		return bridgedRaceReached{}
	}
}

func bridgedRaceBlocked(t *testing.T, db *sql.DB, ctx context.Context, blockingPID int, queryFragment string) {
	t.Helper()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var pid int
		err := db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND $1 = ANY(pg_blocking_pids(pid)) AND wait_event_type = 'Lock'
				AND query ILIKE $2 LIMIT 1`, blockingPID, "%"+queryFragment+"%").Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("%s blocked by the winning subject transaction", queryFragment))
}

func bridgedRacePending(t *testing.T, results <-chan bridgedRaceResult) {
	t.Helper()
	select {
	case result := <-results:
		t.Fatalf("losing ApplyAggregate finished before the winner committed: %+v", result)
	default:
	}
}

// Each row holds its winning subject transaction, proves the competing writer
// waits on that backend, and only then permits the winner to commit.
func TestBridgedVotesNotifications_ConcurrentSubjectChanges(t *testing.T) {
	for _, row := range []struct {
		name string
		kind string
		run  func(*testing.T, bridgedNotificationFixture, context.Context)
	}{
		{"recipient erasure waits for poller", "post", bridgedRaceRecipientErasure},
		{"soft delete wins before poller", "post", bridgedRaceDeleteFirst},
		{"post previous total read under lock", "post", bridgedRacePreviousTotal},
		{"comment previous total read under lock", "comment", bridgedRacePreviousTotal},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newBridgedNotificationFixture(t, row.kind, "native")
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			t.Cleanup(cancel)
			row.run(t, f, ctx)
		})
	}
}

func bridgedRaceRecipientErasure(t *testing.T, f bridgedNotificationFixture, ctx context.Context) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	reached := make(chan bridgedRaceReached, 1)
	repo := f.repository()
	decorated := bridgedNotificationRepositoryDecorator{Repository: repo}
	decorated.apply = func(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		reached <- bridgedRaceReached{processID: pid}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return repo.ApplyUpvoteGroupTx(ctx, tx, intent)
	}
	apply := bridgedRaceApply(ctx, f.store(decorated), f.subject, 2, f.now.Add(time.Minute))
	state := bridgedRaceReachedWithin(t, ctx, reached)
	deletes := make(chan error, 1)
	go func() { deletes <- NewUserRepository(f.db).Delete(ctx, f.recipient) }()
	notificationRaceBlockedDelete(t, f.db, ctx, state.processID, "DELETE FROM posts")
	notificationRaceRequirePending(t, deletes)
	unblock()
	result := bridgedRaceResultWithin(t, ctx, apply)
	require.NoError(t, result.err)
	require.True(t, result.applied)
	select {
	case err := <-deletes:
		require.False(t, notificationRaceDeadlock(err), "Delete deadlocked: %v", err)
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("Delete did not finish: %v", ctx.Err())
	}
	require.Zero(t, f.groupCount(t), "erasure must remove the poller's upvote group")
}

func bridgedRaceDeleteFirst(t *testing.T, f bridgedNotificationFixture, ctx context.Context) {
	transaction, pid := notificationRaceTransaction(t, f.db, ctx)
	_, err := transaction.ExecContext(ctx, `UPDATE posts SET deleted_at = now() WHERE uri = $1`, f.subject)
	require.NoError(t, err)
	apply := bridgedRaceApply(ctx, f.store(f.repository()), f.subject, 2, f.now.Add(time.Minute))
	bridgedRaceBlocked(t, f.db, ctx, pid, f.table)
	bridgedRacePending(t, apply)
	require.NoError(t, transaction.Commit())
	result := bridgedRaceResultWithin(t, ctx, apply)
	require.NoError(t, result.err)
	require.False(t, result.applied, "deleted subject must not accept an aggregate")
	require.Zero(t, f.groupCount(t))
}

func bridgedRacePreviousTotal(t *testing.T, f bridgedNotificationFixture, ctx context.Context) {
	seedStoredAggregate(t, ctx, f.db, f.table, f.subject, 3, 0, f.now.Add(-time.Hour))
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	reached := make(chan bridgedRaceReached, 1)
	committedSort := make(chan time.Time, 1)
	var first atomic.Bool
	var mu sync.Mutex
	bumps := 0
	repo := f.repository()
	decorated := bridgedNotificationRepositoryDecorator{Repository: repo}
	decorated.apply = func(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
		if intent.Action == notifications.UpvoteGroupBump {
			mu.Lock()
			bumps++
			mu.Unlock()
		}
		isFirst := !first.Swap(true)
		if isFirst {
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			reached <- bridgedRaceReached{processID: pid}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := repo.ApplyUpvoteGroupTx(ctx, tx, intent); err != nil {
			return err
		}
		if isFirst {
			var sortAt time.Time
			if err := tx.QueryRowContext(ctx, `SELECT sort_at FROM notifications
				WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, f.recipient, f.subject).Scan(&sortAt); err != nil {
				return err
			}
			committedSort <- sortAt.UTC().Truncate(time.Microsecond)
		}
		return nil
	}
	store := f.store(decorated)
	firstApply := bridgedRaceApply(ctx, store, f.subject, 5, f.now)
	state := bridgedRaceReachedWithin(t, ctx, reached)
	secondAsOf := f.now.Add(time.Millisecond)
	secondApply := bridgedRaceApply(ctx, store, f.subject, 5, secondAsOf)
	bridgedRaceBlocked(t, f.db, ctx, state.processID, f.table)
	bridgedRacePending(t, secondApply)
	unblock()
	firstResult := bridgedRaceResultWithin(t, ctx, firstApply)
	require.NoError(t, firstResult.err)
	require.True(t, firstResult.applied)
	var firstSort time.Time
	select {
	case firstSort = <-committedSort:
	case <-ctx.Done():
		t.Fatalf("first upvote group sort_at was not recorded: %v", ctx.Err())
	}
	secondResult := bridgedRaceResultWithin(t, ctx, secondApply)
	require.NoError(t, secondResult.err)
	require.True(t, secondResult.applied)
	var storedTotal int
	var storedAsOf time.Time
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT bridged_upvote_count, bridged_stats_as_of FROM `+f.table+` WHERE uri = $1`, f.subject).Scan(&storedTotal, &storedAsOf))
	require.Equal(t, 5, storedTotal)
	require.True(t, storedAsOf.Equal(secondAsOf.UTC().Truncate(time.Microsecond)))
	require.Equal(t, 1, f.groupCount(t))
	require.True(t, f.groupSort(t).Equal(firstSort), "the equal-total second application must not bump")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, bumps, "only the increase from the locked previous total sends Bump")
}
