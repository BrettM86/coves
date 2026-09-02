//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPosts_NewSortOrdersByCreatedAtAndPaginates(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, expected := seedSearchNewSortPosts(t, db, "new")
	repo := NewCommunityFeedRepository(db, "test-secret")

	page1, cursor1, err := repo.SearchPosts(ctx, searchSortRequest(community, "new", "all", 2, nil))
	require.NoError(t, err, "fetching new-sort page 1")
	assert.Equal(t, expected[:2], feedURIs(page1),
		"new-sort page 1 must contain Cobalt 5 then Cobalt 4 by created_at descending; a newer Nickel result means changing sort bypassed the text predicate")
	require.NotNil(t, cursor1, "new-sort page 1 must carry a cursor while three matching posts remain")

	page2, cursor2, err := repo.SearchPosts(ctx, searchSortRequest(community, "new", "all", 2, cursor1))
	require.NoError(t, err, "fetching new-sort page 2")
	assert.Equal(t, expected[2:4], feedURIs(page2),
		"new-sort page 2 must continue with Cobalt 3 then Cobalt 2 without repeating or skipping a created_at position")
	require.NotNil(t, cursor2, "new-sort page 2 must carry a cursor while one matching post remains")

	page3, cursor3, err := repo.SearchPosts(ctx, searchSortRequest(community, "new", "all", 2, cursor2))
	require.NoError(t, err, "fetching new-sort page 3")
	assert.Equal(t, expected[4:], feedURIs(page3), "new-sort page 3 must contain only the final Cobalt post")
	assert.Nil(t, cursor3, "the terminal new-sort page must have a nil cursor or clients will request a page that does not exist")

	got := append(feedURIs(page1), feedURIs(page2)...)
	got = append(got, feedURIs(page3)...)
	assert.Equal(t, expected, got,
		"new-sort pagination must return every matching Cobalt post exactly once in created_at descending order and must never admit the newer non-matching Nickel post")
}

func TestSearchPosts_TopSortOrdersByScoreThenCreatedAtAndPaginates(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community := visibilityCommunity(t, db, "srchsorttop")
	author := "did:plc:srchsorttopauthor"
	createTestUser(t, db, "srchsorttopauthor.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	definitions := []struct {
		rkey          string
		title         string
		score         int
		createdOffset time.Duration
	}{
		{"srchsorttop50", "Cobalt 50", 50, time.Hour},
		{"srchsorttop40new", "Cobalt 40 newer", 40, 5 * time.Hour},
		{"srchsorttop40old", "Cobalt 40 older", 40, 4 * time.Hour},
		{"srchsorttop10new", "Cobalt 10 newer", 10, 3 * time.Hour},
		{"srchsorttop10old", "Cobalt 10 older", 10, 2 * time.Hour},
	}
	expected := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		uri := seedVisibilityPost(t, db, community, author, definition.rkey, definition.title, base.Add(definition.createdOffset))
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+definition.rkey, "")
		setSearchPostScore(t, db, uri, definition.score)
		expected = append(expected, uri)
	}
	nonMatch := seedVisibilityPost(t, db, community, author, "srchsorttopnickel", "Nickel", base.Add(6*time.Hour))
	seedVisibilityAdmission(t, db, community, nonMatch, posts.AdmissionStatusAccepted, "bafypostv2srchsorttopnickel", "")
	setSearchPostScore(t, db, nonMatch, 1000)

	repo := NewCommunityFeedRepository(db, "test-secret")
	page1, cursor1, err := repo.SearchPosts(ctx, searchSortRequest(community, "top", "all", 2, nil))
	require.NoError(t, err, "fetching top-sort page 1")
	require.NotNil(t, cursor1, "top-sort page 1 must carry a cursor while three matching posts remain")
	page2, cursor2, err := repo.SearchPosts(ctx, searchSortRequest(community, "top", "all", 2, cursor1))
	require.NoError(t, err, "fetching top-sort page 2")
	require.NotNil(t, cursor2, "top-sort page 2 must carry a cursor while one matching post remains")
	page3, cursor3, err := repo.SearchPosts(ctx, searchSortRequest(community, "top", "all", 2, cursor2))
	require.NoError(t, err, "fetching top-sort page 3")
	assert.Nil(t, cursor3, "the terminal top-sort page must have a nil cursor")

	got := append(feedURIs(page1), feedURIs(page2)...)
	got = append(got, feedURIs(page3)...)
	assert.Equal(t, expected, got,
		"top-sort pagination must order by score descending and created_at descending within score ties, with no duplicates or gaps. Returning the score-1000 Nickel post means the sort bypassed the Cobalt text predicate")
}

