//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hot feeds rank only the newest N visible posts, ordered created_at DESC,
// uri DESC. Running past the bound is not an error: the feed simply ends.
//
// Every expected sequence below is written by hand. The live hot rank is
// (ln(score+1)+1) / (ageHours+2)^1.5, so the fixtures use these rough ranks,
// far enough apart that database time cannot reorder them during a test:
//
//	age      score          rank
//	10m      0              0.31
//	30m      0              0.25
//	1h       0              0.19
//	1h       10             0.65
//	2h       100            0.70
//	2h       1,000          0.99
//	2h       100,000        1.56
//	3h       10,000         0.91
//	3h10m    1,000,000,000  1.85
//	5h       0              0.05
//
// Rank order is deliberately the reverse of recency, and the post just past the
// cutoff outranks everything, so a bound taken over rank instead of recency
// selects the wrong posts.

const hotBoundExcludedScore = 1_000_000_000

func seedHotBoundPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey string, createdAt time.Time, score int) string {
	t.Helper()

	uri := seedFilterablePost(t, db, communityDID, authorDID, rkey, createdAt)
	_, err := db.ExecContext(context.Background(), `UPDATE posts SET score = $1 WHERE uri = $2`, score, uri)
	require.NoErrorf(t, err, "scoring post %s", rkey)
	return uri
}

