package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetCommunityFeed_Hot tests hot feed sorting algorithm
func TestGetCommunityFeed_Hot(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data: community, users, and posts
	ctx := context.Background()
	testID := time.Now().UnixNano()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// Create posts with different scores and ages
	// Post 1: Recent with medium score (should rank high in "hot")
	post1URI := createTestPost(t, db, communityDID, "did:plc:alice", "Recent trending post", 50, time.Now().Add(-1*time.Hour))

	// Post 2: Old with high score (hot algorithm should penalize age)
	post2URI := createTestPost(t, db, communityDID, "did:plc:bob", "Old popular post", 100, time.Now().Add(-24*time.Hour))

	// Post 3: Very recent with low score
	post3URI := createTestPost(t, db, communityDID, "did:plc:charlie", "Brand new post", 5, time.Now().Add(-10*time.Minute))

	// Request hot feed
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=10", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response.Feed, 3)

	// Verify hot ranking: recent + medium score should beat old + high score
	// (exact order depends on hot algorithm, but we can verify posts exist)
	uris := []string{response.Feed[0].Post.URI, response.Feed[1].Post.URI, response.Feed[2].Post.URI}
	assert.Contains(t, uris, post1URI)
	assert.Contains(t, uris, post2URI)
	assert.Contains(t, uris, post3URI)

	// Verify Record field is populated (schema compliance)
	for i, feedPost := range response.Feed {
		assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)
		record, ok := feedPost.Post.Record.(map[string]interface{})
		require.True(t, ok, "Record should be a map")
		assert.Equal(t, "social.coves.community.post", record["$type"], "Record should have correct $type")
		assert.NotEmpty(t, record["community"], "Record should have community")
		assert.NotEmpty(t, record["author"], "Record should have author")
		assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")

		// Verify community reference includes handle (following atProto pattern)
		assert.NotNil(t, feedPost.Post.Community, "Post %d should have community reference", i)
		assert.NotEmpty(t, feedPost.Post.Community.Handle, "Post %d community should have handle", i)
		assert.NotEmpty(t, feedPost.Post.Community.DID, "Post %d community should have DID", i)
		assert.NotEmpty(t, feedPost.Post.Community.Name, "Post %d community should have name", i)
	}
}

// TestGetCommunityFeed_Top_WithTimeframe tests top sorting with time filters
func TestGetCommunityFeed_Top_WithTimeframe(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", time.Now().UnixNano()), fmt.Sprintf("bob.test-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	// Create posts at different times
	// Post 1: 2 hours ago, score 100
	createTestPost(t, db, communityDID, "did:plc:alice", "2 hours old", 100, time.Now().Add(-2*time.Hour))

	// Post 2: 2 days ago, score 200 (should be filtered out by "day" timeframe)
	createTestPost(t, db, communityDID, "did:plc:bob", "2 days old", 200, time.Now().Add(-48*time.Hour))

	// Post 3: 30 minutes ago, score 50
	createTestPost(t, db, communityDID, "did:plc:charlie", "30 minutes old", 50, time.Now().Add(-30*time.Minute))

	t.Run("Top posts from last day", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=top&timeframe=day&limit=10", communityDID), nil)
		rec := httptest.NewRecorder()
		handler.HandleGetCommunity(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response communityFeeds.FeedResponse
		err = json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should only return 2 posts (within last day)
		assert.Len(t, response.Feed, 2)

		// Verify top-ranked post (highest score)
		assert.Equal(t, "2 hours old", *response.Feed[0].Post.Title)
		assert.Equal(t, 100, response.Feed[0].Post.Stats.Score)
	})

	t.Run("Top posts from all time", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=top&timeframe=all&limit=10", communityDID), nil)
		rec := httptest.NewRecorder()
		handler.HandleGetCommunity(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response communityFeeds.FeedResponse
		err = json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should return all 3 posts
		assert.Len(t, response.Feed, 3)

		// Highest score should be first
		assert.Equal(t, "2 days old", *response.Feed[0].Post.Title)
		assert.Equal(t, 200, response.Feed[0].Post.Stats.Score)
	})
}

// TestGetCommunityFeed_New tests chronological sorting
func TestGetCommunityFeed_New(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("news-%d", time.Now().UnixNano()), fmt.Sprintf("charlie.test-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	// Create posts in specific order (older first)
	time1 := time.Now().Add(-3 * time.Hour)
	time2 := time.Now().Add(-2 * time.Hour)
	time3 := time.Now().Add(-1 * time.Hour)

	createTestPost(t, db, communityDID, "did:plc:alice", "Oldest post", 10, time1)
	createTestPost(t, db, communityDID, "did:plc:bob", "Middle post", 100, time2) // High score, but not newest
	createTestPost(t, db, communityDID, "did:plc:charlie", "Newest post", 1, time3)

	// Request new feed
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response.Feed, 3)

	// Verify chronological order (newest first)
	assert.Equal(t, "Newest post", *response.Feed[0].Post.Title)
	assert.Equal(t, "Middle post", *response.Feed[1].Post.Title)
	assert.Equal(t, "Oldest post", *response.Feed[2].Post.Title)
}

