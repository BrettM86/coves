//go:build integration

package postgres_test

import (
	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the community feed's SQL actually orders by, and what its cursor
// actually carries.
//
// Every sort ("hot", "top" with a timeframe, "new") is one ORDER BY clause in
// feed_repo.go, and every page boundary is a keyset predicate derived from the
// opaque cursor that clause emits. Neither can be tested against a fake
// repository: the hot rank is computed in SQL from score and age, the timeframe
// filter is a WHERE on created_at, and the pagination bugs pinned below were all
// mismatches between what the cursor encoded and what the next query compared it
// against. So the suite belongs beside the query, seeded with rows chosen to make
// each sort produce a DIFFERENT answer.
//
// It drives the XRPC handler rather than the repository directly because the
// cursor is only opaque at that boundary — encoding, signing and decoding it is
// part of what is under test, and the response shape (Record, Community, Stats)
// is what a client actually receives. The handler is called in process with
// httptest; nothing here needs a PDS, a Jetstream or a running AppView, which is
// why the package's Postgres-only floor is enough.
//
// This file is in the external test package because it uses Coves/tests/fixtures
// for row seeding; see that package's doc comment for why its callers must be.

// feedCursorSecret is the HMAC key the feed repository signs cursors with. Any
// value works as long as one test uses one value: a cursor minted under one key
// and presented under another is rejected, which is the behaviour a shared
// constant keeps out of the way of the tests that are about ordering.
const feedCursorSecret = "test-cursor-secret"

// newCommunityFeedHandler wires the real repository, service and handler over a
// test database.
//
// Every dependency the feed itself does not need is nil: no PDS account
// provisioner or client factory (nothing here writes to a repo), no vote service
// (no viewer, so no viewer state to hydrate) and no Bluesky service (no
// cross-network embeds). Passing real ones would make these tests fail for
// reasons that have nothing to do with ordering.
func newCommunityFeedHandler(db *sql.DB) *communityFeed.GetCommunityHandler {
	communityService := communities.NewCommunityServiceWithPDSFactory(
		postgres.NewCommunityRepository(db),
		testkit.Endpoints().PDS.BaseURL,
		fixtures.InstanceDID(),
		testkit.Endpoints().PDS.HandleDomain,
		nil,
		nil,
		nil,
		communities.PrivateHostOptions(true)...,
	)
	feedService := communityFeeds.NewCommunityFeedService(
		postgres.NewCommunityFeedRepository(db, feedCursorSecret),
		communityService,
	)
	return communityFeed.NewGetCommunityHandler(feedService, nil, nil)
}

// getCommunityFeed issues one feed request and decodes the response, failing the
// test on any status other than 200.
func getCommunityFeed(t *testing.T, handler *communityFeed.GetCommunityHandler, query string) communityFeeds.FeedResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.communityFeed.getCommunity?"+query, nil))
	require.Equalf(t, http.StatusOK, rec.Code, "GET ?%s: %s", query, rec.Body.String())

	var response communityFeeds.FeedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	return response
}

// getPostTitle reads the title out of a post view's record.
//
// The record is served as the raw indexed JSON rather than a typed struct, so
// asserting on ordering means reaching through an interface{}; every failure
// mode of that reach is a broken response rather than a broken test, hence
// Fatalf rather than a zero value.
func getPostTitle(t *testing.T, pv *posts.PostView) string {
	t.Helper()
	if pv.Record == nil {
		t.Fatalf("getPostTitle: Record is nil for post URI %s", pv.URI)
	}
	record, ok := pv.Record.(map[string]interface{})
	if !ok {
		t.Fatalf("getPostTitle: Record is %T, not map[string]interface{}", pv.Record)
	}
	title, ok := record["title"].(string)
	if !ok {
		t.Fatalf("getPostTitle: title field missing or not string, Record: %+v", record)
	}
	return title
}

