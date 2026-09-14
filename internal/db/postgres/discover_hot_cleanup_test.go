//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_CleanupExpiredHotStateIsBounded(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expiredSnapshotID := seedDiscoverHotCleanupState(t, db, "expired", time.Now().Add(-time.Minute), 3, 2)
	unexpiredSnapshotID := seedDiscoverHotCleanupState(t, db, "unexpired", time.Now().Add(time.Hour), 3, 2)
	unexpiredBefore := readDiscoverHotCleanupState(t, db, unexpiredSnapshotID)
	repository := NewDiscoverRepository(db, "cleanup-secret").(*postgresDiscoverRepo)

	const derivedRowBatchSize = 2
	expiredState := readDiscoverHotCleanupState(t, db, expiredSnapshotID)
	for pass := 0; pass < 10 && expiredState.snapshotExists; pass++ {
		reportedRemoved, err := repository.CleanupExpiredDiscoverHotState(context.Background(), derivedRowBatchSize)
		require.NoError(t, err)

		next := readDiscoverHotCleanupState(t, db, expiredSnapshotID)
		removed := expiredState.derivedRows() - next.derivedRows()
		assert.GreaterOrEqual(t, removed, 0, "cleanup pass %d must not add expired derived rows", pass+1)
		assert.LessOrEqual(t, removed, derivedRowBatchSize,
			"cleanup pass %d bypassed the derived-row bound, likely by cascading an expired parent", pass+1)
		assert.Equal(t, int64(removed), reportedRemoved, "cleanup pass %d reported the wrong removed-row count", pass+1)
		if next.snapshotExists && next.derivedRows() == expiredState.derivedRows() {
			t.Fatalf("cleanup pass %d made no progress on expired state: %+v", pass+1, next)
		}
		expiredState = next
	}

	assert.False(t, expiredState.snapshotExists, "the expired parent must be deleted after its derived rows are drained")
	assert.Equal(t, unexpiredBefore, readDiscoverHotCleanupState(t, db, unexpiredSnapshotID),
		"cleanup must leave unexpired snapshots, candidates, and checkpoints intact")
}

func TestDiscoverRepo_CleanupExpiredHotStateReportsCancellation(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expiredSnapshotID := seedDiscoverHotCleanupState(t, db, "canceled", time.Now().Add(-time.Minute), 1, 1)
	before := readDiscoverHotCleanupState(t, db, expiredSnapshotID)
	repository := NewDiscoverRepository(db, "cleanup-cancellation-secret").(*postgresDiscoverRepo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	removed, err := repository.CleanupExpiredDiscoverHotState(ctx, 2)

	assert.ErrorIs(t, err, context.Canceled, "a canceled cleanup must not report success")
	assert.Zero(t, removed)
	assert.Equal(t, before, readDiscoverHotCleanupState(t, db, expiredSnapshotID))
}

type discoverHotCleanupState struct {
	snapshotExists bool
	candidates     int
	checkpoints    int
}

func (s discoverHotCleanupState) derivedRows() int {
	return s.candidates + s.checkpoints
}

func seedDiscoverHotCleanupState(t *testing.T, db *sql.DB, label string, expiresAt time.Time, candidates, checkpoints int) int64 {
	t.Helper()

	ctx := context.Background()
	var snapshotID int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, created_at, expires_at
		) VALUES ($1, $2, NOW(), NOW(), $3)
		RETURNING id
	`, discoverHotAlgorithmVersion, "cleanup-"+label, expiresAt).Scan(&snapshotID)
	require.NoError(t, err)

	for index := 0; index < candidates; index++ {
		_, err = db.ExecContext(ctx, `
			INSERT INTO discover_hot_candidates (
				snapshot_id, uri, community_did, base_rank, created_at
			) VALUES ($1, $2, $3, $4, NOW())
		`, snapshotID, fmt.Sprintf("at://did:plc:%s/post/%d", label, index),
			"did:plc:cleanupcommunity", float64(candidates-index))
		require.NoError(t, err)
	}
	for index := 0; index < checkpoints; index++ {
		positions := fmt.Sprintf(`{"did:plc:community":%d}`, index)
		recent := []string{fmt.Sprintf("community-%d", index)}
		stateDigest := sha256.Sum256([]byte(positions + "\x00" + recent[0]))
		_, err = db.ExecContext(ctx, `
			INSERT INTO discover_hot_checkpoints (
				snapshot_id, state_digest, community_positions, recent_communities
			) VALUES ($1, $2, $3::jsonb, $4)
		`, snapshotID, stateDigest[:], positions, pq.Array(recent))
		require.NoError(t, err)
	}
	return snapshotID
}

func readDiscoverHotCleanupState(t *testing.T, db *sql.DB, snapshotID int64) discoverHotCleanupState {
	t.Helper()

	var state discoverHotCleanupState
	err := db.QueryRowContext(context.Background(), `
		SELECT
			EXISTS (SELECT 1 FROM discover_hot_snapshots WHERE id = $1),
			(SELECT count(*) FROM discover_hot_candidates WHERE snapshot_id = $1),
			(SELECT count(*) FROM discover_hot_checkpoints WHERE snapshot_id = $1)
	`, snapshotID).Scan(&state.snapshotExists, &state.candidates, &state.checkpoints)
	require.NoError(t, err)
	return state
}
