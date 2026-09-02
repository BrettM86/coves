//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPosts_StopwordOnlyQueryReturnsNothing(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	community, _ := seedSearchQueryPosts(t, db, "stop", "The quick brown fox", "A tale of the city")
	repo := NewCommunityFeedRepository(db, "test-secret")

	for _, query := range []string{"the", "a the of"} {
		feed, cursor, err := repo.SearchPosts(context.Background(), searchQueryRequest(community, query))
		require.NoErrorf(t, err, "a stopword-only query %q must return an empty result rather than fail", query)
		assert.Emptyf(t, feed,
			"a stopword-only query %q parses to an empty tsquery; matching it would dump the corpus on common typing", query)
		assert.Nilf(t, cursor,
			"a stopword-only query %q has no results and therefore must not advertise another page", query)
	}
}

func TestSearchPosts_NegationOnlyQueryReturnsNothing(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	community, _ := seedSearchQueryPosts(t, db, "negonly", "The quick brown fox", "A tale of the city")

	feed, cursor, err := NewCommunityFeedRepository(db, "test-secret").SearchPosts(
		context.Background(), searchQueryRequest(community, "-fox"))
	require.NoError(t, err, "a negation-only query must return an empty result rather than fail")
	assert.Empty(t, feed,
		"websearch_to_tsquery turns '-fox' into !'fox', which every post lacking the word satisfies. Returning those rows creates a whole-corpus scan and result dump per request; search must refuse negation-only queries")
	assert.Nil(t, cursor, "a refused negation-only query has no page position and must return a nil cursor")
}

func TestSearchPosts_NegationExcludesAndPhraseRequiresAdjacency(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	community, uris := seedSearchQueryPosts(t, db, "operators",
		"Quick brown fox jumps",
		"Quick red fox sleeps",
		"Brown quick fox naps",
	)
	repo := NewCommunityFeedRepository(db, "test-secret")

	negatedFeed, _, err := repo.SearchPosts(context.Background(), searchQueryRequest(community, "fox -red"))
	require.NoError(t, err, "searching with a positive term and negation")
	assert.ElementsMatch(t, []string{uris[0], uris[2]}, feedURIs(negatedFeed),
		"a positive query with -red must retain both non-red fox posts and exclude the red one; ignoring negation changes the meaning of the user's search")

	phraseFeed, _, err := repo.SearchPosts(context.Background(), searchQueryRequest(community, `"quick brown"`))
	require.NoError(t, err, "searching for a quoted phrase")
	assert.ElementsMatch(t, []string{uris[0]}, feedURIs(phraseFeed),
		"the quoted phrase 'quick brown' must require adjacency in that order. Brown quick fox contains both words but not the phrase, so returning it means the phrase operator was reduced to an unordered bag of words")
}

func TestSearchPosts_SQLMetacharactersInQueryAreInert(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, uris := seedSearchQueryPosts(t, db, "sql", "Harmless inventory")
	repo := NewCommunityFeedRepository(db, "test-secret")

	_, _, err := repo.SearchPosts(ctx, searchQueryRequest(community, `'; DROP TABLE posts; --`))
	require.NoError(t, err,
		"SQL metacharacters in a search query must remain inert bound-parameter data, never become SQL or make the repository fail")

	inventoryFeed, inventoryCursor, err := repo.SearchPosts(ctx, searchQueryRequest(community, `inventory') OR 1=1 --`))
	require.NoError(t, err, "SQL-looking syntax around a literal search word must remain valid bound query text")
	assert.ElementsMatch(t, []string{uris[0]}, feedURIs(inventoryFeed),
		"the SQL-looking query must still search its literal inventory word and must not turn OR 1=1 into a SQL predicate that returns unrelated rows")
	assert.Nil(t, inventoryCursor, "the single inventory result below the limit must not emit a cursor")

	var postCount int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM posts`).Scan(&postCount)
	require.NoError(t, err,
		"the posts table disappeared after SQL metacharacters were searched; query text must always be passed as a bound parameter")
	assert.GreaterOrEqual(t, postCount, 1, "the harmless inventory row must still exist after both SQL-looking searches")

	longQuery := strings.Repeat("inventory ", 60)
	require.Len(t, []byte(longQuery), 600, "the repository length-policy fixture must be exactly 600 bytes")
	_, _, err = repo.SearchPosts(ctx, searchQueryRequest(community, longQuery))
	require.NoError(t, err,
		"the repository must not choke on a 600-byte query; enforcing the public query-length policy is the service layer's responsibility")
}

func seedSearchQueryPosts(t *testing.T, db *sql.DB, label string, titles ...string) (string, []string) {
	t.Helper()

	community := visibilityCommunity(t, db, "srchq"+label)
	author := "did:plc:srchq" + label + "author"
	createTestUser(t, db, "srchq"+label+"author.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	uris := make([]string, 0, len(titles))
	for index, title := range titles {
		rkey := fmt.Sprintf("srchq%s%d", label, index+1)
		uri := seedVisibilityPost(t, db, community, author, rkey, title, base.Add(time.Duration(index+1)*time.Hour))
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+rkey, "")
		uris = append(uris, uri)
	}
	return community, uris
}

func searchQueryRequest(communityDID, query string) communityFeeds.SearchPostsRequest {
	return communityFeeds.SearchPostsRequest{
		Query:     query,
		Community: communityDID,
		ViewerDID: "",
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
	}
}

func TestSearchPosts_ORWithNegatedBranchThatNormalizesToTrueReturnsNothing(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	community, uris := seedSearchQueryPosts(t, db, "ornegation", "Quick fox")
	repo := NewCommunityFeedRepository(db, "test-secret")

	positiveFeed, _, err := repo.SearchPosts(context.Background(), searchQueryRequest(community, "fox"))
	require.NoError(t, err, "the bare positive term must be a valid search")
	require.Equal(t, uris, feedURIs(positiveFeed),
		"fixture: the quick fox post must match bare fox or an empty mixed query would pass vacuously")

	feed, cursor, err := repo.SearchPosts(context.Background(), searchQueryRequest(community, "fox OR -red"))
	require.NoError(t, err,
		"an OR query with a negated branch that normalizes to true must return an empty result rather than fail")
	assert.Empty(t, feed,
		"querytree renders 'fox OR -red' as T, so executing it would match the whole visible corpus; the search guard must refuse it even though bare fox has a real match")
	assert.Nil(t, cursor,
		"a mixed OR-negation query refused as universally true has no result position and must not emit a cursor")
}
