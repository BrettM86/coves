//go:build integration

package postgres

import (
	"Coves/internal/core/comments"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommunityBlock_AggregateFeedsMuteTheCommunity(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "commute")
	viewerDID := cast.viewer
	otherUserDID := cast.thirdParty

	const (
		blockedCommunityDID = "did:plc:blkcommutex"
		visibleCommunityDID = "did:plc:blkcommutey"
	)
	createTestCommunity(t, db, blockedCommunityDID, "c-blkcommutex.coves.social", viewerDID)
	createTestCommunity(t, db, visibleCommunityDID, "c-blkcommutey.coves.social", viewerDID)
	for _, userDID := range []string{viewerDID, otherUserDID} {
		subscribeToCommunity(t, db, userDID, blockedCommunityDID)
		subscribeToCommunity(t, db, userDID, visibleCommunityDID)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedCommunityOtherPost := seedFilterablePost(t, db, blockedCommunityDID, otherUserDID, "commutexother", base)
	blockedCommunityViewerPost := seedFilterablePost(t, db, blockedCommunityDID, viewerDID, "commutexviewer", base.Add(time.Hour))
	visibleCommunityOtherPost := seedFilterablePost(t, db, visibleCommunityDID, otherUserDID, "commuteyother", base.Add(2*time.Hour))
	allPosts := []string{blockedCommunityOtherPost, blockedCommunityViewerPost, visibleCommunityOtherPost}

	postRepo := NewPostRepository(db)
	commentRepo := NewCommentRepository(db)
	threadComment := &comments.Comment{
		URI:          "at://" + otherUserDID + "/social.coves.community.comment/commutexcomment",
		CID:          "bafycommentcommutex",
		RKey:         "commutexcomment",
		CommenterDID: otherUserDID,
		RootURI:      blockedCommunityOtherPost,
		RootCID:      "bafypostcommutexother",
		ParentURI:    blockedCommunityOtherPost,
		ParentCID:    "bafypostcommutexother",
		Content:      "comment beneath blocked-community post",
		CreatedAt:    base.Add(3 * time.Hour),
	}
	require.NoError(t, commentRepo.Create(ctx, threadComment))

	discoverRepo := NewDiscoverRepository(db, "test-secret")
	readDiscover := func(t *testing.T, viewer string) []string {
		t.Helper()
		feed, _, err := discoverRepo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: viewer,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return discoverURIs(feed)
	}

	timelineRepo := NewTimelineRepository(db, "test-secret")
	readTimeline := func(t *testing.T, userDID string) []string {
		t.Helper()
		feed, _, err := timelineRepo.GetTimeline(ctx, timeline.GetTimelineRequest{
			UserDID: userDID,
			Sort:    "new",
			Limit:   50,
		})
		require.NoError(t, err)
		return timelineURIs(feed)
	}

	communityFeedRepo := NewCommunityFeedRepository(db, "test-secret")
	readCommunityFeed := func(t *testing.T, communityDID, viewer string) []string {
		t.Helper()
		feed, _, err := communityFeedRepo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: viewer,
			Sort:      "new",
			Limit:     50,
		})
		require.NoError(t, err)
		return communityFeedURIs(feed)
	}

	require.ElementsMatch(t, allPosts, readDiscover(t, viewerDID),
		"the viewer must see every fixture post in discover before blocking a community")
	require.ElementsMatch(t, allPosts, readTimeline(t, viewerDID),
		"the viewer must see every fixture post in the subscribed timeline before blocking a community")

	insertCommunityBlock(t, db, viewerDID, blockedCommunityDID)

	t.Run("discover hides every post in the blocked community for the blocker", func(t *testing.T) {
		assert.ElementsMatch(t, []string{visibleCommunityOtherPost}, readDiscover(t, viewerDID),
			"a community mute must remove every post from that community in discover, including the blocker's own posts")
	})

	t.Run("discover is unchanged for another user and for the public", func(t *testing.T) {
		assert.ElementsMatch(t, allPosts, readDiscover(t, otherUserDID),
			"community blocks are viewer-scoped and must not change another user's discover feed")
		assert.ElementsMatch(t, allPosts, readDiscover(t, ""),
			"community blocks are viewer preferences and must not change the public discover feed")
	})

	t.Run("timeline hides the blocked community although the viewer is still subscribed", func(t *testing.T) {
		assert.ElementsMatch(t, []string{visibleCommunityOtherPost}, readTimeline(t, viewerDID),
			"a community mute must override the viewer's subscription without deleting it")
	})

	t.Run("timeline is unchanged for a subscriber who has not blocked", func(t *testing.T) {
		assert.ElementsMatch(t, allPosts, readTimeline(t, otherUserDID),
			"one subscriber's community block must not change another subscriber's timeline")
	})

	t.Run("a permalink read still serves the blocked community's post to the blocker", func(t *testing.T) {
		views, err := postRepo.GetViewsByURIs(ctx, []string{blockedCommunityOtherPost}, viewerDID)
		require.NoError(t, err)
		require.Contains(t, views, blockedCommunityOtherPost,
			"a community mute must not turn an explicit post permalink into a missing post")
		assert.Equal(t, blockedCommunityOtherPost, views[blockedCommunityOtherPost].URI)
	})

	t.Run("the comment thread under it stays reachable", func(t *testing.T) {
		list, _, err := commentRepo.ListByParentWithHotRank(
			ctx, blockedCommunityOtherPost, "new", "", 50, nil, viewerDID)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{threadComment.URI}, commentURIs(list),
			"a community mute must not hide the explicitly requested comment thread beneath its post")
	})

	t.Run("the community's own feed still serves the blocked community to the blocker", func(t *testing.T) {
		// Opening the community feed is an explicit request that overrides the
		// aggregate mute, regardless of the Discover and timeline filters.
		assert.ElementsMatch(t, []string{blockedCommunityOtherPost, blockedCommunityViewerPost},
			readCommunityFeed(t, blockedCommunityDID, viewerDID),
			"an explicit community-feed read must not apply the aggregate community mute")
	})
}
