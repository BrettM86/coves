//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The algorithm version as a compatibility boundary.
//
// A snapshot is a frozen page order, and the cursor a reader holds is a pointer
// into one. A change to the rank formula (the hosted-community bonus at version 2,
// the 0.8 decay exponent at version 3) changes what base_rank MEANS, so a
// snapshot built before it and one built after it are two different orderings
// wearing the same name: a reader paging through the old one would see the old
// ranking silently, and a reader whose next page lands on a rebuilt snapshot
// would see posts repeat or vanish. algorithm_version is the field that keeps
// those two apart, and bumping it is the whole mechanism — it makes stale
// snapshots unreusable and stale cursors invalid in one move.
//
// Every assertion below is written against the LITERAL 2 and 3 rather than the
// discoverHotAlgorithmVersion constant. A test that asserts a row carries the
// constant the writer used is a tautology: it passes at version 1, at version 2
// and at any version anyone ever sets, so it cannot notice the bump failing to
// happen. The literal is the point.
const (
	supersededDiscoverHotAlgorithmVersion = 2
	currentDiscoverHotAlgorithmVersion    = 3
)

func TestDiscoverRepo_HotNewSnapshotsRecordAlgorithmVersionThree(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "algorithm-version-build", []int{400, 300, 200})

	feed, _ := readDiscoverHotRoot(t, NewDiscoverRepository(db, "algorithm-version-build-secret"), "", 10)
	require.Equal(t, expected, feed)

	versions := discoverHotSnapshotVersions(t, db, "")
	require.Len(t, versions, 1, "the read must have built exactly one anonymous snapshot")
	assert.Equal(t, currentDiscoverHotAlgorithmVersion, versions[0],
		"a snapshot built under the gravity-0.8 ranking must be stamped version 3")
}

func TestDiscoverRepo_HotRefusesSupersededAlgorithmVersionSnapshot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "algorithm-version-reuse", []int{400, 300, 200, 100})
	// The seeded snapshots rank the same real posts in the OPPOSITE order to the
	// one a fresh build produces. Reuse is then visible in the feed itself: a
	// reused snapshot serves its own stored order, and a rebuilt one serves the
	// ranking. Asserting on snapshot ids alone would not catch a read that
	// created a new row and then paged from the stale one.
	stale := slices.Clone(expected)
	slices.Reverse(stale)

	repository := NewDiscoverRepository(db, "algorithm-version-reuse-secret").(*postgresDiscoverRepo)
	const controlViewerDID = "did:plc:algorithmversionviewer"
	createTestUser(t, db, "algorithmversionviewer.test", controlViewerDID)

	supersededID, supersededCursor := seedDiscoverHotVersionedSnapshot(t, db, repository,
		supersededDiscoverHotAlgorithmVersion, "", expected)
	currentID, currentCursor := seedDiscoverHotVersionedSnapshot(t, db, repository,
		currentDiscoverHotAlgorithmVersion, controlViewerDID, expected)

	t.Run("a version two snapshot is not reused", func(t *testing.T) {
		feed, _ := readDiscoverHotRoot(t, repository, "", 10)
		assert.Equal(t, expected, feed,
			"a superseded snapshot must be rebuilt, and the rebuild ranks the real candidates")

		rebuilt := discoverHotSnapshotVersionsExcluding(t, db, "", supersededID)
		require.Len(t, rebuilt, 1, "the read must have built exactly one replacement snapshot")
		assert.Equal(t, currentDiscoverHotAlgorithmVersion, rebuilt[0], "the replacement must be stamped version 3")
	})

	t.Run("a version two cursor is invalid", func(t *testing.T) {
		feed, next, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
			Sort:   "hot",
			Limit:  2,
			Cursor: &supersededCursor,
		})
		assert.ErrorIs(t, err, discover.ErrInvalidCursor,
			"a cursor into a superseded snapshot must be refused, not silently served")
		assert.Empty(t, feed)
		assert.Nil(t, next)
	})

	// The control. Everything about these two fixtures is identical but the
	// version, so if the version-3 snapshot is not reused and its cursor is not
	// accepted, the fixtures are malformed and the assertions above prove
	// nothing about the version.
	t.Run("a version three snapshot is reused", func(t *testing.T) {
		feed, _ := readDiscoverHotRoot(t, repository, controlViewerDID, 10)
		assert.Equal(t, stale, feed, "a current, fresh, in-scope snapshot must be served from its stored order")
		assert.Equal(t, []int{currentDiscoverHotAlgorithmVersion},
			discoverHotSnapshotVersions(t, db, controlViewerDID),
			"reuse must not have built a second snapshot in this scope")
	})

	t.Run("a version three cursor is accepted", func(t *testing.T) {
		feed, _, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
			ViewerDID: controlViewerDID,
			Sort:      "hot",
			Limit:     2,
			Cursor:    &currentCursor,
		})
		require.NoError(t, err, "a cursor into a current snapshot must still page")
		assert.Equal(t, stale[:2], discoverURIs(feed))

		// Paging a cursor must read the snapshot the cursor names, not build a
		// fresh one and serve that: a rebuild here would return a plausible page
		// of the right length in the right order, and the reader would silently
		// cross from one frozen ordering into another mid-feed.
		assert.Equal(t, []int64{currentID}, discoverHotSnapshotIDs(t, db, controlViewerDID),
			"serving an accepted cursor must not have created a second snapshot in this scope")
	})
}