// TestGetCommunityFeed_Hot covers the hot sort's response shape: every post in
// the community comes back, and each one carries the record, community and
// author references the lexicon's postView promises. The ranking itself is
// pinned by the three regression tests further down, which are precise about it;
// this one is deliberately not, because a hot rank computed against NOW() is not
// stable enough to assert an exact order on.
func TestGetCommunityFeed_Hot(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	// Three posts spanning the two axes the hot rank trades off against each
	// other: recency and score.
	post1URI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Recent trending post", 50, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, communityDID, "did:plc:bob", "Old popular post", 100, time.Now().Add(-24*time.Hour))
	post3URI := fixtures.Post(t, db, communityDID, "did:plc:charlie", "Brand new post", 5, time.Now().Add(-10*time.Minute))

	response := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=hot&limit=10", communityDID))

	assert.Len(t, response.Feed, 3)

	uris := []string{response.Feed[0].Post.URI, response.Feed[1].Post.URI, response.Feed[2].Post.URI}
	assert.Contains(t, uris, post1URI)
	assert.Contains(t, uris, post2URI)
	assert.Contains(t, uris, post3URI)

	for i, feedPost := range response.Feed {
		assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)
		record, ok := feedPost.Post.Record.(map[string]interface{})
		require.True(t, ok, "Record should be a map")
		assert.Equal(t, "social.coves.community.post", record["$type"], "Record should have correct $type")
		assert.NotEmpty(t, record["community"], "Record should have community")
		assert.NotEmpty(t, record["author"], "Record should have author")
		assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")

		// The community reference is hydrated by a join, not echoed from the
		// record, so a broken join shows up as an empty handle rather than a
		// missing field.
		assert.NotNil(t, feedPost.Post.Community, "Post %d should have community reference", i)
		assert.NotEmpty(t, feedPost.Post.Community.Handle, "Post %d community should have handle", i)
		assert.NotEmpty(t, feedPost.Post.Community.DID, "Post %d community should have DID", i)
		assert.NotEmpty(t, feedPost.Post.Community.Name, "Post %d community should have name", i)
	}
}

// TestGetCommunityFeed_Top_WithTimeframe covers the top sort, whose timeframe is
// a WHERE on created_at applied before the ORDER BY. The seed makes the two
// disagree on purpose: the highest-scoring post is the one the "day" window
// excludes, so a timeframe that was ignored would put it first in both cases.
func TestGetCommunityFeed_Top_WithTimeframe(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	fixtures.Post(t, db, communityDID, "did:plc:alice", "2 hours old", 100, time.Now().Add(-2*time.Hour))
	fixtures.Post(t, db, communityDID, "did:plc:bob", "2 days old", 200, time.Now().Add(-48*time.Hour))
	fixtures.Post(t, db, communityDID, "did:plc:charlie", "30 minutes old", 50, time.Now().Add(-30*time.Minute))

	t.Run("Top posts from last day", func(t *testing.T) {
		response := getCommunityFeed(t, handler,
			fmt.Sprintf("community=%s&sort=top&timeframe=day&limit=10", communityDID))

		// The 2-day-old post is outside the window even though it scores highest.
		assert.Len(t, response.Feed, 2)
		assert.Equal(t, "2 hours old", getPostTitle(t, response.Feed[0].Post))
		assert.Equal(t, 100, response.Feed[0].Post.Stats.Score)
	})

	t.Run("Top posts from all time", func(t *testing.T) {
		response := getCommunityFeed(t, handler,
			fmt.Sprintf("community=%s&sort=top&timeframe=all&limit=10", communityDID))

		assert.Len(t, response.Feed, 3)
		assert.Equal(t, "2 days old", getPostTitle(t, response.Feed[0].Post))
		assert.Equal(t, 200, response.Feed[0].Post.Stats.Score)
	})
}

// TestGetCommunityFeed_New pins chronological ordering. The middle post carries
// by far the highest score, so a "new" sort that fell back to score would put it
// first.
func TestGetCommunityFeed_New(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("news-%s", testID), fmt.Sprintf("charlie-%s.test", testID))
	require.NoError(t, err)

	fixtures.Post(t, db, communityDID, "did:plc:alice", "Oldest post", 10, time.Now().Add(-3*time.Hour))
	fixtures.Post(t, db, communityDID, "did:plc:bob", "Middle post", 100, time.Now().Add(-2*time.Hour))
	fixtures.Post(t, db, communityDID, "did:plc:charlie", "Newest post", 1, time.Now().Add(-1*time.Hour))

	response := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=new&limit=10", communityDID))

	assert.Len(t, response.Feed, 3)
	assert.Equal(t, "Newest post", getPostTitle(t, response.Feed[0].Post))
	assert.Equal(t, "Middle post", getPostTitle(t, response.Feed[1].Post))
	assert.Equal(t, "Oldest post", getPostTitle(t, response.Feed[2].Post))
}

