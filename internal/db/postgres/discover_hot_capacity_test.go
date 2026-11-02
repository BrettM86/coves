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