// seedDiscoverHotVersionedSnapshot writes a snapshot that is valid in every
// respect except, possibly, its algorithm version: fresh, unexpired, in the
// given viewer's scope, with candidate rows for real posts and a checkpoint at
// the root of its ordering. It returns the snapshot id and a cursor built the
// way production builds one.
//
// The rows are written directly rather than through a read, because a read
// cannot be asked to produce a snapshot at a version the code does not currently
// write — which is exactly the row this suite needs.
func seedDiscoverHotVersionedSnapshot(
	t *testing.T,
	db *sql.DB,
	repository *postgresDiscoverRepo,
	algorithmVersion int,
	viewerDID string,
	uris []string,
) (int64, string) {
	t.Helper()

	ctx := context.Background()
	var snapshotID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES ($1, $2, NOW(), NOW() + INTERVAL '30 minutes')
		RETURNING id
	`, algorithmVersion, discoverHotViewerScope(viewerDID)).Scan(&snapshotID))

	// Descending base ranks assigned back to front, so the stored order is the
	// reverse of the ranking a rebuild would compute.
	for position, uri := range uris {
		baseRank := float64(position + 1)
		_, err := db.ExecContext(ctx, `
			INSERT INTO discover_hot_candidates (snapshot_id, uri, community_did, base_rank, created_at)
			SELECT $1, p.uri, p.community_did, $3, p.created_at
			FROM posts p
			WHERE p.uri = $2
		`, snapshotID, uri, baseRank)
		require.NoErrorf(t, err, "seeding candidate %s", uri)
	}
	var candidates int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM discover_hot_candidates WHERE snapshot_id = $1`, snapshotID).Scan(&candidates))
	require.Equal(t, len(uris), candidates, "every seeded candidate must reference a real post")

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	checkpointID, err := repository.storeDiscoverHotCheckpoint(ctx, tx, snapshotID, discover.DiscoverHotSelectionState{
		CommunityPositions: map[string]int{},
		RecentCommunities:  []string{},
	})
	require.NoError(t, err, "seeding the checkpoint the cursor points at")
	require.NoError(t, tx.Commit())

	cursor, err := repository.buildDiscoverHotCursor(snapshotID, checkpointID, viewerDID)
	require.NoError(t, err)
	return snapshotID, cursor
}

// discoverHotSnapshotIDs returns every snapshot id in a viewer scope, oldest
// first, so a test can assert which snapshots exist rather than how many.
func discoverHotSnapshotIDs(t *testing.T, db *sql.DB, viewerDID string) []int64 {
	t.Helper()

	var ids pq.Int64Array
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT coalesce(array_agg(id ORDER BY id), '{}')
		FROM discover_hot_snapshots
		WHERE viewer_scope = $1
	`, discoverHotViewerScope(viewerDID)).Scan(&ids))
	return ids
}

func discoverHotSnapshotVersions(t *testing.T, db *sql.DB, viewerDID string) []int {
	t.Helper()
	return queryDiscoverHotSnapshotVersions(t, db, viewerDID, 0)
}

func discoverHotSnapshotVersionsExcluding(t *testing.T, db *sql.DB, viewerDID string, excludedID int64) []int {
	t.Helper()
	return queryDiscoverHotSnapshotVersions(t, db, viewerDID, excludedID)
}

func queryDiscoverHotSnapshotVersions(t *testing.T, db *sql.DB, viewerDID string, excludedID int64) []int {
	t.Helper()

	var versions pq.Int64Array
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT coalesce(array_agg(algorithm_version ORDER BY id), '{}')
		FROM discover_hot_snapshots
		WHERE viewer_scope = $1
			AND id <> $2
	`, discoverHotViewerScope(viewerDID), excludedID).Scan(&versions))

	result := make([]int, len(versions))
	for i, version := range versions {
		result[i] = int(version)
	}
	return result
}
