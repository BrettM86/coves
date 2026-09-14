//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotAnonymousRefreshReusesSnapshotFromRoot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "sequential", []int{9_000, 8_000, 7_000, 6_000, 5_000})

	firstPage, firstCursor := readDiscoverHotRoot(t, NewDiscoverRepository(db, "snapshot-reuse-secret"), "", 3)
	secondPage, secondCursor := readDiscoverHotRoot(t, NewDiscoverRepository(db, "snapshot-reuse-secret"), "", 3)

	assert.Equal(t, expected[:3], firstPage)
	assert.Equal(t, firstPage, secondPage, "a reused snapshot must begin from its root checkpoint on every no-cursor request")
	require.NotNil(t, firstCursor)
	require.NotNil(t, secondCursor)
	assert.Equal(t, *firstCursor, *secondCursor, "identical root selections should reuse the same immutable checkpoint")
	assert.Equal(t, 1, countDiscoverHotSnapshots(t, db, ""), "anonymous refreshes inside thirty seconds must share one snapshot")
}

func TestDiscoverRepo_HotConcurrentAnonymousRefreshesCreateOneSnapshot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	scores := make([]int, 24)
	for i := range scores {
		scores[i] = 100_000 - i*1_000
	}
	expected := seedDiscoverHotSnapshotCandidates(t, db, "concurrent", scores)

	const requestCount = 3
	type result struct {
		feed   []string
		cursor *string
		err    error
	}
	ready := make(chan struct{}, requestCount)
	start := make(chan struct{})
	results := make(chan result, requestCount)
	for i := 0; i < requestCount; i++ {
		repository := NewDiscoverRepository(db, "snapshot-concurrency-secret")
		go func() {
			ready <- struct{}{}
			<-start
			feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
				Sort:  "hot",
				Limit: 4,
			})
			results <- result{feed: discoverURIs(feed), cursor: cursor, err: err}
		}()
	}
	for i := 0; i < requestCount; i++ {
		<-ready
	}
	close(start)

	for i := 0; i < requestCount; i++ {
		result := <-results
		require.NoErrorf(t, result.err, "concurrent anonymous request %d", i)
		assert.Equalf(t, expected[:4], result.feed, "concurrent anonymous request %d must receive the shared root page", i)
		require.NotNilf(t, result.cursor, "concurrent anonymous request %d has more snapshot candidates", i)
	}

	require.Equal(t, 1, countDiscoverHotSnapshots(t, db, ""),
		"database coordination must coalesce concurrent same-scope snapshot creation across repository instances")
}