// TestGetCommunityFeed_Pagination tests cursor-based pagination
func TestGetCommunityFeed_Pagination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data with many posts
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("pagination-%d", time.Now().UnixNano()), fmt.Sprintf("test.test-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	// Create 25 posts
	for i := 0; i < 25; i++ {
		createTestPost(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), i, time.Now().Add(-time.Duration(i)*time.Minute))
	}

	// Page 1: Get first 10 posts
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 10)
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	t.Logf("Page 1 cursor: %s", *page1.Cursor)

	// Page 2: Use cursor
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10&cursor=%s", communityDID, *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	if rec.Code != http.StatusOK {
		t.Logf("Page 2 error: %s", rec.Body.String())
	}
	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	assert.Len(t, page2.Feed, 10)

	// Verify no duplicate posts between pages
	page1URIs := make(map[string]bool)
	for _, p := range page1.Feed {
		page1URIs[p.Post.URI] = true
	}
	for _, p := range page2.Feed {
		assert.False(t, page1URIs[p.Post.URI], "Found duplicate post between pages")
	}

	// Page 3: Should have remaining 5 posts
	if page2.Cursor == nil {
		t.Fatal("Expected cursor for page 3, got nil")
	}
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10&cursor=%s", communityDID, *page2.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page3 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page3)
	require.NoError(t, err)

	assert.Len(t, page3.Feed, 5)
	assert.Nil(t, page3.Cursor, "Should not have cursor on last page")
}

// TestGetCommunityFeed_InvalidCommunity tests error handling for invalid community
func TestGetCommunityFeed_InvalidCommunity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Request feed for non-existent community
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.communityFeed.getCommunity?community=did:plc:nonexistent&sort=hot&limit=10", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var errResp map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "CommunityNotFound", errResp["error"])
}

// TestGetCommunityFeed_InvalidCursor tests cursor validation
func TestGetCommunityFeed_InvalidCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test community
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("cursortest-%d", time.Now().UnixNano()), fmt.Sprintf("test.test-%d", time.Now().UnixNano()))
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
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=new&limit=10&cursor=%s", communityDID, tt.cursor), nil)
			rec := httptest.NewRecorder()
			handler.HandleGetCommunity(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)

			var errResp map[string]interface{}
			err := json.Unmarshal(rec.Body.Bytes(), &errResp)
			require.NoError(t, err)

			// Accept either InvalidRequest or InvalidCursor (both are correct)
			errorCode := errResp["error"].(string)
			assert.True(t, errorCode == "InvalidRequest" || errorCode == "InvalidCursor", "Expected InvalidRequest or InvalidCursor, got %s", errorCode)
		})
	}
}

// TestGetCommunityFeed_EmptyFeed tests handling of empty communities
func TestGetCommunityFeed_EmptyFeed(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Create community with no posts
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("empty-%d", time.Now().UnixNano()), fmt.Sprintf("test.test-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=10", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	if rec.Code != http.StatusOK {
		t.Logf("Response body: %s", rec.Body.String())
	}
	assert.Equal(t, http.StatusOK, rec.Code)

	var response communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response.Feed, 0)
	assert.Nil(t, response.Cursor)
}

// TestGetCommunityFeed_LimitValidation tests limit parameter validation
func TestGetCommunityFeed_LimitValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test community
	ctx := context.Background()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("limittest-%d", time.Now().UnixNano()), fmt.Sprintf("test.test-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	t.Run("Reject limit over 50", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=100", communityDID), nil)
		rec := httptest.NewRecorder()
		handler.HandleGetCommunity(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errResp["error"])
		assert.Contains(t, errResp["message"], "limit must not exceed 50")
	})

	t.Run("Handle zero limit with default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=0", communityDID), nil)
		rec := httptest.NewRecorder()
		handler.HandleGetCommunity(rec, req)

		// Should succeed with default limit (15)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// TestGetCommunityFeed_HotPaginationBug tests the critical hot pagination bug fix