// TestGetCommunityFeed_Pagination walks a 25-post community in pages of ten and
// asserts the three properties a keyset cursor has to hold: no post appears on
// two pages, the short final page still returns its rows, and the cursor is
// absent exactly when there is nothing left.
func TestGetCommunityFeed_Pagination(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("pagination-%s", testID), fmt.Sprintf("pager-%s.test", testID))
	require.NoError(t, err)

	for i := 0; i < 25; i++ {
		fixtures.Post(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), i,
			time.Now().Add(-time.Duration(i)*time.Minute))
	}

	page1 := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=new&limit=10", communityDID))
	assert.Len(t, page1.Feed, 10)
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	page2 := getCommunityFeed(t, handler,
		fmt.Sprintf("community=%s&sort=new&limit=10&cursor=%s", communityDID, *page1.Cursor))
	assert.Len(t, page2.Feed, 10)

	page1URIs := make(map[string]bool)
	for _, p := range page1.Feed {
		page1URIs[p.Post.URI] = true
	}
	for _, p := range page2.Feed {
		assert.False(t, page1URIs[p.Post.URI], "Found duplicate post between pages")
	}

	require.NotNil(t, page2.Cursor, "Expected cursor for page 3")
	page3 := getCommunityFeed(t, handler,
		fmt.Sprintf("community=%s&sort=new&limit=10&cursor=%s", communityDID, *page2.Cursor))

	assert.Len(t, page3.Feed, 5)
	assert.Nil(t, page3.Cursor, "Should not have cursor on last page")
}

// TestGetCommunityFeed_InvalidCommunity asserts a feed for a community that was
// never indexed answers 404 with the lexicon's CommunityNotFound error name,
// rather than an empty 200 that a client would render as "no posts yet".
func TestGetCommunityFeed_InvalidCommunity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.communityFeed.getCommunity?community=did:plc:nonexistent&sort=hot&limit=10", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, "CommunityNotFound", errResp["error"])
}

// TestGetCommunityFeed_InvalidCursor is a trust-boundary test: the cursor is
// client-supplied and is decoded into a timestamp and an AT-URI that go straight
// into the keyset predicate. Each case is a shape that must be rejected before it
// reaches SQL — including a base64-encoded SQL injection, which proves the
// rejection happens in the decoder rather than being neutralised downstream by
// parameter binding alone.
func TestGetCommunityFeed_InvalidCursor(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("cursortest-%s", testID), fmt.Sprintf("cursor-%s.test", testID))
	require.NoError(t, err)

	tests := []struct {
		name   string
		cursor string
	}{
		{"Invalid base64", "not-base64!!!"},
		{"Malicious SQL", "JyBPUiAnMSc9JzE="},                                  // ' OR '1'='1
		{"Invalid timestamp", "bWFsaWNpb3VzOnN0cmluZw=="},                      // malicious:string
		{"Invalid URI format", "MjAyNS0wMS0wMVQwMDowMDowMFo6bm90LWF0LXVyaQ=="}, // 2025-01-01T00:00:00Z:not-at-uri
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
				"/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10&cursor=%s",
				communityDID, tt.cursor), nil)
			rec := httptest.NewRecorder()
			handler.HandleGetCommunity(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)

			var errResp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))

			// Both names are in the lexicon and both are honest about the cause;
			// which one comes back depends on how far the cursor got before it
			// was rejected, and that is not worth pinning.
			errorCode, ok := errResp["error"].(string)
			require.True(t, ok, "error response must name an error code: %v", errResp)
			assert.True(t, errorCode == "InvalidRequest" || errorCode == "InvalidCursor",
				"Expected InvalidRequest or InvalidCursor, got %s", errorCode)
		})
	}
}

// TestGetCommunityFeed_EmptyFeed asserts an indexed community with no posts is a
// 200 with an empty feed and no cursor — the opposite of the 404 above, and the
// case where a cursor emitted for an empty page would send a client into a loop.
func TestGetCommunityFeed_EmptyFeed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("empty-%s", testID), fmt.Sprintf("empty-%s.test", testID))
	require.NoError(t, err)

	response := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=hot&limit=10", communityDID))

	assert.Len(t, response.Feed, 0)
	assert.Nil(t, response.Cursor)
}

