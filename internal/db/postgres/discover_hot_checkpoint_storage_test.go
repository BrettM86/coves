//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotCheckpointBelowByteLimitPersistsAndDeduplicates(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	var snapshotID int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES ($1, $2, NOW(), NOW() + INTERVAL '5 minutes')
		RETURNING id
	`, discoverHotAlgorithmVersion, "checkpoint-index-size").Scan(&snapshotID)
	require.NoError(t, err)

	positions := make(map[string]int, 128)
	recent := make([]string, 0, 20)
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	for index := range 128 {
		digest := sha256.Sum256([]byte(strconv.Itoa(index)))
		community := "did:plc:" + strings.ToLower(encoding.EncodeToString(digest[:])[:24])
		positions[community] = 1
		if index < cap(recent) {
			recent = append(recent, community)
		}
	}
	state := discover.DiscoverHotSelectionState{
		CommunityPositions: positions,
		RecentCommunities:  recent,
	}
	positionsJSON, err := json.Marshal(positions)
	require.NoError(t, err)
	recentJSON, err := json.Marshal(recent)
	require.NoError(t, err)
	stateBytes := len(positionsJSON) + len(recentJSON)
	require.Greater(t, stateBytes, 2704, "fixture must exceed PostgreSQL's B-tree entry-size limit")
	require.Less(t, stateBytes, 64*1024, "fixture must remain below the advertised checkpoint limit")

	repository := NewDiscoverRepository(db, "checkpoint-index-size-secret").(*postgresDiscoverRepo)
	repository.discoverHotCheckpointStateByteLimit = 64 * 1024
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()

	firstID, err := repository.storeDiscoverHotCheckpoint(ctx, tx, snapshotID, state)
	require.NoError(t, err, "valid %d-byte checkpoint state must fit the storage schema", stateBytes)
	secondID, err := repository.storeDiscoverHotCheckpoint(ctx, tx, snapshotID, state)
	require.NoError(t, err)
	require.Equal(t, firstID, secondID, "identical checkpoint state must deduplicate")
}

func TestDiscoverRepo_HotCheckpointAdmissionUsesDatabaseCanonicalBytes(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	var snapshotID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES ($1, $2, NOW(), NOW() + INTERVAL '5 minutes')
		RETURNING id
	`, discoverHotAlgorithmVersion, "checkpoint-canonical-size").Scan(&snapshotID))

	state := discoverHotBoundaryCheckpointState(65_527)
	positionsJSON, err := json.Marshal(state.CommunityPositions)
	require.NoError(t, err)
	applicationBytes := len(positionsJSON) + len("[]")
	require.Equal(t, 65_535, applicationBytes, "fixture must be one compact-JSON byte below its configured limit")

	var databaseCanonicalBytes int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT octet_length($1::jsonb::text)
			+ octet_length(array_to_json($2::text[])::text)
	`, positionsJSON, pq.Array([]string{})).Scan(&databaseCanonicalBytes))
	require.Equal(t, 65_536, databaseCanonicalBytes,
		"fixture must expose PostgreSQL's canonical JSON spacing at the 64 KiB boundary")

	repository := NewDiscoverRepository(db, "checkpoint-canonical-size-secret").(*postgresDiscoverRepo)
	repository.discoverHotCheckpointStateByteLimit = 65_535
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()

	_, err = repository.storeDiscoverHotCheckpoint(ctx, tx, snapshotID, state)
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable,
		"admission must measure the same canonical bytes as the database constraint")
}

func TestMigration048DiscoverHotCheckpointCanonicalByteBoundary(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	var snapshotID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES ($1, $2, NOW(), NOW() + INTERVAL '5 minutes')
		RETURNING id
	`, discoverHotAlgorithmVersion, "checkpoint-database-boundary").Scan(&snapshotID))

	insert := func(state discover.DiscoverHotSelectionState, digestByte byte) error {
		positionsJSON, err := json.Marshal(state.CommunityPositions)
		require.NoError(t, err)
		digest := make([]byte, sha256.Size)
		digest[0] = digestByte
		_, err = db.ExecContext(ctx, `
			INSERT INTO discover_hot_checkpoints (
				snapshot_id, state_digest, community_positions, recent_communities
			) VALUES ($1, $2, $3::jsonb, $4)
		`, snapshotID, digest, positionsJSON, pq.Array([]string{}))
		return err
	}

	assert.NoError(t, insert(discoverHotBoundaryCheckpointState(65_527), 1),
		"exactly 65,536 database-canonical bytes must satisfy the checkpoint constraint")
	assert.Error(t, insert(discoverHotBoundaryCheckpointState(65_528), 2),
		"65,537 database-canonical bytes must violate the checkpoint constraint")
}

func discoverHotBoundaryCheckpointState(keyBytes int) discover.DiscoverHotSelectionState {
	return discover.DiscoverHotSelectionState{
		CommunityPositions: map[string]int{strings.Repeat("a", keyBytes): 1},
		RecentCommunities:  []string{},
	}
}