func TestSearchPosts_TopSortHonoursTimeframe(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community := visibilityCommunity(t, db, "srchsorttimeframe")
	author := "did:plc:srchsorttimeframeauthor"
	createTestUser(t, db, "srchsorttimeframeauthor.test", author)
	now := time.Now().UTC()

	recent := seedVisibilityPost(t, db, community, author, "srchsorttimerecent", "Cobalt recent", now.Add(-2*time.Hour))
	old := seedVisibilityPost(t, db, community, author, "srchsorttimeold", "Cobalt old", now.Add(-3*24*time.Hour))
	seedVisibilityAdmission(t, db, community, recent, posts.AdmissionStatusAccepted, "bafypostv2srchsorttimerecent", "")
	seedVisibilityAdmission(t, db, community, old, posts.AdmissionStatusAccepted, "bafypostv2srchsorttimeold", "")
	setSearchPostScore(t, db, recent, 5)
	setSearchPostScore(t, db, old, 100)

	repo := NewCommunityFeedRepository(db, "test-secret")
	dayFeed, dayCursor, err := repo.SearchPosts(ctx, searchSortRequest(community, "top", "day", 50, nil))
	require.NoError(t, err, "searching top posts from the last day")
	assert.Equal(t, []string{recent}, feedURIs(dayFeed),
		"the day timeframe must exclude the three-day-old post even though its score is higher; applying top ordering without its timeframe leaks stale results into a bounded search")
	assert.Nil(t, dayCursor, "a one-result day search below the limit must not emit a cursor")

	allFeed, allCursor, err := repo.SearchPosts(ctx, searchSortRequest(community, "top", "all", 50, nil))
	require.NoError(t, err, "searching top posts across all time")
	assert.Equal(t, []string{old, recent}, feedURIs(allFeed),
		"the all timeframe must retain both matching posts and order the score-100 post before the newer score-5 post")
	assert.Nil(t, allCursor, "an all-time result set below the limit must not emit a cursor")
}

func TestSearchPosts_NewCursorIsRejectedUnderRelevanceAndViceVersa(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, _ := seedSearchNewSortPosts(t, db, "crosscursor")
	repo := NewCommunityFeedRepository(db, "test-secret")

	_, newCursor, err := repo.SearchPosts(ctx, searchSortRequest(community, "new", "all", 2, nil))
	require.NoError(t, err, "minting a new-sort cursor")
	require.NotNil(t, newCursor, "the full new-sort first page must mint the cursor under test")
	_, _, err = repo.SearchPosts(ctx, searchSortRequest(community, "relevance", "all", 2, newCursor))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"a correctly signed new-sort cursor is not a relevance position; accepting it under relevance corrupts pagination without reporting an invalid cursor")

	_, relevanceCursor, err := repo.SearchPosts(ctx, searchSortRequest(community, "relevance", "all", 2, nil))
	require.NoError(t, err, "minting a relevance cursor")
	require.NotNil(t, relevanceCursor, "the full relevance first page must mint the cursor under test")
	_, _, err = repo.SearchPosts(ctx, searchSortRequest(community, "new", "all", 2, relevanceCursor))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"a correctly signed relevance cursor is not a new-sort position; accepting it under new corrupts pagination even when both searches use the same query")
}

func seedSearchNewSortPosts(t *testing.T, db *sql.DB, label string) (string, []string) {
	t.Helper()

	community := visibilityCommunity(t, db, "srchsort"+label)
	author := "did:plc:srchsort" + label + "author"
	createTestUser(t, db, "srchsort"+label+"author.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	byNumber := make([]string, 6)
	for number := 1; number <= 5; number++ {
		rkey := fmt.Sprintf("srchsort%s%d", label, number)
		uri := seedVisibilityPost(t, db, community, author, rkey, fmt.Sprintf("Cobalt %d", number), base.Add(time.Duration(number)*time.Hour))
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+rkey, "")
		byNumber[number] = uri
	}
	nonMatchRkey := "srchsort" + label + "nickel"
	nonMatch := seedVisibilityPost(t, db, community, author, nonMatchRkey, "Nickel", base.Add(6*time.Hour))
	seedVisibilityAdmission(t, db, community, nonMatch, posts.AdmissionStatusAccepted, "bafypostv2"+nonMatchRkey, "")

	return community, []string{byNumber[5], byNumber[4], byNumber[3], byNumber[2], byNumber[1]}
}

func setSearchPostScore(t *testing.T, db *sql.DB, uri string, score int) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		UPDATE posts SET score = $2, upvote_count = $2 WHERE uri = $1
	`, uri, score)
	require.NoErrorf(t, err, "setting search score %d on %s", score, uri)
}

func searchSortRequest(communityDID, sort, timeframe string, limit int, cursor *string) communityFeeds.SearchPostsRequest {
	return communityFeeds.SearchPostsRequest{
		Query:     "cobalt",
		Community: communityDID,
		ViewerDID: "",
		Sort:      sort,
		Timeframe: timeframe,
		Limit:     limit,
		Cursor:    cursor,
	}
}
