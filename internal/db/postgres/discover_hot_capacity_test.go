//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotSnapshotCandidateCapacityRollsBackAndRecovers(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "candidate-capacity", []int{300, 200, 100})
	repository := NewDiscoverRepository(db, "snapshot-capacity-secret").(*postgresDiscoverRepo)
	repository.discoverHotCandidateLimit = 2

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 10,
	})
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable,
		"an oversized snapshot must return the retryable capacity sentinel rather than truncated success")
	assert.Empty(t, feed, "capacity refusal must not return a partial feed")
	assert.Nil(t, cursor, "capacity refusal must not return a continuation cursor")
	assert.Equal(t, []int{0, 0, 0}, discoverHotStateCounts(t, db),
		"failed snapshot creation must roll back snapshots, candidates, and checkpoints")

	repository.discoverHotCandidateLimit = 3
	feed, cursor, err = repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 10,
	})
	require.NoError(t, err, "raising the repository limit must recover after capacity refusal")
	assert.Equal(t, expected, discoverURIs(feed), "recovery must return every eligible candidate")
	assert.Nil(t, cursor, "all three eligible candidates fit on the recovered page")
	assert.Equal(t, []int{1, 3, 0}, discoverHotStateCounts(t, db),
		"recovery must persist one complete snapshot without a continuation checkpoint")
}

func TestDiscoverRepo_HotCheckpointStateByteLimitRefusesWithoutPersisting(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	seedDiscoverHotSnapshotCandidates(t, db, "checkpoint-bytes", []int{200, 100})
	repository := NewDiscoverRepository(db, "checkpoint-byte-limit-secret").(*postgresDiscoverRepo)
	repository.discoverHotCheckpointStateByteLimit = 1

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 1,
	})
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable,
		"checkpoint state larger than the byte limit must be refused")
	assert.Empty(t, feed, "checkpoint refusal must not return a partial page")
	assert.Nil(t, cursor, "checkpoint refusal must not return a continuation cursor")
	assert.Equal(t, []int{1, 2, 0}, discoverHotStateCounts(t, db),
		"checkpoint refusal must not persist oversized state")
}

func discoverHotStateCounts(t *testing.T, db *sql.DB) []int {
	t.Helper()

	var snapshots, candidates, checkpoints int
	err := db.QueryRowContext(context.Background(), `
		SELECT
			(SELECT count(*) FROM discover_hot_snapshots),
			(SELECT count(*) FROM discover_hot_candidates),
			(SELECT count(*) FROM discover_hot_checkpoints)
	`).Scan(&snapshots, &candidates, &checkpoints)
	require.NoError(t, err)
	return []int{snapshots, candidates, checkpoints}
}
