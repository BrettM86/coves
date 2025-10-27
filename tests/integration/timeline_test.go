package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/timeline"
	"Coves/internal/api/middleware"
	timelineCore "Coves/internal/core/timeline"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetTimeline_Basic tests timeline feed shows posts from subscribed communities
func TestGetTimeline_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create user
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	// Create two communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	// Create a third community that user is NOT subscribed to
	community3DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("cooking-%d", testID), fmt.Sprintf("charlie-%d.test", testID))
	require.NoError(t, err)

	// Subscribe user to community1 and community2 (but not community3)
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3)
	`, userDID, community1DID, community2DID)
	require.NoError(t, err)

	// Create posts in all three communities
	post1URI := createTestPost(t, db, community1DID, "did:plc:alice", "Gaming post 1", 50, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, community2DID, "did:plc:bob", "Tech post 1", 30, time.Now().Add(-2*time.Hour))
	post3URI := createTestPost(t, db, community3DID, "did:plc:charlie", "Cooking post (should not appear)", 100, time.Now().Add(-30*time.Minute))
	post4URI := createTestPost(t, db, community1DID, "did:plc:alice", "Gaming post 2", 20, time.Now().Add(-3*time.Hour))

	// Request timeline with auth
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response timelineCore.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Should show 3 posts (from community1 and community2, NOT community3)
	assert.Len(t, response.Feed, 3, "Timeline should show posts from subscribed communities only")

	// Verify correct posts are shown
	uris := []string{response.Feed[0].Post.URI, response.Feed[1].Post.URI, response.Feed[2].Post.URI}
	assert.Contains(t, uris, post1URI, "Should contain gaming post 1")
	assert.Contains(t, uris, post2URI, "Should contain tech post 1")
	assert.Contains(t, uris, post4URI, "Should contain gaming post 2")
	assert.NotContains(t, uris, post3URI, "Should NOT contain post from unsubscribed community")

	// Verify posts are sorted by creation time (newest first for "new" sort)
	assert.Equal(t, post1URI, response.Feed[0].Post.URI, "Newest post should be first")
	assert.Equal(t, post2URI, response.Feed[1].Post.URI, "Second newest post")
	assert.Equal(t, post4URI, response.Feed[2].Post.URI, "Oldest post should be last")

	// Verify Record field is populated (schema compliance)
	for i, feedPost := range response.Feed {
		assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)
		record, ok := feedPost.Post.Record.(map[string]interface{})
		require.True(t, ok, "Record should be a map")
		assert.Equal(t, "social.coves.post.record", record["$type"], "Record should have correct $type")
		assert.NotEmpty(t, record["community"], "Record should have community")
		assert.NotEmpty(t, record["author"], "Record should have author")
		assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")
	}
}

// TestGetTimeline_HotSort tests hot sorting across multiple communities
func TestGetTimeline_HotSort(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create user
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	// Create communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	// Subscribe to both
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3)
	`, userDID, community1DID, community2DID)
	require.NoError(t, err)

	// Create posts with different scores and ages
	// Recent with medium score from gaming (should rank high)
	createTestPost(t, db, community1DID, "did:plc:alice", "Recent trending gaming", 50, time.Now().Add(-1*time.Hour))

	// Old with high score from tech (age penalty)
	createTestPost(t, db, community2DID, "did:plc:bob", "Old popular tech", 100, time.Now().Add(-24*time.Hour))

	// Very recent with low score from gaming
	createTestPost(t, db, community1DID, "did:plc:charlie", "Brand new gaming", 5, time.Now().Add(-10*time.Minute))

	// Request hot timeline
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=hot&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response timelineCore.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response.Feed, 3, "Timeline should show all posts from subscribed communities")

	// All posts should have community context
	for _, feedPost := range response.Feed {
		assert.NotNil(t, feedPost.Post.Community, "Post should have community context")
		assert.Contains(t, []string{community1DID, community2DID}, feedPost.Post.Community.DID)
	}
}

// TestGetTimeline_Pagination tests cursor-based pagination
func TestGetTimeline_Pagination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create user
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	// Create community
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// Subscribe
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3)
	`, userDID, communityDID)
	require.NoError(t, err)

	// Create 5 posts
	for i := 0; i < 5; i++ {
		createTestPost(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	// First page: limit 2
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=2", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 timelineCore.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 2, "First page should have 2 posts")
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	// Second page: use cursor
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=2&cursor=%s", *page1.Cursor), nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec = httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 timelineCore.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	assert.Len(t, page2.Feed, 2, "Second page should have 2 posts")
	assert.NotNil(t, page2.Cursor, "Should have cursor for next page")

	// Verify no overlap
	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
	assert.NotEqual(t, page1.Feed[1].Post.URI, page2.Feed[1].Post.URI, "Pages should not overlap")
}

// TestGetTimeline_EmptyWhenNoSubscriptions tests timeline is empty when user has no subscriptions
func TestGetTimeline_EmptyWhenNoSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create user (but don't subscribe to any communities)
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	// Request timeline
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response timelineCore.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Empty(t, response.Feed, "Timeline should be empty when user has no subscriptions")
	assert.Nil(t, response.Cursor, "Should not have cursor when no results")
}

// TestGetTimeline_Unauthorized tests timeline requires authentication
func TestGetTimeline_Unauthorized(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	// Request timeline WITHOUT auth context
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	// Should return 401 Unauthorized
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var errorResp map[string]string
	err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
	require.NoError(t, err)

	assert.Equal(t, "AuthenticationRequired", errorResp["error"])
}

// TestGetTimeline_LimitValidation tests limit parameter validation
func TestGetTimeline_LimitValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create user
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	t.Run("Limit exceeds maximum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=100", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errorResp map[string]string
		err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errorResp["error"])
		assert.Contains(t, errorResp["message"], "limit")
	})
}