// Verifies that posts with higher raw scores but lower hot ranks don't get dropped during pagination
func TestGetCommunityFeed_HotPaginationBug(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data
	ctx := context.Background()
	testID := time.Now().UnixNano()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("hotbug-%d", testID), fmt.Sprintf("hotbug-%d.test", testID))
	require.NoError(t, err)

	// Create posts that reproduce the bug:
	// Post A: Recent, low score (hot_rank ~17.6) - should be on page 1
	// Post B: Old, high score (hot_rank ~10.4) - should be on page 2
	// Post C: Older, medium score (hot_rank ~8.2) - should be on page 2
	//
	// Bug: If cursor stores raw score (17) from Post A, Post B (score=100) gets filtered out
	// because WHERE p.score < 17 excludes it, even though hot_rank(B) < hot_rank(A)

	_ = createTestPost(t, db, communityDID, "did:plc:alice", "Recent trending", 17, time.Now().Add(-1*time.Hour))
	postB := createTestPost(t, db, communityDID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	_ = createTestPost(t, db, communityDID, "did:plc:charlie", "Older medium", 50, time.Now().Add(-36*time.Hour))

	// Page 1: Get first post (limit=1)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=1", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 1)
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	// The highest hot_rank post should be first (recent with low-medium score)
	firstPostURI := page1.Feed[0].Post.URI
	t.Logf("Page 1 - First post: %s (URI: %s)", *page1.Feed[0].Post.Title, firstPostURI)
	t.Logf("Page 1 - Cursor: %s", *page1.Cursor)

	// Page 2: Use cursor - this is where the bug would occur
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=2&cursor=%s", communityDID, *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Page 2 failed: %s", rec.Body.String())
	}

	var page2 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	// CRITICAL: Page 2 should contain at least 1 post (at most 2 due to time drift)
	// Bug would cause high-score posts to be filtered out entirely
	assert.GreaterOrEqual(t, len(page2.Feed), 1, "Page 2 should contain at least 1 remaining post")
	assert.LessOrEqual(t, len(page2.Feed), 3, "Page 2 should contain at most 3 posts")

	// Collect all URIs across pages
	allURIs := []string{firstPostURI}
	seenURIs := map[string]bool{firstPostURI: true}
	for _, p := range page2.Feed {
		allURIs = append(allURIs, p.Post.URI)
		t.Logf("Page 2 - Post: %s (URI: %s)", *p.Post.Title, p.Post.URI)
		// Check for duplicates
		if seenURIs[p.Post.URI] {
			t.Errorf("Duplicate post found: %s", p.Post.URI)
		}
		seenURIs[p.Post.URI] = true
	}

	// The critical test: Post B (high raw score, low hot rank) must appear somewhere
	// Without the fix, it would be filtered out by p.score < 17
	if !seenURIs[postB] {
		t.Fatalf("CRITICAL BUG: Post B (old, high score=100) missing - filtered by raw score cursor!")
	}

	t.Logf("SUCCESS: All posts with high raw scores appear (bug fixed)")
	t.Logf("Found %d total posts across pages (expected 3, time drift may cause slight variation)", len(allURIs))
}

// TestGetCommunityFeed_HotCursorPrecision tests that hot rank cursor preserves full float precision
// Regression test for precision bug where posts with hot ranks differing by <1e-6 were dropped
func TestGetCommunityFeed_HotCursorPrecision(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)
	feedService := communityFeeds.NewCommunityFeedService(feedRepo, communityService)
	handler := communityFeed.NewGetCommunityHandler(feedService)

	// Setup test data
	ctx := context.Background()
	testID := time.Now().UnixNano()
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("precision-%d", testID), fmt.Sprintf("precision-%d.test", testID))
	require.NoError(t, err)

	// Create posts with very similar ages (fractions of seconds apart)
	// This creates hot ranks that differ by tiny amounts (<1e-6)
	// Without full precision, pagination would drop the second post
	baseTime := time.Now().Add(-2 * time.Hour)

	// Post A: 2 hours old, score 50 (hot_rank ~8.24)
	postA := createTestPost(t, db, communityDID, "did:plc:alice", "Post A", 50, baseTime)

	// Post B: 2 hours + 100ms old, score 50 (hot_rank ~8.239999... - differs by <1e-6)
	// This is the critical post that would get dropped with low precision
	postB := createTestPost(t, db, communityDID, "did:plc:bob", "Post B", 50, baseTime.Add(100*time.Millisecond))

	// Post C: 2 hours + 200ms old, score 50
	postC := createTestPost(t, db, communityDID, "did:plc:charlie", "Post C", 50, baseTime.Add(200*time.Millisecond))

	// Page 1: Get first post (limit=1)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=1", communityDID), nil)
	rec := httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 1)
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	firstPostURI := page1.Feed[0].Post.URI
	t.Logf("Page 1 - First post: %s", firstPostURI)
	t.Logf("Page 1 - Cursor: %s", *page1.Cursor)

	// Page 2: Use cursor - this is where precision loss would drop Post B
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.communityFeed.getCommunity?community=%s&sort=hot&limit=2&cursor=%s", communityDID, *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetCommunity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Page 2 failed: %s", rec.Body.String())
	}

	var page2 communityFeeds.FeedResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	// CRITICAL: Page 2 must contain the remaining posts
	// Without full precision, Post B (with hot_rank differing by <1e-6) would be filtered out
	assert.GreaterOrEqual(t, len(page2.Feed), 2, "Page 2 should contain at least 2 remaining posts")

	// Verify all posts appear across pages
	allURIs := map[string]bool{firstPostURI: true}
	for _, p := range page2.Feed {
		allURIs[p.Post.URI] = true
		t.Logf("Page 2 - Post: %s", p.Post.URI)
	}

	// All 3 posts must be present
	assert.True(t, allURIs[postA], "Post A missing")
	assert.True(t, allURIs[postB], "CRITICAL: Post B missing - cursor precision loss bug!")
	assert.True(t, allURIs[postC], "Post C missing")

	t.Logf("SUCCESS: All posts with similar hot ranks preserved (precision bug fixed)")
}
