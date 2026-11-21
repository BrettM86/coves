//go:build integration

// Discover Hot's behaviour is decided almost entirely by SQL: the ranking
// expression, the cursor encoding and the ORDER BY that the cursor has to agree
// with all live in the Postgres repository, so a test with a stubbed repository
// would assert nothing that can actually break. These tests therefore run
// against a real database, serving the real repository through getFeed scope
// all (newDiscoverHotFeedHandler) in process — the shortest path that still
// exercises every layer the ranking passes through. The new and top sorts'
// tests are in internal/db/postgres/discover_feed_test.go, served through the
// getDiscover alias.
//
// They are in an external test package because they import
// Coves/internal/db/postgres, which imports this package: in-package that is an
// import cycle.
package discover_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorSecret is the key the repository signs pagination cursors with. Its
// value is irrelevant to these tests as long as the same repository instance
// both mints and verifies the cursor, which is what the pagination cases do.
const cursorSecret = "test-cursor-secret"

// TestGetDiscover_HotSort checks that hot ranking does not lose posts. The
// ordering itself is pinned by TestGetDiscover_HotSort_LogDampedRanking; what
// this case guards is that switching sort modes still returns the whole
// cross-community set.
func TestGetDiscover_HotSort(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	handler := newDiscoverHotFeedHandler(db, cursorSecret)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	post1URI := fixtures.Post(t, db, community1DID, "did:plc:alice", "Recent trending", 50, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, community2DID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	post3URI := fixtures.Post(t, db, community1DID, "did:plc:charlie", "Brand new", 5, time.Now().Add(-10*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getFeed?scope=all&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetFeed(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain recent trending post")
	assert.True(t, uris[post2URI], "Should contain old popular post")
	assert.True(t, uris[post3URI], "Should contain brand new post")
}

// TestGetDiscover_HotSort_LogDampedRanking pins the tuning of the log-damped
// hot rank: (SIGN(score)*LN(ABS(score)+1) + 1) / (age_hours + 2)^0.8
//
// The scenario mirrors the federation problem that motivated the formula: a
// bridged Lemmy post with a huge vote count from a much larger population must
// not bury fresh organic posts for days. At the same time, votes still matter —
// a two-day-old genuinely popular post should outrank a six-hour-old post
// nobody voted on. (Under the 0.8 decay a 782-vote post a single day old
// narrowly outranks a fresh 0-vote post, so the blowout is two days old.)
func TestGetDiscover_HotSort_LogDampedRanking(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	handler := newDiscoverHotFeedHandler(db, cursorSecret)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("ranking-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	// A bridged Lemmy-style blowout: 782 votes, two days old (rank ≈ 0.34)
	blowoutURI := fixtures.Post(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-48*time.Hour))
	// A brand-new organic post with no votes yet (rank ≈ 0.56)
	freshURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	// A six-hour-old post with no votes (rank ≈ 0.19)
	sixHourURI := fixtures.Post(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	// A brand-new but downvoted post (rank ≈ -0.44)
	downvotedURI := fixtures.Post(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getFeed?scope=all&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetFeed(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var response discoverPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	position := make(map[string]int)
	for i, post := range response.Feed {
		position[post.Post.URI] = i
	}
	for _, uri := range []string{blowoutURI, freshURI, sixHourURI, downvotedURI} {
		require.Contains(t, position, uri, "All posts should appear in the hot feed")
	}

	assert.Less(t, position[freshURI], position[blowoutURI],
		"Fresh 0-vote post should outrank a two-day-old vote blowout")
	assert.Less(t, position[blowoutURI], position[sixHourURI],
		"Two-day-old high-vote post should still outrank a 6h-old 0-vote post")
	assert.Less(t, position[sixHourURI], position[downvotedURI],
		"Post at score -5 (negative rank) should rank below these positive-rank posts")
}

// TestGetDiscover_HotSort_PaginationCoversNegativeScores paginates the exact
// LogDampedRanking scenario one post at a time. This exercises the cursor-side
// hot-rank expression (rebuilt with a pinned timestamp) against the live ORDER
// BY across the positive-to-negative rank boundary: every post must appear
// exactly once, in rank order, with no skips or duplicates. A divergence
// between the live and cursor formulas fails this test.
func TestGetDiscover_HotSort_PaginationCoversNegativeScores(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	handler := newDiscoverHotFeedHandler(db, cursorSecret)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("hotpage-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	blowoutURI := fixtures.Post(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-48*time.Hour))
	freshURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	sixHourURI := fixtures.Post(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	downvotedURI := fixtures.Post(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	expectedOrder := []string{freshURI, blowoutURI, sixHourURI, downvotedURI}

	// Paginate to exhaustion, one post per page (the database is a private
	// clone, so the feed contains exactly these four posts)
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		requestURL := "/xrpc/social.coves.feed.getFeed?scope=all&limit=1"
		if cursor != "" {
			requestURL += "&cursor=" + url.QueryEscape(cursor)
		}
		req := httptest.NewRequest(http.MethodGet, requestURL, nil)
		rec := httptest.NewRecorder()
		handler.HandleGetFeed(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "page %d should succeed", page)

		var response discoverPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))

		for _, post := range response.Feed {
			seen = append(seen, post.Post.URI)
		}
		if response.Cursor == nil {
			break
		}
		cursor = *response.Cursor
	}

	assert.Equal(t, expectedOrder, seen,
		"Pagination should return every post exactly once, in hot-rank order")
}

// TestGetDiscover_HotSort_NormalizesAndDiversifiesAcrossPages is the outer
// contract for Discover's community-aware Hot policy. All current candidates
// have the same age, so their ordering cannot drift with the test clock. Under
// the legacy raw formula the 100-vote high-baseline post beats the 45-vote
// ordinary-baseline post; partial normalization must reverse that comparison.
func TestGetDiscover_HotSort_NormalizesAndDiversifiesAcrossPages(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	handler := newDiscoverHotFeedHandler(db, cursorSecret)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	ordinaryCommunity, err := fixtures.Community(ctx, db, fmt.Sprintf("ordinary-%s", testID), fmt.Sprintf("ordinary-%s.test", testID))
	require.NoError(t, err)
	highBaselineCommunity, err := fixtures.Community(ctx, db, fmt.Sprintf("high-%s", testID), fmt.Sprintf("high-%s.test", testID))
	require.NoError(t, err)
	alternativeCommunity, err := fixtures.Community(ctx, db, fmt.Sprintf("alternative-%s", testID), fmt.Sprintf("alternative-%s.test", testID))
	require.NoError(t, err)

	fixtureNow := time.Now()
	matureCreatedAt := fixtureNow.Add(-72 * time.Hour)
	for i := 0; i < 5; i++ {
		fixtures.Post(t, db, ordinaryCommunity, "did:plc:ordinary-history", fmt.Sprintf("Ordinary history %d", i), 3, matureCreatedAt)
		fixtures.Post(t, db, highBaselineCommunity, "did:plc:high-history", fmt.Sprintf("High history %d", i), 1_000_000, matureCreatedAt)
		fixtures.Post(t, db, alternativeCommunity, "did:plc:alternative-history", fmt.Sprintf("Alternative history %d", i), 3, matureCreatedAt)
	}

	candidateCreatedAt := fixtureNow.Add(-2 * time.Hour)
	ordinaryFirst := fixtures.Post(t, db, ordinaryCommunity, "did:plc:ordinary-author", "Ordinary first", 45, candidateCreatedAt)
	highFirst := fixtures.Post(t, db, highBaselineCommunity, "did:plc:high-author", "High first", 100, candidateCreatedAt)
	ordinarySecond := fixtures.Post(t, db, ordinaryCommunity, "did:plc:ordinary-author", "Ordinary second", 35, candidateCreatedAt)
	alternativeFirst := fixtures.Post(t, db, alternativeCommunity, "did:plc:alternative-author", "Alternative first", 6, candidateCreatedAt)
	ordinaryThird := fixtures.Post(t, db, ordinaryCommunity, "did:plc:ordinary-author", "Ordinary third", 28, candidateCreatedAt)
	highSecond := fixtures.Post(t, db, highBaselineCommunity, "did:plc:high-author", "High second", 35, candidateCreatedAt)

	expected := []string{
		ordinaryFirst,
		highFirst,
		ordinarySecond,
		alternativeFirst,
		ordinaryThird,
		highSecond,
	}

	var returned []string
	cursor := ""
	for page := 0; page < 2; page++ {
		requestURL := "/xrpc/social.coves.feed.getFeed?scope=all&limit=3"
		if cursor != "" {
			requestURL += "&cursor=" + url.QueryEscape(cursor)
		}

		req := httptest.NewRequest(http.MethodGet, requestURL, nil)
		rec := httptest.NewRecorder()
		handler.HandleGetFeed(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "page %d should succeed: %s", page+1, rec.Body.String())

		var response discoverPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		require.Len(t, response.Feed, 3, "page %d should be full", page+1)
		for _, post := range response.Feed {
			returned = append(returned, post.Post.URI)
		}

		if page == 0 {
			require.NotNil(t, response.Cursor, "first page should continue the ranking session")
			cursor = *response.Cursor
		}
	}

	unique := make(map[string]struct{}, len(returned))
	for _, uri := range returned {
		require.NotContains(t, unique, uri, "paginated Discover Hot must not repeat a post")
		unique[uri] = struct{}{}
	}
	require.Equal(t, expected, returned,
		"normalization and progressive community penalties must produce one continuous cross-page order")
}

// TestGetDiscover_HotSort_FutureDatedPost guards the feed against records
// asserting a future created_at (hostile or clock-skewed federated repos).
// The ingestion path clamps these to now, but this test inserts one directly
// to pin the SQL-level defence in hotRankSQL: GREATEST(age, 0) means a
// future-dated post ranks like a brand-new 0-vote post — it must not error
// the query (negative POWER base) and must not outrank a post with real votes.
func TestGetDiscover_HotSort_FutureDatedPost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	handler := newDiscoverHotFeedHandler(db, cursorSecret)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("future-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	// A 0-vote post claiming to be from 10 days in the future
	futureURI := fixtures.Post(t, db, communityDID, "did:plc:mallory", "Future dated post", 0, time.Now().Add(240*time.Hour))
	// A recent post with modest genuine engagement (rank ≈ 2.05 vs the clamped ≈ 0.57)
	votedURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Recent voted post", 50, time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getFeed?scope=all&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetFeed(rec, req)

	// Before the GREATEST age clamp this query 500'd with "a negative number
	// raised to a non-integer power yields a complex result"
	require.Equal(t, http.StatusOK, rec.Code, "Future-dated post must not break the hot feed")

	var response discoverPage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))

	position := make(map[string]int)
	for i, post := range response.Feed {
		position[post.Post.URI] = i
	}
	require.Contains(t, position, futureURI)
	require.Contains(t, position, votedURI)

	assert.Less(t, position[votedURI], position[futureURI],
		"Future-dated 0-vote post should rank like a fresh post, not above genuinely engaged posts")
}
