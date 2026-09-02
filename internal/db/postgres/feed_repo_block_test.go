//go:build integration

package postgres

import (
	"Coves/internal/core/communityFeeds"
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// User-block filtering in the community feed. See viewer_block_fixtures_test.go
// for why there is one of these per read path.

// communityFeedURIs renders a community feed as the URIs it returned, which is
// both the whole assertion and the only readable failure message.
func communityFeedURIs(feed []*communityFeeds.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

func TestCommunityFeedRepo_ViewerBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "feed")

	const communityDID = "did:plc:blkfeedcommunity"
	createTestCommunity(t, db, communityDID, "c-blkfeed.coves.social", cast.viewer)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedPost := seedFilterablePost(t, db, communityDID, cast.blocked, "feedblocked", base)
	thirdPartyPost := seedFilterablePost(t, db, communityDID, cast.thirdParty, "feedthird", base.Add(time.Hour))
	viewerPost := seedFilterablePost(t, db, communityDID, cast.viewer, "feedviewer", base.Add(2*time.Hour))

	repo := NewCommunityFeedRepository(db, "test-secret")
	read := func(t *testing.T, viewerDID string) []string {
		t.Helper()
		feed, _, err := repo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: viewerDID,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return communityFeedURIs(feed)
	}

	// The baseline runs before the block so the filtering assertions below cannot
	// pass vacuously: without it, "the blocked author is absent" is also what a
	// feed that returns nothing at all looks like.
	require.ElementsMatch(t, []string{blockedPost, thirdPartyPost, viewerPost}, read(t, ""),
		"the fixture is not visible before any block exists")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	t.Run("the blocker stops seeing the blocked author, and only them", func(t *testing.T) {
		assert.ElementsMatch(t, []string{thirdPartyPost, viewerPost}, read(t, cast.viewer))
	})

	t.Run("the blocked user still sees the blocker", func(t *testing.T) {
		// Blocks are ONE-DIRECTIONAL, and this is the assertion that says so.
		// Filtering on (blocked_did = viewer) as well as (blocker_did = viewer)
		// would silently turn every block into a mute for both parties — a
		// change no other test in this file would notice, because the blocker's
		// own view is identical either way.
		assert.Contains(t, read(t, cast.blocked), viewerPost,
			"the blocked user must still see the blocker's posts")
	})

	t.Run("an unauthenticated read is unaffected", func(t *testing.T) {
		// A block is a viewer preference, not a takedown: with no viewer there
		// is nobody whose preference could apply, and the blocked author's posts
		// are still public.
		assert.ElementsMatch(t, []string{blockedPost, thirdPartyPost, viewerPost}, read(t, ""))
	})
}

// TestCommunityFeedRepo_CommunityBlockIsNotApplied pins the product decision
// that community blocks mute aggregate surfaces: Discover, the subscribed
// timeline, and search when it follows. Under Reddit/Lemmy semantics, opening
// the community itself is an explicit request that is honored; post permalinks
// and comment threads stay reachable for the same reason.
func TestCommunityFeedRepo_CommunityBlockIsNotApplied(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "feedcommunity")

	const communityDID = "did:plc:blkfeedcommunitypin"
	createTestCommunity(t, db, communityDID, "c-blkfeedcommunitypin.coves.social", cast.viewer)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thirdPartyPost := seedFilterablePost(t, db, communityDID, cast.thirdParty, "feedcommunitythird", base)
	viewerPost := seedFilterablePost(t, db, communityDID, cast.viewer, "feedcommunityviewer", base.Add(time.Hour))

	repo := NewCommunityFeedRepository(db, "test-secret")
	read := func(t *testing.T) []string {
		t.Helper()
		feed, _, err := repo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: cast.viewer,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return communityFeedURIs(feed)
	}

	insertCommunityBlock(t, db, cast.viewer, communityDID)

	assert.ElementsMatch(t, []string{thirdPartyPost, viewerPost}, read(t),
		"the aggregate-feed community-block clause has leaked into the community's own feed")

	t.Run("author blocks still apply independently", func(t *testing.T) {
		insertUserBlock(t, db, cast.viewer, cast.thirdParty)
		assert.ElementsMatch(t, []string{viewerPost}, read(t),
			"an author block must still filter the explicit community feed independently of the community mute")
	})
}
