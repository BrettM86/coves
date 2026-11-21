//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A post deleted after the rank query has started must still be served on that
// page: the rank query and the hydration read one snapshot, so a page is never
// shorter than the ranking that produced it (an empty page with a cursor makes
// clients stop paging).
func TestSearchPosts_PageReadsOneSnapshotWhenARankedPostVanishes(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		lockClass = 1_126_644_054
		lockID    = 1
	)

	label := testkit.UniqueID(t)
	token := testkit.UniqueID(t)
	author := "did:plc:" + label + "author"
	createTestUser(t, db, label+"author.test", author)
	community := visibilityCommunity(t, db, label+"one")

	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seed := func(rkey string, createdAt time.Time) string {
		uri := seedVisibilityPost(t, db, community, author, rkey, token+" title", createdAt)
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+rkey, "")
		return uri
	}
	newer := seed("snapnew1", createdAt)
	vanishing := seed("snapold1", createdAt.Add(-time.Minute))

	repository := NewCommunityFeedRepository(db, searchEquivalenceCursorSecret,
		WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	request := communityFeeds.SearchPostsRequest{Query: token, Sort: "new", Timeframe: "all", Limit: 2}

	gate := installDiscoverHotHydrationGate(t, db, vanishing, lockClass, lockID)

	type pageResult struct {
		feed   []*communityFeeds.FeedViewPost
		cursor *string
		err    error
	}
	results := make(chan pageResult, 1)
	go func() {
		feed, cursor, err := repository.SearchPosts(ctx, request)
		results <- pageResult{feed: feed, cursor: cursor, err: err}
	}()

	// The rank query is now reading the vanishing post; delete it before the
	// hydration runs.
	waitForDiscoverHotAdvisoryLock(t, db, gate.holderPID)
	mutation, err := db.ExecContext(ctx, `
		UPDATE `+discoverHotHydrationStoredPosts+`
		SET deleted_at = clock_timestamp()
		WHERE uri = $1
	`, vanishing)
	require.NoError(t, err)
	mutated, err := mutation.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, mutated)
	gate.release(t)

	page := <-results
	require.NoError(t, page.err)
	assert.Equal(t, []string{newer, vanishing}, feedURIs(page.feed),
		"the page must hold every post its rank query ranked")
	assert.Nil(t, page.cursor)

	after, _, err := repository.SearchPosts(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, []string{newer}, feedURIs(after), "the next request sees the deletion")
}
