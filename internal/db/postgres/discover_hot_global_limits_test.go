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

func TestDiscoverRepo_HotGlobalStoredCandidateLimitCountsUnpurgedState(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "global-candidates", []int{200, 100})
	ctx := context.Background()

	firstRepository := NewDiscoverRepository(db, "global-candidate-secret").(*postgresDiscoverRepo)
	firstRepository.discoverHotStoredCandidateLimit = 5
	feed, cursor, err := firstRepository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, expected, discoverURIs(feed))
	assert.Nil(t, cursor)

	secondRepository := NewDiscoverRepository(db, "global-candidate-secret").(*postgresDiscoverRepo)
	secondRepository.discoverHotStoredCandidateLimit = 5
	feed, cursor, err = secondRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		ViewerDID: "did:plc:globalcandidateviewerone",
		Sort:      "hot",
		Limit:     10,
	})
	require.NoError(t, err)
	assert.Equal(t, expected, discoverURIs(feed))
	assert.Nil(t, cursor)
	require.Equal(t, []int{2, 4, 0}, discoverHotStateCounts(t, db))

	_, err = db.ExecContext(ctx, `
		UPDATE discover_hot_snapshots
		SET expires_at = NOW() - INTERVAL '1 second'
		WHERE viewer_scope = $1
	`, discoverHotViewerScope(""))
	require.NoError(t, err)
	stateBeforeRefusal := discoverHotStateCounts(t, db)

	thirdRepository := NewDiscoverRepository(db, "global-candidate-secret").(*postgresDiscoverRepo)
	thirdRepository.discoverHotStoredCandidateLimit = 5
	feed, cursor, err = thirdRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		ViewerDID: "did:plc:globalcandidateviewertwo",
		Sort:      "hot",
		Limit:     10,
	})
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable,
		"expired candidates still occupy durable capacity until cleanup purges them")
	assert.Empty(t, feed)
	assert.Nil(t, cursor)
	assert.Equal(t, stateBeforeRefusal, discoverHotStateCounts(t, db),
		"capacity refusal must atomically leave no partial snapshot or candidates")

	removed, err := thirdRepository.CleanupExpiredDiscoverHotState(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(2), removed)
	require.Equal(t, []int{1, 2, 0}, discoverHotStateCounts(t, db))

	feed, cursor, err = thirdRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		ViewerDID: "did:plc:globalcandidateviewertwo",
		Sort:      "hot",
		Limit:     10,
	})
	require.NoError(t, err, "snapshot creation must recover after cleanup frees durable candidate capacity")
	assert.Equal(t, expected, discoverURIs(feed))
	assert.Nil(t, cursor)
	assert.Equal(t, []int{2, 4, 0}, discoverHotStateCounts(t, db))
}

func TestDiscoverRepo_HotGlobalCheckpointLimitRefusesWithoutAdvancing(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "global-checkpoints", []int{300, 200, 100})
	ctx := context.Background()
	firstRepository := NewDiscoverRepository(db, "global-checkpoint-secret").(*postgresDiscoverRepo)
	firstRepository.discoverHotStoredCheckpointLimit = 1

	firstPage, inputCursor, err := firstRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		Sort:  "hot",
		Limit: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, expected[:1], discoverURIs(firstPage))
	require.NotNil(t, inputCursor)
	require.Equal(t, []int{1, 3, 1}, discoverHotStateCounts(t, db))
	stateBeforeRefusal := discoverHotStateCounts(t, db)

	continuationRepository := NewDiscoverRepository(db, "global-checkpoint-secret").(*postgresDiscoverRepo)
	continuationRepository.discoverHotStoredCheckpointLimit = 1
	feed, outputCursor, err := continuationRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		Sort:   "hot",
		Limit:  1,
		Cursor: inputCursor,
	})
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable)
	assert.Empty(t, feed, "a refused continuation must not return a page whose state was not checkpointed")
	assert.Nil(t, outputCursor)
	assert.Equal(t, stateBeforeRefusal, discoverHotStateCounts(t, db),
		"checkpoint refusal must not persist or advance continuation state")

	continuationRepository.discoverHotStoredCheckpointLimit = 2
	feed, outputCursor, err = continuationRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		Sort:   "hot",
		Limit:  1,
		Cursor: inputCursor,
	})
	require.NoError(t, err, "replaying the unchanged input cursor must recover after raising the checkpoint limit")
	assert.Equal(t, expected[1:2], discoverURIs(feed))
	require.NotNil(t, outputCursor)
	assert.Equal(t, []int{1, 3, 2}, discoverHotStateCounts(t, db))
}

