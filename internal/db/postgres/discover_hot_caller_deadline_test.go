//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotCallerDeadlineIsRetryableButCancellationIsNot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	lockKey := discoverHotSnapshotLockKey(discoverHotViewerScope(""))
	lockConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lockConnection.Close()) })
	_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, unlockErr := lockConnection.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
		require.NoError(t, unlockErr)
	})

	repository := NewDiscoverRepository(db, "caller-deadline-secret").(*postgresDiscoverRepo)
	repository.discoverHotWorkDeadline = time.Second

	deadlineContext, cancelDeadline := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelDeadline()
	_, _, deadlineErr := repository.GetDiscover(deadlineContext, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	assert.ErrorIs(t, deadlineErr, context.DeadlineExceeded)
	assert.ErrorIs(t, deadlineErr, discover.ErrDiscoverUnavailable,
		"caller deadline exhaustion must remain identifiable as retryable Discover unavailability")

	canceledContext, cancel := context.WithCancel(ctx)
	cancel()
	_, _, canceledErr := repository.GetDiscover(canceledContext, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	assert.ErrorIs(t, canceledErr, context.Canceled)
	assert.NotErrorIs(t, canceledErr, discover.ErrDiscoverUnavailable,
		"plain caller cancellation must not be relabeled as retryable unavailability")
}
