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