func TestDiscoverRepo_HotConcurrentSameCursorReplayDeduplicatesCheckpoint(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "concurrent-cursor-replay", []int{400, 300, 200, 100})
	firstRepository := NewDiscoverRepository(db, "concurrent-cursor-replay-secret")
	firstPage, inputCursor, err := firstRepository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, expected[:1], discoverURIs(firstPage))
	require.NotNil(t, inputCursor)

	type result struct {
		feed   []string
		cursor *string
		err    error
	}
	const requestCount = 2
	ready := make(chan struct{}, requestCount)
	start := make(chan struct{})
	results := make(chan result, requestCount)
	for range requestCount {
		repository := NewDiscoverRepository(db, "concurrent-cursor-replay-secret")
		go func() {
			ready <- struct{}{}
			<-start
			feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
				Sort:   "hot",
				Limit:  1,
				Cursor: inputCursor,
			})
			results <- result{feed: discoverURIs(feed), cursor: cursor, err: err}
		}()
	}
	for range requestCount {
		<-ready
	}
	close(start)

	got := make([]result, 0, requestCount)
	for range requestCount {
		result := <-results
		require.NoError(t, result.err)
		assert.Equal(t, expected[1:2], result.feed)
		require.NotNil(t, result.cursor)
		got = append(got, result)
	}
	assert.Equal(t, *got[0].cursor, *got[1].cursor,
		"same-cursor replays must return the same immutable output checkpoint")

	var checkpoints int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM discover_hot_checkpoints
	`).Scan(&checkpoints))
	assert.Equal(t, 2, checkpoints,
		"the input checkpoint and one deduplicated output checkpoint must be the only stored states")
}

func TestDiscoverRepo_HotInitialSnapshotWorkDeadlineLeavesStateUnchanged(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stateBefore := discoverHotStateCounts(t, db)
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

	repository := NewDiscoverRepository(db, "snapshot-deadline-secret").(*postgresDiscoverRepo)
	repository.discoverHotWorkDeadline = 20 * time.Millisecond
	requestContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	feed, cursor, err := repository.GetDiscover(requestContext, discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 10,
	})

	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable)
	assert.Empty(t, feed)
	assert.Nil(t, cursor)
	assert.Equal(t, stateBefore, discoverHotStateCounts(t, db),
		"a timed-out initial request must not persist partial snapshot state")
}

func TestDiscoverRepo_HotAuthenticatedRefreshUsesIsolatedSnapshotMembership(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	initial := seedDiscoverHotSnapshotCandidates(t, db, "anonymous-scope", []int{300, 200, 100})
	anonymousRepository := NewDiscoverRepository(db, "snapshot-scope-secret")
	anonymousFirst, _ := readDiscoverHotRoot(t, anonymousRepository, "", 10)
	require.Equal(t, initial, anonymousFirst)

	newCandidate := seedDiscoverHotSnapshotCandidates(t, db, "viewer-scope", []int{1_000_000})[0]
	const viewerDID = "did:plc:hotsnapshotviewer"
	createTestUser(t, db, "hotsnapshotviewer.test", viewerDID)

	authenticated, _ := readDiscoverHotRoot(t, NewDiscoverRepository(db, "snapshot-scope-secret"), viewerDID, 10)
	anonymousAgain, _ := readDiscoverHotRoot(t, NewDiscoverRepository(db, "snapshot-scope-secret"), "", 10)

	assert.Equal(t, append([]string{newCandidate}, initial...), authenticated,
		"an authenticated viewer must build isolated membership rather than reuse the anonymous snapshot")
	assert.Equal(t, anonymousFirst, anonymousAgain, "the anonymous scope must continue reusing its original membership")
	assert.NotContains(t, anonymousAgain, newCandidate, "a post created after the anonymous snapshot belongs only to a new snapshot")
	assert.Equal(t, 1, countDiscoverHotSnapshots(t, db, ""))
	assert.Equal(t, 1, countDiscoverHotSnapshots(t, db, viewerDID))
}

func seedDiscoverHotSnapshotCandidates(t *testing.T, db *sql.DB, label string, scores []int) []string {
	t.Helper()

	ctx := context.Background()
	createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	result := make([]string, len(scores))
	for i, score := range scores {
		communityDID, err := fixtures.Community(ctx, db,
			fmt.Sprintf("hot-snapshot-%s-%02d", label, i),
			fmt.Sprintf("hot-snapshot-%s-%02d.test", label, i))
		require.NoError(t, err)
		result[i] = fixtures.Post(t, db, communityDID, "did:plc:hotsnapshotauthor",
			fmt.Sprintf("Hot snapshot candidate %d", i), score, createdAt)
	}
	return result
}

func readDiscoverHotRoot(t *testing.T, repository discover.Repository, viewerDID string, limit int) ([]string, *string) {
	t.Helper()

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		ViewerDID: viewerDID,
		Sort:      "hot",
		Limit:     limit,
	})
	require.NoError(t, err)
	return discoverURIs(feed), cursor
}

func countDiscoverHotSnapshots(t *testing.T, db *sql.DB, viewerDID string) int {
	t.Helper()

	var count int
	err := db.QueryRowContext(context.Background(), `
		SELECT count(*)
		FROM discover_hot_snapshots
		WHERE algorithm_version = $1 AND viewer_scope = $2
	`, discoverHotAlgorithmVersion, discoverHotViewerScope(viewerDID)).Scan(&count)
	require.NoError(t, err)
	return count
}