func TestDiscoverRepo_HotGlobalSnapshotBuildRateLimitAllowsReuseButRefusesNewScope(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	expected := seedDiscoverHotSnapshotCandidates(t, db, "global-build-rate", []int{200, 100})
	ctx := context.Background()

	firstRepository := newRateLimitedDiscoverHotRepository(db, "global-build-rate-secret", 1)
	firstFeed, firstCursor, err := firstRepository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, expected, discoverURIs(firstFeed))
	assert.Nil(t, firstCursor)

	reuseRepository := newRateLimitedDiscoverHotRepository(db, "global-build-rate-secret", 1)
	reusedFeed, reusedCursor, err := reuseRepository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.NoError(t, err, "same-scope refresh inside thirty seconds must reuse without consuming another build")
	assert.Equal(t, expected, discoverURIs(reusedFeed))
	assert.Nil(t, reusedCursor)
	require.Equal(t, []int{1, 2, 0}, discoverHotStateCounts(t, db))
	stateBeforeRefusal := discoverHotStateCounts(t, db)

	differentScopeRepository := newRateLimitedDiscoverHotRepository(db, "global-build-rate-secret", 1)
	feed, cursor, err := differentScopeRepository.GetDiscover(ctx, discover.GetDiscoverRequest{
		ViewerDID: "did:plc:globalbuildrateviewer",
		Sort:      "hot",
		Limit:     10,
	})
	assert.ErrorIs(t, err, discover.ErrDiscoverUnavailable)
	assert.Empty(t, feed)
	assert.Nil(t, cursor)
	assert.Equal(t, stateBeforeRefusal, discoverHotStateCounts(t, db),
		"build-rate refusal must not leave a partial snapshot or candidates")
}

func TestDiscoverRepo_HotFailedSnapshotBuildConsumesGlobalRateLimit(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	candidates := seedDiscoverHotSnapshotCandidates(t, db, "failed-build-rate", []int{200, 100})
	ctx := context.Background()
	firstRepository := newRateLimitedDiscoverHotRepository(db, "failed-build-rate-secret", 1)
	firstRepository.discoverHotCandidateLimit = 1

	feed, cursor, err := firstRepository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.ErrorIs(t, err, discover.ErrDiscoverUnavailable)
	assert.Empty(t, feed)
	assert.Nil(t, cursor)
	require.Equal(t, []int{0, 0, 0}, discoverHotStateCounts(t, db))

	_, err = db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, candidates[1])
	require.NoError(t, err)
	stateBeforeRetry := discoverHotStateCounts(t, db)
	retryRepository := newRateLimitedDiscoverHotRepository(db, "failed-build-rate-secret", 1)
	retryRepository.discoverHotCandidateLimit = 1

	feed, cursor, err = retryRepository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.ErrorIs(t, err, discover.ErrDiscoverUnavailable,
		"failed full snapshot work must consume the shared build budget")
	assert.Empty(t, feed)
	assert.Nil(t, cursor)
	assert.Equal(t, stateBeforeRetry, discoverHotStateCounts(t, db),
		"a build-rate refusal must not persist a snapshot")
}

func newRateLimitedDiscoverHotRepository(db *sql.DB, cursorSecret string, buildsPerMinute int) *postgresDiscoverRepo {
	repository := NewDiscoverRepository(db, cursorSecret).(*postgresDiscoverRepo)
	repository.discoverHotSnapshotBuildsPerMinuteLimit = buildsPerMinute
	return repository
}
