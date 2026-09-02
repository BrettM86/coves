//go:build integration

package postgres

import (
	"Coves/internal/core/discover"
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// User-block filtering in the discover feed. See viewer_block_fixtures_test.go
// for why there is one of these per read path.

// discoverURIs renders a discover feed as the URIs it returned.
func discoverURIs(feed []*discover.FeedViewPost) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if item.Post != nil {
			uris = append(uris, item.Post.URI)
		}
	}
	return uris
}

func TestDiscoverRepo_ViewerBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "disc")

	const communityDID = "did:plc:blkdisccommunity"
	createTestCommunity(t, db, communityDID, "c-blkdisc.coves.social", cast.viewer)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedPost := seedFilterablePost(t, db, communityDID, cast.blocked, "discblocked", base)
	thirdPartyPost := seedFilterablePost(t, db, communityDID, cast.thirdParty, "discthird", base.Add(time.Hour))
	viewerPost := seedFilterablePost(t, db, communityDID, cast.viewer, "discviewer", base.Add(2*time.Hour))

	repo := NewDiscoverRepository(db, "test-secret")
	read := func(t *testing.T, viewerDID string) []string {
		t.Helper()
		feed, _, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: viewerDID,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return discoverURIs(feed)
	}

	// Discover spans every community, so the exact-set assertions below are only
	// meaningful because each test runs against its own database clone.
	require.ElementsMatch(t, []string{blockedPost, thirdPartyPost, viewerPost}, read(t, ""),
		"the fixture is not visible before any block exists")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	t.Run("the blocker stops seeing the blocked author, and only them", func(t *testing.T) {
		assert.ElementsMatch(t, []string{thirdPartyPost, viewerPost}, read(t, cast.viewer))
	})

	t.Run("the blocked user still sees the blocker", func(t *testing.T) {
		assert.Contains(t, read(t, cast.blocked), viewerPost,
			"blocks are one-directional: the blocked user's own discover feed is unchanged")
	})

	t.Run("an unauthenticated read is unaffected", func(t *testing.T) {
		assert.ElementsMatch(t, []string{blockedPost, thirdPartyPost, viewerPost}, read(t, ""))
	})
}

