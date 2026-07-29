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
