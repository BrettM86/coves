package integration

import (
	"Coves/internal/api/handlers/timeline"
	"Coves/internal/api/middleware"
	"Coves/internal/db/postgres"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	timelineCore "Coves/internal/core/timeline"

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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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
		assert.Equal(t, "social.coves.community.post", record["$type"], "Record should have correct $type")
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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

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

// TestGetTimeline_MultiCommunity_E2E tests the complete multi-community timeline flow
// This is the comprehensive E2E test specified in PRD_ALPHA_GO_LIVE.md (lines 236-246)
//
// Test Coverage:
// - Creates 3+ communities with different posts
// - Subscribes user to all communities
// - Creates posts with varied ages and scores across communities
// - Verifies timeline shows posts from ALL subscribed communities
// - Tests all sorting modes (hot, top, new) across communities
// - Ensures proper aggregation and no cross-contamination
func TestGetTimeline_MultiCommunity_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	timelineService := timelineCore.NewTimelineService(timelineRepo)
	handler := timeline.NewGetTimelineHandler(timelineService, nil)

	ctx := context.Background()
	testID := time.Now().UnixNano()
	userDID := fmt.Sprintf("did:plc:user-%d", testID)

	// Create test user
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
	`, userDID, fmt.Sprintf("testuser-%d.test", testID), "https://bsky.social")
	require.NoError(t, err)

	// Create 4 communities (user will subscribe to 3, not subscribe to 1)
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err, "Failed to create gaming community")

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err, "Failed to create tech community")

	community3DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("music-%d", testID), fmt.Sprintf("charlie-%d.test", testID))
	require.NoError(t, err, "Failed to create music community")

	community4DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("cooking-%d", testID), fmt.Sprintf("dave-%d.test", testID))
	require.NoError(t, err, "Failed to create cooking community (unsubscribed)")

	t.Logf("Created 4 communities: gaming=%s, tech=%s, music=%s, cooking=%s",
		community1DID, community2DID, community3DID, community4DID)

	// Subscribe user to first 3 communities (NOT community4)
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3), ($1, $4, 3)
	`, userDID, community1DID, community2DID, community3DID)
	require.NoError(t, err, "Failed to create subscriptions")

	t.Log("✓ User subscribed to gaming, tech, and music communities")

	// Create posts across all 4 communities with varied ages and scores
	// This tests that timeline correctly:
	// 1. Aggregates posts from multiple subscribed communities
	// 2. Excludes posts from unsubscribed communities
	// 3. Handles different sorting algorithms across community boundaries

	// Gaming community posts (2 posts)
	gamingPost1 := createTestPost(t, db, community1DID, "did:plc:gamer1", "Epic gaming moment", 100, time.Now().Add(-2*time.Hour))
	gamingPost2 := createTestPost(t, db, community1DID, "did:plc:gamer2", "New game release", 75, time.Now().Add(-30*time.Minute))

	// Tech community posts (3 posts)
	techPost1 := createTestPost(t, db, community2DID, "did:plc:dev1", "Golang best practices", 150, time.Now().Add(-4*time.Hour))
	techPost2 := createTestPost(t, db, community2DID, "did:plc:dev2", "atProto deep dive", 200, time.Now().Add(-1*time.Hour))
	techPost3 := createTestPost(t, db, community2DID, "did:plc:dev3", "Docker tips", 50, time.Now().Add(-15*time.Minute))

	// Music community posts (2 posts)
	musicPost1 := createTestPost(t, db, community3DID, "did:plc:artist1", "Album review", 80, time.Now().Add(-3*time.Hour))
	musicPost2 := createTestPost(t, db, community3DID, "did:plc:artist2", "Live concert tonight", 120, time.Now().Add(-10*time.Minute))

	// Cooking community posts (should NOT appear - user not subscribed)
	cookingPost := createTestPost(t, db, community4DID, "did:plc:chef1", "Best pizza recipe", 500, time.Now().Add(-5*time.Minute))

	t.Logf("✓ Created 8 posts: 2 gaming, 3 tech, 2 music, 1 cooking (unsubscribed)")

	// Test 1: NEW sorting - chronological order across communities
	t.Run("NEW sort - chronological across all subscribed communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have exactly 7 posts (excluding cooking community)
		assert.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		// Verify chronological order (newest first)
		expectedOrder := []string{
			musicPost2,  // 10 minutes ago
			techPost3,   // 15 minutes ago
			gamingPost2, // 30 minutes ago
			techPost2,   // 1 hour ago
			gamingPost1, // 2 hours ago
			musicPost1,  // 3 hours ago
			techPost1,   // 4 hours ago
		}

		for i, expectedURI := range expectedOrder {
			assert.Equal(t, expectedURI, response.Feed[i].Post.URI,
				"Post %d should be %s in chronological order", i, expectedURI)
		}

		// Verify cooking post is NOT present
		for _, feedPost := range response.Feed {
			assert.NotEqual(t, cookingPost, feedPost.Post.URI,
				"Cooking post from unsubscribed community should NOT appear")
		}

		// Verify each post has community context from the correct community
		communityCountsByDID := make(map[string]int)
		for _, feedPost := range response.Feed {
			require.NotNil(t, feedPost.Post.Community, "Post should have community context")
			communityCountsByDID[feedPost.Post.Community.DID]++
		}

		assert.Equal(t, 2, communityCountsByDID[community1DID], "Should have 2 gaming posts")
		assert.Equal(t, 3, communityCountsByDID[community2DID], "Should have 3 tech posts")
		assert.Equal(t, 2, communityCountsByDID[community3DID], "Should have 2 music posts")
		assert.Equal(t, 0, communityCountsByDID[community4DID], "Should have 0 cooking posts")

		t.Log("✓ NEW sort works correctly across multiple communities")
	})

	// Test 2: HOT sorting - balances recency and score across communities
	t.Run("HOT sort - recency+score algorithm across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=hot&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should still have exactly 7 posts
		assert.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		// Hot algorithm should rank recent high-scoring posts higher
		// techPost2: 1 hour old, score 200 - should rank very high
		// musicPost2: 10 minutes old, score 120 - should rank high (recent + good score)
		// gamingPost1: 2 hours old, score 100 - should rank medium
		// techPost1: 4 hours old, score 150 - age penalty

		// Verify top post is one of the high hot-rank posts
		topPostURIs := []string{musicPost2, techPost2, gamingPost2}
		assert.Contains(t, topPostURIs, response.Feed[0].Post.URI,
			"Top post should be one of the recent high-scoring posts")

		// Verify all posts are from subscribed communities
		for _, feedPost := range response.Feed {
			assert.Contains(t, []string{community1DID, community2DID, community3DID},
				feedPost.Post.Community.DID,
				"All posts should be from subscribed communities")
			assert.NotEqual(t, cookingPost, feedPost.Post.URI,
				"Cooking post should NOT appear")
		}

		t.Log("✓ HOT sort works correctly across multiple communities")
	})

	// Test 3: TOP sorting with timeframe - highest scores across communities
	t.Run("TOP sort - highest scores across all communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=top&timeframe=all&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should still have exactly 7 posts
		assert.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		// Verify top-ranked posts by score (highest first)
		// techPost2: 200 score
		// techPost1: 150 score
		// musicPost2: 120 score
		// gamingPost1: 100 score
		// musicPost1: 80 score
		// gamingPost2: 75 score
		// techPost3: 50 score

		assert.Equal(t, techPost2, response.Feed[0].Post.URI, "Top post should be techPost2 (score 200)")
		assert.Equal(t, techPost1, response.Feed[1].Post.URI, "Second post should be techPost1 (score 150)")
		assert.Equal(t, musicPost2, response.Feed[2].Post.URI, "Third post should be musicPost2 (score 120)")

		// Verify scores are descending
		for i := 0; i < len(response.Feed)-1; i++ {
			currentScore := response.Feed[i].Post.Stats.Score
			nextScore := response.Feed[i+1].Post.Stats.Score
			assert.GreaterOrEqual(t, currentScore, nextScore,
				"Scores should be in descending order (post %d score=%d, post %d score=%d)",
				i, currentScore, i+1, nextScore)
		}

		// Verify cooking post is NOT present (even though it has highest score)
		for _, feedPost := range response.Feed {
			assert.NotEqual(t, cookingPost, feedPost.Post.URI,
				"Cooking post should NOT appear even with high score")
		}

		t.Log("✓ TOP sort works correctly across multiple communities")
	})

	// Test 4: TOP with day timeframe - filters old posts
	t.Run("TOP sort with day timeframe - filters across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=top&timeframe=day&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// All our test posts are within the last day, so should have all 7
		assert.Len(t, response.Feed, 7, "All posts are within last day")

		// Verify all posts are within last 24 hours
		dayAgo := time.Now().Add(-24 * time.Hour)
		for _, feedPost := range response.Feed {
			postTime := feedPost.Post.IndexedAt
			assert.True(t, postTime.After(dayAgo),
				"Post should be within last 24 hours")
		}

		t.Log("✓ TOP sort with timeframe works correctly across multiple communities")
	})

	// Test 5: Pagination works across multiple communities
	t.Run("Pagination across multiple communities", func(t *testing.T) {
		// First page: limit 3
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=3", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page1 timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &page1)
		require.NoError(t, err)

		assert.Len(t, page1.Feed, 3, "First page should have 3 posts")
		assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

		// Second page
		req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=3&cursor=%s", *page1.Cursor), nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec = httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page2 timelineCore.TimelineResponse
		err = json.Unmarshal(rec.Body.Bytes(), &page2)
		require.NoError(t, err)

		assert.Len(t, page2.Feed, 3, "Second page should have 3 posts")
		assert.NotNil(t, page2.Cursor, "Should have cursor for third page")

		// Verify no overlap between pages
		page1URIs := make(map[string]bool)
		for _, p := range page1.Feed {
			page1URIs[p.Post.URI] = true
		}
		for _, p := range page2.Feed {
			assert.False(t, page1URIs[p.Post.URI], "Pages should not overlap")
		}

		// Third page (remaining post)
		req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=3&cursor=%s", *page2.Cursor), nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec = httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page3 timelineCore.TimelineResponse
		err = json.Unmarshal(rec.Body.Bytes(), &page3)
		require.NoError(t, err)

		assert.Len(t, page3.Feed, 1, "Third page should have 1 remaining post")
		assert.Nil(t, page3.Cursor, "Should not have cursor on last page")

		t.Log("✓ Pagination works correctly across multiple communities")
	})

	// Test 6: Verify post record schema compliance across communities
	t.Run("Record schema compliance across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timelineCore.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Verify every post has proper Record structure
		for i, feedPost := range response.Feed {
			assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)

			record, ok := feedPost.Post.Record.(map[string]interface{})
			require.True(t, ok, "Record should be a map")

			assert.Equal(t, "social.coves.community.post", record["$type"],
				"Record should have correct $type")
			assert.NotEmpty(t, record["community"], "Record should have community")
			assert.NotEmpty(t, record["author"], "Record should have author")
			assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")

			// Verify community reference
			assert.NotNil(t, feedPost.Post.Community, "Post should have community reference")
			assert.NotEmpty(t, feedPost.Post.Community.DID, "Community should have DID")
			assert.NotEmpty(t, feedPost.Post.Community.Handle, "Community should have handle")
			assert.NotEmpty(t, feedPost.Post.Community.Name, "Community should have name")

			// Verify community DID matches one of our subscribed communities
			assert.Contains(t, []string{community1DID, community2DID, community3DID},
				feedPost.Post.Community.DID,
				"Post should be from one of the subscribed communities")
		}

		t.Log("✓ All posts have proper record schema and community references")
	})

	t.Log("\n✅ Multi-Community Timeline E2E Test Complete!")
	t.Log("Summary:")
	t.Log("  ✓ Created 4 communities (3 subscribed, 1 unsubscribed)")
	t.Log("  ✓ Created 8 posts across communities (7 in subscribed, 1 in unsubscribed)")
	t.Log("  ✓ NEW sort: Chronological order across all subscribed communities")
	t.Log("  ✓ HOT sort: Recency+score algorithm works across communities")
	t.Log("  ✓ TOP sort: Highest scores across communities (with timeframe filtering)")
	t.Log("  ✓ Pagination: Works correctly across community boundaries")
	t.Log("  ✓ Schema: All posts have proper record structure and community refs")
	t.Log("  ✓ Security: Unsubscribed community posts correctly excluded")
}