// TestGetCommunityFeed_LimitValidation covers the pagination bound. An
// unbounded limit is how a single request turns into a full table scan, so an
// over-large one is refused rather than silently clamped; a missing one falls
// back to the default.
func TestGetCommunityFeed_LimitValidation(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("limittest-%s", testID), fmt.Sprintf("limit-%s.test", testID))
	require.NoError(t, err)

	t.Run("Reject limit over 50", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
			"/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=100", communityDID), nil)
		rec := httptest.NewRecorder()
		handler.HandleGetCommunity(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))

		assert.Equal(t, "InvalidRequest", errResp["error"])
		assert.Contains(t, errResp["message"], "limit must not exceed 50")
	})

	t.Run("Handle zero limit with default", func(t *testing.T) {
		// limit=0 means "unspecified", not "no posts": the handler substitutes
		// its default rather than refusing the request.
		getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=hot&limit=0", communityDID))
	})
}

// TestGetCommunityFeed_HotPaginationBug is a regression pin.
//
// The hot cursor used to carry the leading post's RAW SCORE, and the next page's
// predicate was "WHERE p.score < <cursor score>". That is only equivalent to
// "hot_rank < <cursor rank>" when score and hot rank agree on the ordering, and
// the whole point of a hot rank is that they do not: a recent post with score 17
// outranks an old one with score 100, and paging past the former then dropped the
// latter out of the feed entirely.
//
// The seed reproduces exactly that arrangement, and the assertion that matters is
// that Post B — old, high score, low hot rank — is still reachable on page 2.
func TestGetCommunityFeed_HotPaginationBug(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("hotbug-%s", testID), fmt.Sprintf("hotbug-%s.test", testID))
	require.NoError(t, err)

	// Post A: recent, low score      → highest hot rank, page 1
	// Post B: old, highest raw score → low hot rank, must survive to page 2
	// Post C: older, medium score    → lowest hot rank
	_ = fixtures.Post(t, db, communityDID, "did:plc:alice", "Recent trending", 17, time.Now().Add(-1*time.Hour))
	postB := fixtures.Post(t, db, communityDID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	_ = fixtures.Post(t, db, communityDID, "did:plc:charlie", "Older medium", 50, time.Now().Add(-36*time.Hour))

	page1 := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=hot&limit=1", communityDID))
	assert.Len(t, page1.Feed, 1)
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	firstPostURI := page1.Feed[0].Post.URI
	t.Logf("Page 1 - First post: %s (URI: %s)", getPostTitle(t, page1.Feed[0].Post), firstPostURI)

	page2 := getCommunityFeed(t, handler,
		fmt.Sprintf("community=%s&sort=hot&limit=2&cursor=%s", communityDID, *page1.Cursor))

	// The exact count is left loose because the hot rank is recomputed against
	// NOW() on the second request; the bug this pins showed up as posts vanishing
	// altogether, not as an off-by-one.
	assert.GreaterOrEqual(t, len(page2.Feed), 1, "Page 2 should contain at least 1 remaining post")
	assert.LessOrEqual(t, len(page2.Feed), 3, "Page 2 should contain at most 3 posts")

	allURIs := []string{firstPostURI}
	seenURIs := map[string]bool{firstPostURI: true}
	for _, p := range page2.Feed {
		allURIs = append(allURIs, p.Post.URI)
		t.Logf("Page 2 - Post: %s (URI: %s)", getPostTitle(t, p.Post), p.Post.URI)
		if seenURIs[p.Post.URI] {
			t.Errorf("Duplicate post found: %s", p.Post.URI)
		}
		seenURIs[p.Post.URI] = true
	}

	if !seenURIs[postB] {
		t.Fatalf("CRITICAL BUG: Post B (old, high score=100) missing - filtered by raw score cursor!")
	}

	t.Logf("Found %d total posts across pages (expected 3, time drift may cause slight variation)", len(allURIs))
}

