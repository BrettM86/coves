//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotContinuationRefillsAfterLiveAuthorBlock(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "hotcontinuation")

	communities := []string{
		"did:plc:blkhotcontinuationa",
		"did:plc:blkhotcontinuationb",
		"did:plc:blkhotcontinuationc",
		"did:plc:blkhotcontinuationd",
	}
	for i, communityDID := range communities {
		createTestCommunity(t, db, communityDID, "c-blkhotcontinuation"+string(rune('a'+i))+".coves.social", cast.viewer)
	}

	createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	seed := func(communityDID, authorDID, rkey string, score int) string {
		t.Helper()
		uri := seedFilterablePost(t, db, communityDID, authorDID, rkey, createdAt)
		_, err := db.ExecContext(ctx, `
			UPDATE posts
			SET score = $2, upvote_count = $2
			WHERE uri = $1
		`, uri, score)
		require.NoErrorf(t, err, "ranking post %s", rkey)
		return uri
	}

	a1 := seed(communities[0], cast.thirdParty, "hotconta1", 1_000_000)
	blockedA2 := seed(communities[0], cast.blocked, "hotconta2", 120_000)
	a3 := seed(communities[0], cast.thirdParty, "hotconta3", 50_000)
	b1 := seed(communities[1], cast.thirdParty, "hotcontb1", 200_000)
	b2 := seed(communities[1], cast.thirdParty, "hotcontb2", 10_000)
	c1 := seed(communities[2], cast.thirdParty, "hotcontc1", 1_500)
	d1 := seed(communities[3], cast.thirdParty, "hotcontd1", 10)

	repo := NewDiscoverRepository(db, "hot-continuation-secret")
	page := func(cursor *string) ([]string, *string) {
		t.Helper()
		feed, next, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{
			ViewerDID: cast.viewer,
			Sort:      "hot",
			Limit:     2,
			Cursor:    cursor,
		})
		require.NoError(t, err)
		return discoverURIs(feed), next
	}

	pageOne, cursor := page(nil)
	require.Equal(t, []string{a1, b1}, pageOne, "fixture must establish the intended snapshot checkpoint")
	require.NotNil(t, cursor, "snapshot has five candidates beyond page one")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	pageTwo, cursor := page(cursor)
	require.Equal(t, []string{c1, a3}, pageTwo,
		"the newly blocked candidate must be skipped and replaced without adding its community to emitted diversity history")

	returned := append([]string{}, pageOne...)
	returned = append(returned, pageTwo...)
	for continuationPages := 0; cursor != nil; continuationPages++ {
		require.Less(t, continuationPages, 4, "Discover Hot continuation did not exhaust")
		var next []string
		next, cursor = page(cursor)
		require.NotEmpty(t, next, "a continuation cursor must not lead to an empty filtered page")
		returned = append(returned, next...)
	}

	expected := []string{a1, b1, c1, a3, b2, d1}
	assert.Equal(t, expected, returned,
		"continuation must exhaust eligible snapshot candidates in diversified order")
	assert.NotContains(t, returned, blockedA2, "the live-blocked snapshot candidate must never be returned")
	unique := make(map[string]struct{}, len(returned))
	for _, uri := range returned {
		unique[uri] = struct{}{}
	}
	assert.Len(t, unique, len(expected), "eligible snapshot candidates must not be duplicated")
}