func seedHotBoundPendingAdmission(t *testing.T, db *sql.DB, communityDID, uri string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_post_admissions (community_did, post_uri, status)
		VALUES ($1, $2, 'pending')
	`, communityDID, uri)
	require.NoError(t, err, "holding the post pending")
}

// seedHotBoundDeletedPost seeds a soft-deleted post. Its rank is irrelevant: it
// is never served, and only its recency can cost a visible post a bound slot.
func seedHotBoundDeletedPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey string, createdAt time.Time) {
	t.Helper()

	uri := seedHotBoundPost(t, db, communityDID, authorDID, rkey, createdAt, 0)
	_, err := db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, uri)
	require.NoErrorf(t, err, "deleting post %s", rkey)
}

// seedHotBoundDiscoverPost gives the post its own community, so Discover's
// per-community diversity has no effect on the served order.
func seedHotBoundDiscoverPost(t *testing.T, db *sql.DB, ownerDID, authorDID, rkey string, createdAt time.Time, score int) string {
	t.Helper()

	communityDID := "did:plc:hotbound" + rkey
	createTestCommunity(t, db, communityDID, "c-hotbound"+rkey+".coves.social", ownerDID)
	return seedHotBoundPost(t, db, communityDID, authorDID, rkey, createdAt, score)
}

func TestDiscoverRepo_HotCandidateBoundServesNewestPosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "bounddisc")
	now := time.Now().Truncate(time.Second)

	newest := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "newest", now.Add(-time.Hour), 0)
	middle := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "middle", now.Add(-2*time.Hour), 100)
	oldestKept := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "oldestkept", now.Add(-3*time.Hour), 10_000)
	seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "excluded", now.Add(-190*time.Minute), hotBoundExcludedScore)

	repository := NewDiscoverRepository(db, "hot-bound-secret").(*postgresDiscoverRepo)
	repository.discoverHotCandidateLimit = 3

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.NoError(t, err, "running past the candidate bound must not be an error")
	assert.Equal(t, []string{oldestKept, middle, newest}, discoverURIs(feed),
		"the newest three are served in rank order, and the older high-score post is not among them")
	assert.Nil(t, cursor, "the feed ends at the bound")
	assert.Equal(t, []int{1, 3, 0}, discoverHotStateCounts(t, db),
		"one snapshot holds exactly the bounded candidates, with no continuation checkpoint")
}

func TestDiscoverRepo_HotCandidateBoundTieAtCutoffResolvesByURIDescending(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "bounddisctie")
	now := time.Now().Truncate(time.Second)
	tiedAt := now.Add(-2 * time.Hour)

	newest := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "newest", now.Add(-time.Hour), 0)
	// The lower URI has the higher score, so only the URI tiebreak drops it.
	seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "tiea", tiedAt, 100_000)
	tiedHigherURI := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "tieb", tiedAt, 1_000)

	repository := NewDiscoverRepository(db, "hot-bound-secret").(*postgresDiscoverRepo)
	repository.discoverHotCandidateLimit = 2

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{Sort: "hot", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{tiedHigherURI, newest}, discoverURIs(feed))
	assert.Nil(t, cursor)
	assert.Equal(t, []int{1, 2, 0}, discoverHotStateCounts(t, db))
}

func TestDiscoverRepo_HotCandidateBoundCountsOnlyVisiblePosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "bounddiscvisible")
	now := time.Now().Truncate(time.Second)

	removed := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "removed", now.Add(-10*time.Minute), 0)
	insertDiscoverHotRemoval(t, db, removed)
	seedHotBoundDiscoverPost(t, db, cast.viewer, cast.blocked, "blockedauthor", now.Add(-20*time.Minute), 0)
	insertUserBlock(t, db, cast.viewer, cast.blocked)
	seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "blockedcommunity", now.Add(-30*time.Minute), 0)
	insertCommunityBlock(t, db, cast.viewer, "did:plc:hotboundblockedcommunity")

	newerKept := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "newerkept", now.Add(-time.Hour), 0)
	olderKept := seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "olderkept", now.Add(-2*time.Hour), 1_000)
	seedHotBoundDiscoverPost(t, db, cast.viewer, cast.thirdParty, "excluded", now.Add(-3*time.Hour), hotBoundExcludedScore)

	repository := NewDiscoverRepository(db, "hot-bound-secret").(*postgresDiscoverRepo)
	repository.discoverHotCandidateLimit = 2

	feed, cursor, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
		ViewerDID: cast.viewer,
		Sort:      "hot",
		Limit:     10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{olderKept, newerKept}, discoverURIs(feed),
		"removed and blocked posts must leave both bound slots for visible posts")
	assert.Nil(t, cursor)
}

// hotBoundRead reads one page as the feed's viewer.
type hotBoundRead func(t *testing.T, sort string, limit int, cursor *string) ([]string, *string)

// hotBoundFeed is one bounded feed, reached through its repository. Each shape
// places communityDID in viewerDID's view of that feed and applies candidateLimit.
type hotBoundFeed struct {
	name string
	open func(t *testing.T, db *sql.DB, viewerDID, communityDID string, candidateLimit int) hotBoundRead
}

func hotBoundFeeds() []hotBoundFeed {
	return []hotBoundFeed{
		{name: "timeline", open: openHotBoundTimeline},
		{name: "community", open: openHotBoundCommunityFeed},
	}
}

func openHotBoundTimeline(t *testing.T, db *sql.DB, viewerDID, communityDID string, candidateLimit int) hotBoundRead {
	t.Helper()

	subscribeToCommunity(t, db, viewerDID, communityDID)
	repository := NewTimelineRepository(db, "hot-bound-secret").(*postgresTimelineRepo)
	repository.hotCandidateLimit = candidateLimit
	return func(t *testing.T, sort string, limit int, cursor *string) ([]string, *string) {
		t.Helper()
		feed, nextCursor, err := repository.GetTimeline(context.Background(), timeline.GetTimelineRequest{
			UserDID: viewerDID,
			Sort:    sort,
			Limit:   limit,
			Cursor:  cursor,
		})
		require.NoError(t, err)
		return timelineURIs(feed), nextCursor
	}
}

func openHotBoundCommunityFeed(t *testing.T, db *sql.DB, viewerDID, communityDID string, candidateLimit int) hotBoundRead {
	t.Helper()

	repository := NewCommunityFeedRepository(db, "hot-bound-secret").(*postgresFeedRepo)
	repository.hotCandidateLimit = candidateLimit
	return func(t *testing.T, sort string, limit int, cursor *string) ([]string, *string) {
		t.Helper()
		feed, nextCursor, err := repository.GetCommunityFeed(context.Background(), communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: viewerDID,
			Sort:      sort,
			Limit:     limit,
			Cursor:    cursor,
		})
		require.NoError(t, err)
		return communityFeedURIs(feed), nextCursor
	}
}

func TestHotCandidateBound_TieAtCutoffResolvesByURIDescending(t *testing.T) {
	t.Parallel()

	for _, shape := range hotBoundFeeds() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			cast := seedBlockFilterCast(t, db, "boundtie")
			const communityDID = "did:plc:hotboundtie"
			createTestCommunity(t, db, communityDID, "c-hotboundtie.coves.social", cast.viewer)
			now := time.Now().Truncate(time.Second)
			tiedAt := now.Add(-2 * time.Hour)

			newest := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "newest", now.Add(-time.Hour), 0)
			// The lower URI has the higher score, so only the URI tiebreak drops it.
			seedHotBoundPost(t, db, communityDID, cast.thirdParty, "tie-a", tiedAt, 100_000)
			tiedHigherURI := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "tie-b", tiedAt, 1_000)

			read := shape.open(t, db, cast.viewer, communityDID, 2)
			uris, cursor := read(t, "hot", 10, nil)
			assert.Equal(t, []string{tiedHigherURI, newest}, uris)
			assert.Nil(t, cursor)
		})
	}
}

func TestHotCandidateBound_PaginationStaysInsideTheBound(t *testing.T) {
	t.Parallel()

	for _, shape := range hotBoundFeeds() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			cast := seedBlockFilterCast(t, db, "boundpage")
			const communityDID = "did:plc:hotboundpage"
			createTestCommunity(t, db, communityDID, "c-hotboundpage.coves.social", cast.viewer)
			now := time.Now().Truncate(time.Second)

			newest := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "newest", now.Add(-time.Hour), 0)
			middle := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "middle", now.Add(-2*time.Hour), 100)
			oldestKept := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "oldest-kept", now.Add(-3*time.Hour), 10_000)
			seedHotBoundPost(t, db, communityDID, cast.thirdParty, "excluded-high", now.Add(-190*time.Minute), hotBoundExcludedScore)
			// This one ranks below every kept post, so a bound recomputed after the
			// cursor filter would pull it onto the second page.
			seedHotBoundPost(t, db, communityDID, cast.thirdParty, "excluded-low", now.Add(-5*time.Hour), 0)

			read := shape.open(t, db, cast.viewer, communityDID, 3)

			firstPage, cursor := read(t, "hot", 2, nil)
			assert.Equal(t, []string{oldestKept, middle}, firstPage)
			require.NotNil(t, cursor, "one bounded post remains after the first page")

			secondPage, cursor := read(t, "hot", 2, cursor)
			assert.Equal(t, []string{newest}, secondPage)
			assert.Nil(t, cursor, "the feed ends at the bound")
		})
	}
}

// The bound belongs to Hot alone.
func TestHotCandidateBound_DoesNotApplyToTopOrNew(t *testing.T) {
	t.Parallel()

	for _, shape := range hotBoundFeeds() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			cast := seedBlockFilterCast(t, db, "boundsorts")
			const communityDID = "did:plc:hotboundsorts"
			createTestCommunity(t, db, communityDID, "c-hotboundsorts.coves.social", cast.viewer)
			now := time.Now().Truncate(time.Second)

			newest := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "newest", now.Add(-time.Hour), 1)
			middle := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "middle", now.Add(-2*time.Hour), 100)
			oldest := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "oldest", now.Add(-3*time.Hour), 10_000)

			read := shape.open(t, db, cast.viewer, communityDID, 1)

			uris, _ := read(t, "new", 10, nil)
			assert.Equal(t, []string{newest, middle, oldest}, uris)

			uris, _ = read(t, "top", 10, nil)
			assert.Equal(t, []string{oldest, middle, newest}, uris)
		})
	}
}

func TestTimelineRepo_HotCandidateBoundCountsOnlyTheViewersVisiblePosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "boundtimeline")
	const (
		subscribedCommunity   = "did:plc:hotboundtimelinesubscribed"
		unsubscribedCommunity = "did:plc:hotboundtimelineunsubscribed"
		blockedCommunity      = "did:plc:hotboundtimelineblocked"
	)
	createTestCommunity(t, db, subscribedCommunity, "c-hotboundtimelinesubscribed.coves.social", cast.viewer)
	createTestCommunity(t, db, unsubscribedCommunity, "c-hotboundtimelineunsubscribed.coves.social", cast.viewer)
	createTestCommunity(t, db, blockedCommunity, "c-hotboundtimelineblocked.coves.social", cast.viewer)
	subscribeToCommunity(t, db, cast.thirdParty, unsubscribedCommunity)
	subscribeToCommunity(t, db, cast.viewer, blockedCommunity)
	insertCommunityBlock(t, db, cast.viewer, blockedCommunity)
	insertUserBlock(t, db, cast.viewer, cast.blocked)
	now := time.Now().Truncate(time.Second)

	seedHotBoundDeletedPost(t, db, subscribedCommunity, cast.thirdParty, "deleted", now.Add(-5*time.Minute))
	ownPending := seedHotBoundPost(t, db, subscribedCommunity, cast.viewer, "own-pending", now.Add(-10*time.Minute), 0)
	seedHotBoundPendingAdmission(t, db, subscribedCommunity, ownPending)
	seedHotBoundPost(t, db, unsubscribedCommunity, cast.thirdParty, "unsubscribed", now.Add(-20*time.Minute), 0)
	seedHotBoundPost(t, db, subscribedCommunity, cast.blocked, "blocked-author", now.Add(-30*time.Minute), 0)
	seedHotBoundPost(t, db, blockedCommunity, cast.thirdParty, "blocked-community", now.Add(-40*time.Minute), 0)
	removed := seedHotBoundPost(t, db, subscribedCommunity, cast.thirdParty, "removed", now.Add(-50*time.Minute), 0)
	insertDiscoverHotRemoval(t, db, removed)

	newerKept := seedHotBoundPost(t, db, subscribedCommunity, cast.thirdParty, "newer-kept", now.Add(-time.Hour), 10)
	olderKept := seedHotBoundPost(t, db, subscribedCommunity, cast.thirdParty, "older-kept", now.Add(-2*time.Hour), 1_000)
	seedHotBoundPost(t, db, subscribedCommunity, cast.thirdParty, "excluded", now.Add(-3*time.Hour), hotBoundExcludedScore)

	read := openHotBoundTimeline(t, db, cast.viewer, subscribedCommunity, 3)
	uris, cursor := read(t, "hot", 10, nil)
	assert.Equal(t, []string{olderKept, newerKept, ownPending}, uris,
		"the viewer's own pending post takes a slot; filtered posts do not")
	assert.Nil(t, cursor)
}

func TestCommunityFeedRepo_HotCandidateBoundCountsOnlyTheViewersVisiblePosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "boundcommunity")
	const (
		communityDID      = "did:plc:hotboundcommunity"
		otherCommunityDID = "did:plc:hotboundcommunityother"
	)
	createTestCommunity(t, db, communityDID, "c-hotboundcommunity.coves.social", cast.viewer)
	createTestCommunity(t, db, otherCommunityDID, "c-hotboundcommunityother.coves.social", cast.viewer)
	// A community block mutes aggregate feeds, not the community's own feed.
	insertCommunityBlock(t, db, cast.viewer, communityDID)
	insertUserBlock(t, db, cast.viewer, cast.blocked)
	now := time.Now().Truncate(time.Second)

	seedHotBoundDeletedPost(t, db, communityDID, cast.thirdParty, "deleted", now.Add(-5*time.Minute))
	ownPending := seedHotBoundPost(t, db, communityDID, cast.viewer, "own-pending", now.Add(-10*time.Minute), 0)
	seedHotBoundPendingAdmission(t, db, communityDID, ownPending)
	seedHotBoundPost(t, db, otherCommunityDID, cast.thirdParty, "other-community", now.Add(-20*time.Minute), 0)
	blockedAuthor := seedHotBoundPost(t, db, communityDID, cast.blocked, "blocked-author", now.Add(-30*time.Minute), 0)
	removed := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "removed", now.Add(-50*time.Minute), 0)
	insertDiscoverHotRemoval(t, db, removed)

	newerKept := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "newer-kept", now.Add(-time.Hour), 10)
	olderKept := seedHotBoundPost(t, db, communityDID, cast.thirdParty, "older-kept", now.Add(-2*time.Hour), 1_000)
	seedHotBoundPost(t, db, communityDID, cast.thirdParty, "excluded", now.Add(-3*time.Hour), hotBoundExcludedScore)

	t.Run("signed-in viewer", func(t *testing.T) {
		read := openHotBoundCommunityFeed(t, db, cast.viewer, communityDID, 3)
		uris, cursor := read(t, "hot", 10, nil)
		assert.Equal(t, []string{olderKept, newerKept, ownPending}, uris,
			"the viewer's own pending post takes a slot; filtered posts do not")
		assert.Nil(t, cursor)
	})

	t.Run("anonymous viewer", func(t *testing.T) {
		read := openHotBoundCommunityFeed(t, db, "", communityDID, 3)
		uris, cursor := read(t, "hot", 10, nil)
		assert.Equal(t, []string{olderKept, newerKept, blockedAuthor}, uris,
			"a pending post is hidden from the public and takes no public slot")
		assert.Nil(t, cursor)
	})
}
