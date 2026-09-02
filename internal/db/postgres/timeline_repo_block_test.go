//go:build integration

package postgres

import (
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// User-block filtering in the subscribed timeline. See
// viewer_block_fixtures_test.go for why there is one of these per read path.
//
// The timeline is the one query with no unauthenticated shape — it is defined by
// whose subscriptions it reads — which is also why its block filter reuses $1
// for both the subscriber and the blocker. That reuse is the thing worth
// pinning: it is correct only because those two are always the same person.

// timelineURIs renders a timeline as the URIs it returned.
func timelineURIs(feed []*timeline.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

// subscribeToCommunity indexes a subscription, which is what puts a community's
// posts in a user's timeline at all.
func subscribeToCommunity(t *testing.T, db *sql.DB, userDID, communityDID string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
		VALUES ($1, $2, NOW())
	`, userDID, communityDID)
	require.NoError(t, err, "indexing the subscription")
}

func TestTimelineRepo_ViewerBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "tl")

	const communityDID = "did:plc:blktlcommunity"
	createTestCommunity(t, db, communityDID, "c-blktl.coves.social", cast.viewer)
	subscribeToCommunity(t, db, cast.viewer, communityDID)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedPost := seedFilterablePost(t, db, communityDID, cast.blocked, "tlblocked", base)
	thirdPartyPost := seedFilterablePost(t, db, communityDID, cast.thirdParty, "tlthird", base.Add(time.Hour))

	repo := NewTimelineRepository(db, "test-secret")
	read := func(t *testing.T) []string {
		t.Helper()
		feed, _, err := repo.GetTimeline(ctx, timeline.GetTimelineRequest{
			UserDID: cast.viewer,
			Sort:    "new",
			Limit:   50,
		})
		require.NoError(t, err)
		return timelineURIs(feed)
	}

	require.ElementsMatch(t, []string{blockedPost, thirdPartyPost}, read(t),
		"the fixture is not visible before any block exists")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	assert.ElementsMatch(t, []string{thirdPartyPost}, read(t),
		"the blocked author's post must leave the timeline and the third party's must stay")
}

func TestTimelineRepo_CommunityBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "tlcommunity")

	const (
		blockedCommunityDID = "did:plc:blktlcommunityx"
		visibleCommunityDID = "did:plc:blktlcommunityy"
	)
	createTestCommunity(t, db, blockedCommunityDID, "c-blktlcommunityx.coves.social", cast.viewer)
	createTestCommunity(t, db, visibleCommunityDID, "c-blktlcommunityy.coves.social", cast.viewer)
	for _, userDID := range []string{cast.viewer, cast.thirdParty} {
		subscribeToCommunity(t, db, userDID, blockedCommunityDID)
		subscribeToCommunity(t, db, userDID, visibleCommunityDID)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedCommunityPost := seedFilterablePost(t, db, blockedCommunityDID, cast.thirdParty, "tlcommunityblocked", base)
	viewerPostInBlockedCommunity := seedFilterablePost(t, db, blockedCommunityDID, cast.viewer, "tlcommunityviewer", base.Add(time.Hour))
	visibleCommunityPost := seedFilterablePost(t, db, visibleCommunityDID, cast.thirdParty, "tlcommunityvisible", base.Add(2*time.Hour))
	allPosts := []string{blockedCommunityPost, viewerPostInBlockedCommunity, visibleCommunityPost}

	repo := NewTimelineRepository(db, "test-secret")
	read := func(t *testing.T, userDID string) []string {
		t.Helper()
		feed, _, err := repo.GetTimeline(ctx, timeline.GetTimelineRequest{
			UserDID: userDID,
			Sort:    "new",
			Limit:   50,
		})
		require.NoError(t, err)
		return timelineURIs(feed)
	}

	require.ElementsMatch(t, allPosts, read(t, cast.viewer),
		"the viewer must see every fixture post in the subscribed timeline before blocking a community")

	insertCommunityBlock(t, db, cast.viewer, blockedCommunityDID)

	t.Run("the subscriber stops seeing the blocked community although still subscribed", func(t *testing.T) {
		// Blocking does not unsubscribe the viewer, so the subscription and block
		// coexist. The block must win when the subscribed timeline is read.
		assert.ElementsMatch(t, []string{visibleCommunityPost}, read(t, cast.viewer),
			"the community block must remove the muted community even while its subscription remains")
	})

	t.Run("the viewer's own post in the blocked community is hidden too", func(t *testing.T) {
		assert.NotContains(t, read(t, cast.viewer), viewerPostInBlockedCommunity,
			"the viewer's own post must not bypass a mute of its community")
	})

	t.Run("another subscriber is unaffected", func(t *testing.T) {
		assert.ElementsMatch(t, allPosts, read(t, cast.thirdParty),
			"community blocks are viewer-scoped and must not change another subscriber's timeline")
	})

	t.Run("deleting the block restores visibility", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
			DELETE FROM community_blocks
			WHERE user_did = $1 AND community_did = $2
		`, cast.viewer, blockedCommunityDID)
		require.NoError(t, err, "deleting the indexed community block")
		assert.ElementsMatch(t, allPosts, read(t, cast.viewer),
			"deleting a community block must restore timeline visibility from live database state")
	})
}