// TestGetCommunityFeed_HotCursorPrecision is a regression pin.
//
// The hot rank is a float, and the cursor used to round it on the way out. Posts
// whose ranks differed by less than the surviving precision then compared equal
// to the cursor and were excluded by the strict "<" in the keyset predicate —
// silently dropped, never shown on any page.
//
// The three posts below share a score and are 100 milliseconds apart, which puts
// their hot ranks within roughly 1e-5 of each other. Post B is the one that
// disappeared.
func TestGetCommunityFeed_HotCursorPrecision(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("precision-%s", testID), fmt.Sprintf("prec-%s.test", testID))
	require.NoError(t, err)

	baseTime := time.Now().Add(-2 * time.Hour)
	postA := fixtures.Post(t, db, communityDID, "did:plc:alice", "Post A", 50, baseTime)
	postB := fixtures.Post(t, db, communityDID, "did:plc:bob", "Post B", 50, baseTime.Add(100*time.Millisecond))
	postC := fixtures.Post(t, db, communityDID, "did:plc:charlie", "Post C", 50, baseTime.Add(200*time.Millisecond))

	page1 := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=hot&limit=1", communityDID))
	assert.Len(t, page1.Feed, 1)
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	firstPostURI := page1.Feed[0].Post.URI
	t.Logf("Page 1 - First post: %s", firstPostURI)

	page2 := getCommunityFeed(t, handler,
		fmt.Sprintf("community=%s&sort=hot&limit=2&cursor=%s", communityDID, *page1.Cursor))

	assert.GreaterOrEqual(t, len(page2.Feed), 2, "Page 2 should contain at least 2 remaining posts")

	allURIs := map[string]bool{firstPostURI: true}
	for _, p := range page2.Feed {
		allURIs[p.Post.URI] = true
		t.Logf("Page 2 - Post: %s", p.Post.URI)
	}

	assert.True(t, allURIs[postA], "Post A missing")
	assert.True(t, allURIs[postB], "CRITICAL: Post B missing - cursor precision loss bug!")
	assert.True(t, allURIs[postC], "Post C missing")
}

// TestGetCommunityFeed_HotCursorTimeDrift is a regression pin.
//
// A hot rank is a function of age, so it changes between requests. The cursor
// used to store a rank computed with NOW() at page-1 time, and page 2 then
// recomputed every candidate's rank with a LATER NOW() before comparing against
// it. Posts drifted across the boundary in both directions: some were returned
// twice, others skipped.
//
// The fix stores the cursor's creation timestamp inside the cursor and uses that
// as "now" for subsequent pages, so the ranking a page-2 query sees is the same
// one the page-1 cursor was cut from.
//
// Fifteen posts one millisecond apart with identical scores is the worst case:
// the ranks are so close that any drift at all reorders them.
func TestGetCommunityFeed_HotCursorTimeDrift(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("timedrift-%s", testID), fmt.Sprintf("drift-%s.test", testID))
	require.NoError(t, err)

	baseTime := time.Now().Add(-1 * time.Hour)
	var allPostURIs []string
	for i := 0; i < 15; i++ {
		postURI := fixtures.Post(t, db, communityDID, fmt.Sprintf("did:plc:user%d", i),
			fmt.Sprintf("Post %d", i), 10, baseTime.Add(time.Duration(i)*time.Millisecond))
		allPostURIs = append(allPostURIs, postURI)
	}

	seenURIs := make(map[string]int)
	var cursor *string
	pageNum := 0

	for {
		pageNum++
		query := fmt.Sprintf("community=%s&sort=hot&limit=5", communityDID)
		if cursor != nil {
			query += "&cursor=" + *cursor
		}

		page := getCommunityFeed(t, handler, query)
		if len(page.Feed) == 0 {
			break
		}

		for _, p := range page.Feed {
			seenURIs[p.Post.URI]++
			if seenURIs[p.Post.URI] > 1 {
				t.Errorf("DUPLICATE on page %d: %s (seen %d times)", pageNum, p.Post.URI, seenURIs[p.Post.URI])
			}
		}

		cursor = page.Cursor
		if cursor == nil {
			break
		}

		// Fifteen posts in pages of five is four requests including the empty
		// one; anything past ten means the cursor stopped advancing.
		if pageNum > 10 {
			t.Fatal("Too many pages - possible infinite loop")
		}
	}

	assert.Equal(t, 15, len(seenURIs), "Should see all 15 posts")
	for uri, count := range seenURIs {
		if count != 1 {
			t.Errorf("Post %s seen %d times (expected 1)", uri, count)
		}
	}

	for _, uri := range allPostURIs {
		assert.Contains(t, seenURIs, uri, "Missing post: %s", uri)
	}
}