func TestDiscoverRepo_CommunityBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "disccommunity")

	const (
		blockedCommunityDID = "did:plc:blkdisccommunityx"
		visibleCommunityDID = "did:plc:blkdisccommunityy"
	)
	createTestCommunity(t, db, blockedCommunityDID, "c-blkdisccommunityx.coves.social", cast.viewer)
	createTestCommunity(t, db, visibleCommunityDID, "c-blkdisccommunityy.coves.social", cast.viewer)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedCommunityPost := seedFilterablePost(t, db, blockedCommunityDID, cast.thirdParty, "disccommunityblocked", base)
	viewerPostInBlockedCommunity := seedFilterablePost(t, db, blockedCommunityDID, cast.viewer, "disccommunityviewer", base.Add(time.Hour))
	visibleCommunityPost := seedFilterablePost(t, db, visibleCommunityDID, cast.thirdParty, "disccommunityvisible", base.Add(2*time.Hour))
	allPosts := []string{blockedCommunityPost, viewerPostInBlockedCommunity, visibleCommunityPost}

	repo := NewDiscoverRepository(db, "test-secret")
	read := func(t *testing.T, viewerDID string) []string {
		t.Helper()
		feed, _, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: viewerDID,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return discoverURIs(feed)
	}

	require.ElementsMatch(t, allPosts, read(t, cast.viewer),
		"the viewer must see every fixture post before blocking a community")

	insertCommunityBlock(t, db, cast.viewer, blockedCommunityDID)

	t.Run("the blocker stops seeing the blocked community, and only it", func(t *testing.T) {
		assert.ElementsMatch(t, []string{visibleCommunityPost}, read(t, cast.viewer),
			"the community block must remove only posts whose community_did identifies the muted community")
	})

	t.Run("the viewer's own post in the blocked community is hidden too", func(t *testing.T) {
		// A community block mutes the place and is keyed on p.community_did, not
		// the author. Unlike a user block, it must hide the viewer's own posts.
		assert.NotContains(t, read(t, cast.viewer), viewerPostInBlockedCommunity,
			"the viewer's own post must not bypass a mute of its community")
	})

	t.Run("another user and the public are unaffected", func(t *testing.T) {
		assert.ElementsMatch(t, allPosts, read(t, cast.thirdParty),
			"community blocks are viewer-scoped and must not change another user's discover feed")
		assert.ElementsMatch(t, allPosts, read(t, ""),
			"community blocks are viewer preferences and must not change the public discover feed")
	})

	t.Run("deleting the block restores visibility", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
			DELETE FROM community_blocks
			WHERE user_did = $1 AND community_did = $2
		`, cast.viewer, blockedCommunityDID)
		require.NoError(t, err, "deleting the indexed community block")
		assert.ElementsMatch(t, allPosts, read(t, cast.viewer),
			"deleting a community block must restore visibility from live database state")
	})
}

// TestDiscoverRepo_BlockedPostsDoNotConsumeAPageSlot closes the one gap the
// four author-block suites left: they all read with Limit 50 against three
// posts, so none of them says anything about what a block does to PAGINATION.
//
// The failure mode this rules out is the common shape of the bug. If the filter
// ran in Go over rows the database had already limited, a viewer who blocks
// somebody would get short pages — ten requested, eight delivered — and, worse,
// a page could come back EMPTY while more matching posts existed further down,
// which a client reads as the end of the feed. The blocked author would then be
// able to truncate the feed of anybody who blocked them by posting.
//
// It does not, because the NOT EXISTS is part of the WHERE clause and therefore
// applies before LIMIT. That is worth an assertion rather than a reading of the
// SQL: the parameter index of the viewer filter is built by string formatting
// differently in each of the four author-block queries, and moving the filter out of the
// WHERE clause during a refactor is exactly the kind of change that keeps every
// existing test green.
func TestDiscoverRepo_BlockedPostsDoNotConsumeAPageSlot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "page")

	const communityDID = "did:plc:blkpagecommunity"
	createTestCommunity(t, db, communityDID, "c-blkpage.coves.social", cast.viewer)

	// Six posts, alternating authors, newest last. Sorted "new" the blocked
	// author owns every other position, so any page of size two that did not
	// filter before limiting would come back half empty — and the first page
	// would be a single post rather than two.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wanted []string
	for i := 0; i < 6; i++ {
		author := cast.thirdParty
		if i%2 == 0 {
			author = cast.blocked
		}
		uri := seedFilterablePost(t, db, communityDID, author,
			"page"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
		if author != cast.blocked {
			wanted = append(wanted, uri)
		}
	}
	require.Len(t, wanted, 3, "three of the six posts survive the block")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	repo := NewDiscoverRepository(db, "test-secret")
	page := func(t *testing.T, cursor *string) ([]string, *string) {
		t.Helper()
		feed, next, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: cast.viewer, Sort: "new", Limit: 2, Cursor: cursor,
		})
		require.NoError(t, err)
		return discoverURIs(feed), next
	}

	first, cursor := page(t, nil)
	assert.Len(t, first, 2,
		"a page of two came back short. The block filter is being applied after the LIMIT, so the "+
			"blocked author's posts are eating slots that visible posts should occupy")
	require.NotNil(t, cursor, "two visible posts remain, so there must be a next page")

	second, lastCursor := page(t, cursor)
	assert.Len(t, second, 1, "one visible post is left")

	// Every visible post appeared exactly once across the pages, and no blocked
	// post appeared at all.
	seen := append(append([]string{}, first...), second...)
	assert.ElementsMatch(t, wanted, seen,
		"pagination over a filtered feed must visit each surviving post exactly once")

	if lastCursor != nil {
		last, _ := page(t, lastCursor)
		assert.Empty(t, last, "there is nothing past the third visible post")
	}
}

// TestDiscoverRepo_BlockedCommunityPostsDoNotConsumeAPageSlot applies the same
// pagination contract to community mutes. The community filter must run in SQL
// before LIMIT; filtering a limited page afterward would produce short or empty
// pages while visible posts still exist further down the feed.
func TestDiscoverRepo_BlockedCommunityPostsDoNotConsumeAPageSlot(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "communitypage")

	const (
		blockedCommunityDID = "did:plc:blkcommunitypagex"
		visibleCommunityDID = "did:plc:blkcommunitypagey"
	)
	createTestCommunity(t, db, blockedCommunityDID, "c-blkcommunitypagex.coves.social", cast.viewer)
	createTestCommunity(t, db, visibleCommunityDID, "c-blkcommunitypagey.coves.social", cast.viewer)

	// Six posts by the same author, alternating communities, newest last. Scores
	// rise with recency, so new, hot and top all have deterministic ordering and
	// the blocked community occupies every other position in each.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wanted []string
	for i := 0; i < 6; i++ {
		communityDID := visibleCommunityDID
		if i%2 == 0 {
			communityDID = blockedCommunityDID
		}
		uri := seedFilterablePost(t, db, communityDID, cast.thirdParty,
			"communitypage"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
		_, err := db.ExecContext(ctx, `
			UPDATE posts
			SET upvote_count = $2, score = $2
			WHERE uri = $1
		`, uri, i+1)
		require.NoErrorf(t, err, "ranking post communitypage%c", rune('a'+i))
		if communityDID != blockedCommunityDID {
			wanted = append(wanted, uri)
		}
	}
	require.Len(t, wanted, 3, "three of the six posts survive the community block")

	insertCommunityBlock(t, db, cast.viewer, blockedCommunityDID)

	repo := NewDiscoverRepository(db, "test-secret")
	page := func(t *testing.T, sort, timeframe string, cursor *string) ([]string, *string) {
		t.Helper()
		feed, next, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: cast.viewer, Sort: sort, Timeframe: timeframe, Limit: 2, Cursor: cursor,
		})
		require.NoError(t, err)
		return discoverURIs(feed), next
	}

	for _, tc := range []struct {
		sort      string
		timeframe string
	}{
		{sort: "new"},
		{sort: "hot", timeframe: "all"},
		{sort: "top", timeframe: "all"},
	} {
		t.Run(tc.sort, func(t *testing.T) {
			if tc.sort == "new" {
				first, cursor := page(t, tc.sort, tc.timeframe, nil)
				assert.Len(t, first, 2,
					"a page of two came back short. The community block filter is being applied after the LIMIT, so the "+
						"blocked community's posts are eating slots that visible posts should occupy")
				require.NotNil(t, cursor, "one visible post remains, so there must be a next page")

				second, lastCursor := page(t, tc.sort, tc.timeframe, cursor)
				assert.Len(t, second, 1,
					"the second page contains more than the one remaining visible-community post because blocked-community posts are still returned")

				// Every visible-community post appeared exactly once across the pages,
				// and no blocked-community post appeared at all.
				seen := append(append([]string{}, first...), second...)
				assert.ElementsMatch(t, wanted, seen,
					"pagination over a community-filtered feed must visit each surviving post exactly once; blocked-community posts are still returned")

				if lastCursor != nil {
					last, _ := page(t, tc.sort, tc.timeframe, lastCursor)
					assert.Empty(t, last,
						"posts from the blocked community are still returned after all three visible posts have been paged")
				}
				return
			}

			// Hot and top continuation cursors each bind three values, moving the
			// viewer parameter from its first-page position. Walking every page pins
			// that the community filter keeps using the shifted viewer placeholder.
			var seen []string
			var cursor *string
			for pageNumber := 0; ; pageNumber++ {
				require.Less(t, pageNumber, 4, "%s cursor pagination did not terminate", tc.sort)
				uris, next := page(t, tc.sort, tc.timeframe, cursor)
				require.NotEmpty(t, uris,
					"%s returned an empty page while a cursor said more visible posts existed", tc.sort)
				seen = append(seen, uris...)
				if next == nil {
					break
				}
				cursor = next
			}

			assert.Len(t, seen, len(wanted), "%s pagination returned a visible post more than once", tc.sort)
			assert.ElementsMatch(t, wanted, seen,
				"%s pagination must visit every visible-community post once and no blocked-community posts", tc.sort)
		})
	}
}
