//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPosts_StemsQueryAndContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "srchStem")
	author := "did:plc:srchstemauthor"
	createTestUser(t, db, "srchstemauthor.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	runningShoes := seedVisibilityPost(t, db, community, author, "srchstemrunning", "Running shoes review", base.Add(2*time.Hour))
	marathonNutrition := seedVisibilityPost(t, db, community, author, "srchstemnutrition", "Marathon nutrition", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, community, runningShoes, posts.AdmissionStatusAccepted, "bafypostv2srchstemrunning", "")
	seedVisibilityAdmission(t, db, community, marathonNutrition, posts.AdmissionStatusAccepted, "bafypostv2srchstemnutrition", "")

	feed, _, err := NewCommunityFeedRepository(db, "test-secret").SearchPosts(ctx, communityFeeds.SearchPostsRequest{
		Query:     "run",
		Community: community,
		ViewerDID: "",
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
	})
	require.NoError(t, err, "a valid stemmed post search must not fail")
	assert.ElementsMatch(t, []string{runningShoes}, feedURIs(feed),
		"english stemming must reduce both 'running' and 'run' to the same lexeme. Missing the running-shoes post means search only matched literal substrings; returning marathon nutrition means a non-matching accepted post escaped the text predicate")
}

func TestSearchPosts_RelevanceRanksTitleHitAboveBodyHit(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "srchRank")
	author := "did:plc:srchrankauthor"
	createTestUser(t, db, "srchrankauthor.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	titleHit := seedVisibilityPost(t, db, community, author, "srchranktitle", "Kernel scheduling notes", base.Add(2*time.Hour))
	bodyHit := seedPostWithContentOnly(t, db, community, author, "srchrankbody", "notes on kernel bugs", base.Add(3*time.Hour))
	nonMatch := seedVisibilityPost(t, db, community, author, "srchranknone", "Garbage collection notes", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, community, titleHit, posts.AdmissionStatusAccepted, "bafypostv2srchranktitle", "")
	seedVisibilityAdmission(t, db, community, bodyHit, posts.AdmissionStatusAccepted, "bafypostv2srchrankbody", "")
	seedVisibilityAdmission(t, db, community, nonMatch, posts.AdmissionStatusAccepted, "bafypostv2srchranknone", "")

	feed, _, err := NewCommunityFeedRepository(db, "test-secret").SearchPosts(ctx, communityFeeds.SearchPostsRequest{
		Query:     "kernel",
		Community: community,
		ViewerDID: "",
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
	})
	require.NoError(t, err, "a valid relevance-sorted post search must not fail")
	assert.Equal(t, []string{titleHit, bodyHit}, feedURIs(feed),
		"a title hit carries weight A and a body hit weight B, so with one occurrence each the title must rank first even though the body post is newer; a tie, inverted order, or non-match means the search-vector weights were dropped or the text predicate was bypassed")
}