// TestGetCommunityFeed_BlobURLTransformation covers the rewrite the AppView
// applies on the way out: an embed's thumbnail is stored as the blob ref the
// author's PDS returned, and is served — with the image proxy configured, which
// production requires — as a proxy URL on the media hostname rather than a
// direct PDS getBlob URL.
//
// Three things are pinned. The URL is built from the COMMUNITY's DID, because
// that is the repository the blob lives in — pointing it at the author would
// 404. The embed's $type changes to the "#view" variant, because the served
// shape no longer matches the record schema and the postView union in
// social/coves/community/post/defs.json requires the view type on the wire. And
// the verbatim record still carries its blob ref, not the hydrated URL: scanPostView
// decodes the stored embed a second time rather than aliasing the view's map,
// and this is the only test that pins that against the feed path (see
// post_repo.go).
//
// This test is deliberately NOT parallel. It sets the process-wide image-URL
// config that scanPostView reads, so it must run in the serial phase — Go runs
// every non-parallel test to completion before any t.Parallel test resumes, so
// the config is set and restored inside a window no parallel sibling overlaps.
// Do not add t.Parallel here without moving the config off a global.
func TestGetCommunityFeed_BlobURLTransformation(t *testing.T) {
	db := testkit.DB(t)
	handler := newCommunityFeedHandler(db)

	// Serve URLs the way production does: through the image proxy on the media
	// hostname. The config is process-wide, so it is restored afterwards.
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://img.coves.social",
	})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("blobtest-%s", testID), fmt.Sprintf("blob-%s.test", testID))
	require.NoError(t, err)

	// This post carries an embed, which fixtures.Post does not model, so it is
	// inserted directly.
	authorDID := "did:plc:blobauthor" + testID
	fixtures.User(t, db, "blobauthor-"+testID+".test", authorDID)

	const thumbCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	embedJSON := `{
		"$type": "social.coves.embed.external",
		"external": {
			"uri": "https://example.com/article",
			"title": "Example Article",
			"description": "A test article",
			"thumb": {
				"$type": "blob",
				"ref": {
					"$link": "` + thumbCID + `"
				},
				"mimeType": "image/jpeg",
				"size": 52813
			}
		}
	}`

	rkey := testkit.TID()
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", communityDID, rkey)
	_, err = db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at, score, upvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), 10, 10)
	`, uri, "bafytest", rkey, authorDID, communityDID, "Post with blob thumb", embedJSON)
	require.NoError(t, err)

	response := getCommunityFeed(t, handler, fmt.Sprintf("community=%s&sort=new&limit=10", communityDID))
	require.Len(t, response.Feed, 1, "Should have one post")

	feedPost := response.Feed[0]
	require.NotNil(t, feedPost.Post.Embed, "Post should have embed")

	embedMap, ok := feedPost.Post.Embed.(map[string]interface{})
	require.True(t, ok, "Embed should be a map")
	assert.Equal(t, "social.coves.embed.external#view", embedMap["$type"],
		"Transformed embed should be declared as the external view type")

	external, ok := embedMap["external"].(map[string]interface{})
	require.True(t, ok, "External should be a map")

	thumbURL, ok := external["thumb"].(string)
	require.True(t, ok, "Thumb should be a string URL after transformation")

	// With the image proxy configured, the thumb addresses the media hostname,
	// never the PDS. A getBlob URL escaping into a feed response is media served
	// around the CDN that scans it.
	expectedURL := fmt.Sprintf("https://img.coves.social/img/embed_thumbnail/plain/%s/%s",
		communityDID, thumbCID)
	assert.Equal(t, expectedURL, thumbURL, "Thumb URL should be an image proxy URL")
	assert.NotContains(t, thumbURL, "com.atproto.sync.getBlob",
		"the AppView must not hand clients a direct PDS blob URL")

	// The lexicon calls `record` the post record verbatim, and hydration mutates
	// the embed in place. scanPostView therefore decodes the stored embed a
	// second time instead of aliasing the view's map — if it ever goes back to
	// sharing one map, the projection above would rewrite the record too and the
	// record would claim a record $type while carrying view-shaped fields.
	record, ok := feedPost.Post.Record.(map[string]interface{})
	require.True(t, ok, "Record should be a map")

	recordEmbed, ok := record["embed"].(map[string]interface{})
	require.True(t, ok, "the verbatim record must still carry its embed")
	assert.Equal(t, "social.coves.embed.external", recordEmbed["$type"],
		"the record embed must keep the record $type, not the hydrated #view type")

	recordExternal, ok := recordEmbed["external"].(map[string]interface{})
	require.True(t, ok, "record embed external should be a map")
	assert.IsType(t, map[string]interface{}{}, recordExternal["thumb"],
		"the record's thumb must still be a blob reference, not the hydrated URL string")
}
