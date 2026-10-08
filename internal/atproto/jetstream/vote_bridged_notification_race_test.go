//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type bridgedVoteRaceRepository struct {
	notifications.Repository
	reached chan<- int
	release <-chan struct{}
}

func (r bridgedVoteRaceRepository) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	var pid int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return err
	}
	r.reached <- pid
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Repository.ApplyUpvoteGroupTx(ctx, tx, intent)
}

type bridgedVoteRaceApplyResult struct {
	applied bool
	err     error
}

func bridgedVoteRacePID(t *testing.T, ctx context.Context, reached <-chan int, operation string) int {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case pid := <-reached:
		return pid
	case <-wait.Done():
		t.Fatalf("%s never reached ApplyUpvoteGroupTx: %v", operation, wait.Err())
		return 0
	}
}

func bridgedVoteRaceBlocked(t *testing.T, db *sql.DB, ctx context.Context, winnerPID int, queryFragment string) {
	t.Helper()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var pid int
		err := db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND $1 = ANY(pg_blocking_pids(pid)) AND wait_event_type = 'Lock'
				AND query ILIKE $2 LIMIT 1`, winnerPID, "%"+queryFragment+"%").Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("%s waiting behind winning subject row lock", queryFragment))
}

func bridgedVoteRaceError(t *testing.T, ctx context.Context, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		var databaseError *pq.Error
		require.False(t, errors.As(err, &databaseError) && databaseError.Code == "40P01", "vote delete deadlocked: %v", err)
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("vote delete did not finish: %v", ctx.Err())
	}
}

func bridgedVoteRaceApplied(t *testing.T, ctx context.Context, result <-chan bridgedVoteRaceApplyResult) {
	t.Helper()
	select {
	case outcome := <-result:
		var databaseError *pq.Error
		require.False(t, errors.As(outcome.err, &databaseError) && databaseError.Code == "40P01", "bridged aggregate deadlocked: %v", outcome.err)
		require.NoError(t, outcome.err)
		require.True(t, outcome.applied)
	case <-ctx.Done():
		t.Fatalf("bridged aggregate did not finish: %v", ctx.Err())
	}
}

func TestVoteConsumer_BridgedIncreaseAndLastNativeVoteDeleteSerialize(t *testing.T) {
	for _, order := range []string{"native delete first", "bridged increase first"} {
		t.Run(order, func(t *testing.T) {
			fixture := newUpvoteGroupFixture(t)
			// The fixture's author can live on the bridge host; the recipient must be native.
			insertBridgedUserOnPDS(t, fixture.db, fixture.author, testkit.UniqueID(t)+"author.test", bridgedTestNativePDS)
			notificationRepo := postgres.NewNotificationRepository(fixture.db, postgres.WithBridgedUpvoteTotals())
			consumer := fixture.consumer(WithVoteNotifications(notificationRepo))
			voter := fixture.voter(t)
			voteURI := deliverGroupVote(t, consumer, voter, fixture.post, "up", fixture.createdAt)
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post))
			key := strings.TrimPrefix(voteURI, "at://"+voter+"/social.coves.feed.vote/")
			deleteEvent := revCommitEvent(voter, "social.coves.feed.vote", "delete", key,
				testkit.TID(), "", time.Now().UnixMicro()+1_000_000, nil)
			var now time.Time
			require.NoError(t, fixture.db.QueryRow(`SELECT now()`).Scan(&now))
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			t.Cleanup(cancel)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			reached := make(chan int, 1)
			blocking := bridgedVoteRaceRepository{Repository: notificationRepo, reached: reached, release: release}
			voteResults := make(chan error, 1)
			aggregateResults := make(chan bridgedVoteRaceApplyResult, 1)
			startDelete := func(repo notifications.Repository) {
				voteConsumer := fixture.consumer(WithVoteNotifications(repo))
				go func() { voteResults <- voteConsumer.HandleEvent(ctx, deleteEvent) }()
			}
			startAggregate := func(repo notifications.Repository) {
				store := postgres.NewBridgedVotesRepository(fixture.db, postgres.WithBridgedVoteNotifications(repo, nil))
				go func() {
					applied, err := store.ApplyAggregate(ctx, bridgedvotes.Aggregate{URI: fixture.post, Upvotes: 2, AsOf: now.Add(time.Minute)})
					aggregateResults <- bridgedVoteRaceApplyResult{applied, err}
				}()
			}
			if order == "native delete first" {
				startDelete(blocking)
				pid := bridgedVoteRacePID(t, ctx, reached, "native vote delete")
				startAggregate(notificationRepo)
				bridgedVoteRaceBlocked(t, fixture.db, ctx, pid, "posts")
			} else {
				startAggregate(blocking)
				pid := bridgedVoteRacePID(t, ctx, reached, "bridged aggregate")
				startDelete(notificationRepo)
				bridgedVoteRaceBlocked(t, fixture.db, ctx, pid, "UPDATE posts")
			}
			select {
			case err := <-voteResults:
				t.Fatalf("vote delete completed before the winner was released: %v", err)
			default:
			}
			select {
			case result := <-aggregateResults:
				t.Fatalf("bridged aggregate completed before the winner was released: %+v", result)
			default:
			}
			unblock()
			bridgedVoteRaceError(t, ctx, voteResults)
			bridgedVoteRaceApplied(t, ctx, aggregateResults)
			exists, active := voteRowState(t, fixture.db, voteURI)
			require.True(t, exists && !active, "the last native upvote must be retracted")
			require.Equal(t, 1, groupCount(t, fixture.db, fixture.author, fixture.post),
				"bridged upvotes keep the group after the last native upvote is deleted")
		})
	}
}
