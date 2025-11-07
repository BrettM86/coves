package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/discover"
	"Coves/internal/db/postgres"

	discoverCore "Coves/internal/core/discover"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetDiscover_ShowsAllCommunities tests discover feed shows posts from ALL communities
func TestGetDiscover_ShowsAllCommunities(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create three communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	community3DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("cooking-%d", testID), fmt.Sprintf("charlie-%d.test", testID))
	require.NoError(t, err)

	// Create posts in all three communities
	post1URI := createTestPost(t, db, community1DID, "did:plc:alice", "Gaming post", 50, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, community2DID, "did:plc:bob", "Tech post", 30, time.Now().Add(-2*time.Hour))
	post3URI := createTestPost(t, db, community3DID, "did:plc:charlie", "Cooking post", 100, time.Now().Add(-30*time.Minute))

	// Request discover feed (no auth required!)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify all our posts are present (may include posts from other tests)
	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain gaming post")
	assert.True(t, uris[post2URI], "Should contain tech post")
	assert.True(t, uris[post3URI], "Should contain cooking post")

	// Verify newest post appears before older posts in the feed
	var post3Index, post1Index, post2Index int = -1, -1, -1
	for i, post := range response.Feed {
		switch post.Post.URI {
		case post3URI:
			post3Index = i
		case post1URI:
			post1Index = i
		case post2URI:
			post2Index = i
		}
	}
	if post3Index >= 0 && post1Index >= 0 && post2Index >= 0 {
		assert.Less(t, post3Index, post1Index, "Newest post (30min ago) should appear before 1hr old post")
		assert.Less(t, post1Index, post2Index, "1hr old post should appear before 2hr old post")
	}
}

// TestGetDiscover_NoAuthRequired tests discover feed works without authentication
func TestGetDiscover_NoAuthRequired(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community and post
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("public-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	postURI := createTestPost(t, db, communityDID, "did:plc:alice", "Public post", 10, time.Now())

	// Request discover WITHOUT any authentication
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	// Note: No auth context set!
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Should succeed without auth
	assert.Equal(t, http.StatusOK, rec.Code, "Discover should work without authentication")

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify our post is present
	found := false
	for _, post := range response.Feed {
		if post.Post.URI == postURI {
			found = true
			break
		}
	}
	assert.True(t, found, "Should show post even without authentication")
}

// TestGetDiscover_HotSort tests hot sorting across all communities
func TestGetDiscover_HotSort(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	// Create posts with different scores/ages
	post1URI := createTestPost(t, db, community1DID, "did:plc:alice", "Recent trending", 50, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, community2DID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	post3URI := createTestPost(t, db, community1DID, "did:plc:charlie", "Brand new", 5, time.Now().Add(-10*time.Minute))

	// Request hot discover
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify all our posts are present
	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain recent trending post")
	assert.True(t, uris[post2URI], "Should contain old popular post")
	assert.True(t, uris[post3URI], "Should contain brand new post")
}

// TestGetDiscover_Pagination tests cursor-based pagination
func TestGetDiscover_Pagination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("test-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// Create 5 posts
	for i := 0; i < 5; i++ {
		createTestPost(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	// First page: limit 2
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=2", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 2, "First page should have 2 posts")
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	// Second page: use cursor
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getDiscover?sort=new&limit=2&cursor=%s", *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	assert.Len(t, page2.Feed, 2, "Second page should have 2 posts")

	// Verify no overlap
	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
}

// TestGetDiscover_LimitValidation tests limit parameter validation
func TestGetDiscover_LimitValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService)

	t.Run("Limit exceeds maximum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=100", nil)
		rec := httptest.NewRecorder()
		handler.HandleGetDiscover(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errorResp map[string]string
		err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errorResp["error"])
		assert.Contains(t, errorResp["message"], "limit")
	})
}
