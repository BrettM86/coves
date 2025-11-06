package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/db/postgres"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommentQuery_BasicFetch tests fetching top-level comments with default params
func TestCommentQuery_BasicFetch(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "basicfetch.test", "did:plc:basicfetch123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "basicfetchcomm", "ownerbasic.test")
	require.NoError(t, err, "Failed to create test community")

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Basic Fetch Test Post", 0, time.Now())

	// Create 3 top-level comments with different scores and ages
	comment1 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "First comment", 10, 2, time.Now().Add(-2*time.Hour))
	comment2 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Second comment", 5, 1, time.Now().Add(-30*time.Minute))
	comment3 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Third comment", 3, 0, time.Now().Add(-5*time.Minute))

	// Fetch comments with default params (hot sort)
	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "hot",
		Depth:   10,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err, "GetComments should not return error")
	require.NotNil(t, resp, "Response should not be nil")

	// Verify all 3 comments returned
	assert.Len(t, resp.Comments, 3, "Should return all 3 top-level comments")

	// Verify stats are correct
	for _, threadView := range resp.Comments {
		commentView := threadView.Comment
		assert.NotNil(t, commentView.Stats, "Stats should not be nil")

		// Verify upvotes, downvotes, score, reply count present
		assert.GreaterOrEqual(t, commentView.Stats.Upvotes, 0, "Upvotes should be non-negative")
		assert.GreaterOrEqual(t, commentView.Stats.Downvotes, 0, "Downvotes should be non-negative")
		assert.Equal(t, 0, commentView.Stats.ReplyCount, "Top-level comments should have 0 replies")
	}

	// Verify URIs match
	commentURIs := []string{comment1, comment2, comment3}
	returnedURIs := make(map[string]bool)
	for _, tv := range resp.Comments {
		returnedURIs[tv.Comment.URI] = true
	}

	for _, uri := range commentURIs {
		assert.True(t, returnedURIs[uri], "Comment URI %s should be in results", uri)
	}
}

// TestCommentQuery_NestedReplies tests fetching comments with nested reply structure
func TestCommentQuery_NestedReplies(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "nested.test", "did:plc:nested123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "nestedcomm", "ownernested.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Nested Test Post", 0, time.Now())

	// Create nested structure:
	// Post
	//  |- Comment A (top-level)
	//      |- Reply A1
	//          |- Reply A1a
	//          |- Reply A1b
	//      |- Reply A2
	//  |- Comment B (top-level)

	commentA := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Comment A", 5, 0, time.Now().Add(-1*time.Hour))
	replyA1 := createTestCommentWithScore(t, db, testUser.DID, postURI, commentA, "Reply A1", 3, 0, time.Now().Add(-50*time.Minute))
	replyA1a := createTestCommentWithScore(t, db, testUser.DID, postURI, replyA1, "Reply A1a", 2, 0, time.Now().Add(-40*time.Minute))
	replyA1b := createTestCommentWithScore(t, db, testUser.DID, postURI, replyA1, "Reply A1b", 1, 0, time.Now().Add(-30*time.Minute))
	replyA2 := createTestCommentWithScore(t, db, testUser.DID, postURI, commentA, "Reply A2", 2, 0, time.Now().Add(-20*time.Minute))
	commentB := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Comment B", 4, 0, time.Now().Add(-10*time.Minute))

	// Fetch with depth=2 (should get 2 levels of nesting)
	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "new",
		Depth:   2,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err)
	require.Len(t, resp.Comments, 2, "Should return 2 top-level comments")

	// Find Comment A in results
	var commentAThread *comments.ThreadViewComment
	for _, tv := range resp.Comments {
		if tv.Comment.URI == commentA {
			commentAThread = tv
			break
		}
	}
	require.NotNil(t, commentAThread, "Comment A should be in results")

	// Verify Comment A has replies
	require.NotNil(t, commentAThread.Replies, "Comment A should have replies")
	assert.Len(t, commentAThread.Replies, 2, "Comment A should have 2 direct replies (A1 and A2)")

	// Find Reply A1
	var replyA1Thread *comments.ThreadViewComment
	for _, reply := range commentAThread.Replies {
		if reply.Comment.URI == replyA1 {
			replyA1Thread = reply
			break
		}
	}
	require.NotNil(t, replyA1Thread, "Reply A1 should be in results")

	// Verify Reply A1 has nested replies (at depth 2)
	require.NotNil(t, replyA1Thread.Replies, "Reply A1 should have nested replies at depth 2")
	assert.Len(t, replyA1Thread.Replies, 2, "Reply A1 should have 2 nested replies (A1a and A1b)")

	// Verify reply URIs
	replyURIs := make(map[string]bool)
	for _, r := range replyA1Thread.Replies {
		replyURIs[r.Comment.URI] = true
	}
	assert.True(t, replyURIs[replyA1a], "Reply A1a should be present")
	assert.True(t, replyURIs[replyA1b], "Reply A1b should be present")

	// Verify no deeper nesting (depth limit enforced)
	for _, r := range replyA1Thread.Replies {
		assert.Nil(t, r.Replies, "Replies at depth 2 should not have further nesting")
	}

	_ = commentB
	_ = replyA2
}

// TestCommentQuery_DepthLimit tests depth limiting works correctly
func TestCommentQuery_DepthLimit(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "depth.test", "did:plc:depth123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "depthcomm", "ownerdepth.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Depth Test Post", 0, time.Now())

	// Create deeply nested thread (5 levels)
	// Post -> C1 -> C2 -> C3 -> C4 -> C5
	c1 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Level 1", 5, 0, time.Now().Add(-5*time.Minute))
	c2 := createTestCommentWithScore(t, db, testUser.DID, postURI, c1, "Level 2", 4, 0, time.Now().Add(-4*time.Minute))
	c3 := createTestCommentWithScore(t, db, testUser.DID, postURI, c2, "Level 3", 3, 0, time.Now().Add(-3*time.Minute))
	c4 := createTestCommentWithScore(t, db, testUser.DID, postURI, c3, "Level 4", 2, 0, time.Now().Add(-2*time.Minute))
	c5 := createTestCommentWithScore(t, db, testUser.DID, postURI, c4, "Level 5", 1, 0, time.Now().Add(-1*time.Minute))

	t.Run("Depth 0 returns flat list", func(t *testing.T) {
		service := setupCommentService(db)
		req := &comments.GetCommentsRequest{
			PostURI: postURI,
			Sort:    "new",
			Depth:   0,
			Limit:   50,
		}

		resp, err := service.GetComments(ctx, req)
		require.NoError(t, err)
		require.Len(t, resp.Comments, 1, "Should return 1 top-level comment")

		// Verify no replies included
		assert.Nil(t, resp.Comments[0].Replies, "Depth 0 should not include replies")

		// Verify HasMore flag is set (c1 has replies)
		assert.True(t, resp.Comments[0].HasMore, "HasMore should be true when replies exist but depth=0")
	})

	t.Run("Depth 3 returns exactly 3 levels", func(t *testing.T) {
		service := setupCommentService(db)
		req := &comments.GetCommentsRequest{
			PostURI: postURI,
			Sort:    "new",
			Depth:   3,
			Limit:   50,
		}

		resp, err := service.GetComments(ctx, req)
		require.NoError(t, err)
		require.Len(t, resp.Comments, 1, "Should return 1 top-level comment")

		// Traverse and verify exactly 3 levels
		level1 := resp.Comments[0]
		require.NotNil(t, level1.Replies, "Level 1 should have replies")
		require.Len(t, level1.Replies, 1, "Level 1 should have 1 reply")

		level2 := level1.Replies[0]
		require.NotNil(t, level2.Replies, "Level 2 should have replies")
		require.Len(t, level2.Replies, 1, "Level 2 should have 1 reply")

		level3 := level2.Replies[0]
		require.NotNil(t, level3.Replies, "Level 3 should have replies")
		require.Len(t, level3.Replies, 1, "Level 3 should have 1 reply")

		// Level 4 should NOT have replies (depth limit)
		level4 := level3.Replies[0]
		assert.Nil(t, level4.Replies, "Level 4 should not have replies (depth limit)")

		// Verify HasMore is set correctly at depth boundary
		assert.True(t, level4.HasMore, "HasMore should be true at depth boundary when more replies exist")
	})

	_ = c2
	_ = c3
	_ = c4
	_ = c5
}

// TestCommentQuery_HotSorting tests hot sorting with Lemmy algorithm
func TestCommentQuery_HotSorting(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "hot.test", "did:plc:hot123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "hotcomm", "ownerhot.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Hot Sorting Test", 0, time.Now())

	// Create 3 comments with different scores and ages
	// Comment 1: score=10, created 1 hour ago
	c1 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Old high score", 10, 0, time.Now().Add(-1*time.Hour))

	// Comment 2: score=5, created 5 minutes ago (should rank higher due to recency)
	c2 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Recent medium score", 5, 0, time.Now().Add(-5*time.Minute))

	// Comment 3: score=-2, created now (negative score should rank lower)
	c3 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Negative score", 0, 2, time.Now())

	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "hot",
		Depth:   0,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err)
	require.Len(t, resp.Comments, 3, "Should return all 3 comments")

	// Verify hot sorting order
	// Recent comment with medium score should rank higher than old comment with high score
	assert.Equal(t, c2, resp.Comments[0].Comment.URI, "Recent medium score should rank first")
	assert.Equal(t, c1, resp.Comments[1].Comment.URI, "Old high score should rank second")
	assert.Equal(t, c3, resp.Comments[2].Comment.URI, "Negative score should rank last")

	// Verify negative scores are handled gracefully
	negativeComment := resp.Comments[2].Comment
	assert.Equal(t, -2, negativeComment.Stats.Score, "Negative score should be preserved")
	assert.Equal(t, 0, negativeComment.Stats.Upvotes, "Upvotes should be 0")
	assert.Equal(t, 2, negativeComment.Stats.Downvotes, "Downvotes should be 2")
}

// TestCommentQuery_TopSorting tests top sorting with score-based ordering
func TestCommentQuery_TopSorting(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "top.test", "did:plc:top123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "topcomm", "ownertop.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Top Sorting Test", 0, time.Now())

	// Create comments with different scores
	c1 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Low score", 2, 0, time.Now().Add(-30*time.Minute))
	c2 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "High score", 10, 0, time.Now().Add(-1*time.Hour))
	c3 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Medium score", 5, 0, time.Now().Add(-15*time.Minute))

	t.Run("Top sort without timeframe", func(t *testing.T) {
		service := setupCommentService(db)
		req := &comments.GetCommentsRequest{
			PostURI: postURI,
			Sort:    "top",
			Depth:   0,
			Limit:   50,
		}

		resp, err := service.GetComments(ctx, req)
		require.NoError(t, err)
		require.Len(t, resp.Comments, 3)

		// Verify highest score first
		assert.Equal(t, c2, resp.Comments[0].Comment.URI, "Highest score should be first")
		assert.Equal(t, c3, resp.Comments[1].Comment.URI, "Medium score should be second")
		assert.Equal(t, c1, resp.Comments[2].Comment.URI, "Low score should be third")
	})

	t.Run("Top sort with hour timeframe", func(t *testing.T) {
		service := setupCommentService(db)
		req := &comments.GetCommentsRequest{
			PostURI:   postURI,
			Sort:      "top",
			Timeframe: "hour",
			Depth:     0,
			Limit:     50,
		}

		resp, err := service.GetComments(ctx, req)
		require.NoError(t, err)

		// Only comments from last hour should be included (c1 and c3, not c2)
		assert.LessOrEqual(t, len(resp.Comments), 2, "Should exclude comments older than 1 hour")

		// Verify c2 (created 1 hour ago) is excluded
		for _, tv := range resp.Comments {
			assert.NotEqual(t, c2, tv.Comment.URI, "Comment older than 1 hour should be excluded")
		}
	})
}

// TestCommentQuery_NewSorting tests chronological sorting
func TestCommentQuery_NewSorting(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "new.test", "did:plc:new123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "newcomm", "ownernew.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "New Sorting Test", 0, time.Now())

	// Create comments at different times (different scores to verify time is priority)
	c1 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Oldest", 10, 0, time.Now().Add(-1*time.Hour))
	c2 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Middle", 5, 0, time.Now().Add(-30*time.Minute))
	c3 := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Newest", 2, 0, time.Now().Add(-5*time.Minute))

	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "new",
		Depth:   0,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err)
	require.Len(t, resp.Comments, 3)

	// Verify chronological order (newest first)
	assert.Equal(t, c3, resp.Comments[0].Comment.URI, "Newest comment should be first")
	assert.Equal(t, c2, resp.Comments[1].Comment.URI, "Middle comment should be second")
	assert.Equal(t, c1, resp.Comments[2].Comment.URI, "Oldest comment should be third")
}

// TestCommentQuery_Pagination tests cursor-based pagination
func TestCommentQuery_Pagination(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "page.test", "did:plc:page123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "pagecomm", "ownerpage.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Pagination Test", 0, time.Now())

	// Create 60 comments
	allCommentURIs := make([]string, 60)
	for i := 0; i < 60; i++ {
		uri := createTestCommentWithScore(t, db, testUser.DID, postURI, postURI,
			fmt.Sprintf("Comment %d", i), i, 0, time.Now().Add(-time.Duration(60-i)*time.Minute))
		allCommentURIs[i] = uri
	}

	service := setupCommentService(db)

	// Fetch first page (limit=50)
	req1 := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "new",
		Depth:   0,
		Limit:   50,
	}

	resp1, err := service.GetComments(ctx, req1)
	require.NoError(t, err)
	assert.Len(t, resp1.Comments, 50, "First page should have 50 comments")
	require.NotNil(t, resp1.Cursor, "Cursor should be present for next page")

	// Fetch second page with cursor
	req2 := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "new",
		Depth:   0,
		Limit:   50,
		Cursor:  resp1.Cursor,
	}

	resp2, err := service.GetComments(ctx, req2)
	require.NoError(t, err)
	assert.Len(t, resp2.Comments, 10, "Second page should have remaining 10 comments")
	assert.Nil(t, resp2.Cursor, "Cursor should be nil on last page")

	// Verify no duplicates between pages
	page1URIs := make(map[string]bool)
	for _, tv := range resp1.Comments {
		page1URIs[tv.Comment.URI] = true
	}

	for _, tv := range resp2.Comments {
		assert.False(t, page1URIs[tv.Comment.URI], "Comment %s should not appear in both pages", tv.Comment.URI)
	}

	// Verify all comments eventually retrieved
	allRetrieved := make(map[string]bool)
	for _, tv := range resp1.Comments {
		allRetrieved[tv.Comment.URI] = true
	}
	for _, tv := range resp2.Comments {
		allRetrieved[tv.Comment.URI] = true
	}
	assert.Len(t, allRetrieved, 60, "All 60 comments should be retrieved across pages")
}

// TestCommentQuery_EmptyThread tests fetching comments from a post with no comments
func TestCommentQuery_EmptyThread(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "empty.test", "did:plc:empty123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "emptycomm", "ownerempty.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Empty Thread Test", 0, time.Now())

	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "hot",
		Depth:   10,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp, "Response should not be nil")

	// Verify empty array (not null)
	assert.NotNil(t, resp.Comments, "Comments array should not be nil")
	assert.Len(t, resp.Comments, 0, "Comments array should be empty")

	// Verify no cursor returned
	assert.Nil(t, resp.Cursor, "Cursor should be nil for empty results")
}

// TestCommentQuery_DeletedComments tests that soft-deleted comments are excluded
func TestCommentQuery_DeletedComments(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	testUser := createTestUser(t, db, "deleted.test", "did:plc:deleted123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "deletedcomm", "ownerdeleted.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Deleted Comments Test", 0, time.Now())

	// Create 5 comments via Jetstream consumer
	commentURIs := make([]string, 5)
	for i := 0; i < 5; i++ {
		rkey := generateTID()
		uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)
		commentURIs[i] = uri

		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        fmt.Sprintf("bafyc%d", i),
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": fmt.Sprintf("Comment %d", i),
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
				},
			},
		}

		require.NoError(t, consumer.HandleEvent(ctx, event))
	}

	// Soft-delete 2 comments (index 1 and 3)
	deleteEvent1 := &jetstream.JetstreamEvent{
		Did:  testUser.DID,
		Kind: "commit",
		Commit: &jetstream.CommitEvent{
			Operation:  "delete",
			Collection: "social.coves.feed.comment",
			RKey:       strings.Split(commentURIs[1], "/")[4],
		},
	}
	require.NoError(t, consumer.HandleEvent(ctx, deleteEvent1))

	deleteEvent2 := &jetstream.JetstreamEvent{
		Did:  testUser.DID,
		Kind: "commit",
		Commit: &jetstream.CommitEvent{
			Operation:  "delete",
			Collection: "social.coves.feed.comment",
			RKey:       strings.Split(commentURIs[3], "/")[4],
		},
	}
	require.NoError(t, consumer.HandleEvent(ctx, deleteEvent2))

	// Fetch comments
	service := setupCommentService(db)
	req := &comments.GetCommentsRequest{
		PostURI: postURI,
		Sort:    "new",
		Depth:   0,
		Limit:   50,
	}

	resp, err := service.GetComments(ctx, req)
	require.NoError(t, err)

	// Verify only 3 comments returned (2 were deleted)
	assert.Len(t, resp.Comments, 3, "Should only return non-deleted comments")

	// Verify deleted comments are not in results
	returnedURIs := make(map[string]bool)
	for _, tv := range resp.Comments {
		returnedURIs[tv.Comment.URI] = true
	}

	assert.False(t, returnedURIs[commentURIs[1]], "Deleted comment 1 should not be in results")
	assert.False(t, returnedURIs[commentURIs[3]], "Deleted comment 3 should not be in results")
	assert.True(t, returnedURIs[commentURIs[0]], "Non-deleted comment 0 should be in results")
	assert.True(t, returnedURIs[commentURIs[2]], "Non-deleted comment 2 should be in results")
	assert.True(t, returnedURIs[commentURIs[4]], "Non-deleted comment 4 should be in results")
}

// TestCommentQuery_InvalidInputs tests error handling for invalid inputs
func TestCommentQuery_InvalidInputs(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	service := setupCommentService(db)

	t.Run("Invalid post URI", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "not-an-at-uri",
			Sort:    "hot",
			Depth:   10,
			Limit:   50,
		}

		_, err := service.GetComments(ctx, req)
		assert.Error(t, err, "Should return error for invalid AT-URI")
		assert.Contains(t, err.Error(), "invalid", "Error should mention invalid")
	})

	t.Run("Negative depth", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "at://did:plc:test/social.coves.feed.post/abc123",
			Sort:    "hot",
			Depth:   -5,
			Limit:   50,
		}

		resp, err := service.GetComments(ctx, req)
		// Should not error, but should clamp to default (10)
		require.NoError(t, err)
		// Depth is normalized in validation
		_ = resp
	})

	t.Run("Depth exceeds max", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "at://did:plc:test/social.coves.feed.post/abc123",
			Sort:    "hot",
			Depth:   150, // Exceeds max of 100
			Limit:   50,
		}

		resp, err := service.GetComments(ctx, req)
		// Should not error, but should clamp to 100
		require.NoError(t, err)
		_ = resp
	})

	t.Run("Limit exceeds max", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "at://did:plc:test/social.coves.feed.post/abc123",
			Sort:    "hot",
			Depth:   10,
			Limit:   150, // Exceeds max of 100
		}

		resp, err := service.GetComments(ctx, req)
		// Should not error, but should clamp to 100
		require.NoError(t, err)
		_ = resp
	})

	t.Run("Invalid sort", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "at://did:plc:test/social.coves.feed.post/abc123",
			Sort:    "invalid",
			Depth:   10,
			Limit:   50,
		}

		_, err := service.GetComments(ctx, req)
		assert.Error(t, err, "Should return error for invalid sort")
		assert.Contains(t, err.Error(), "invalid sort", "Error should mention invalid sort")
	})

	t.Run("Empty post URI", func(t *testing.T) {
		req := &comments.GetCommentsRequest{
			PostURI: "",
			Sort:    "hot",
			Depth:   10,
			Limit:   50,
		}

		_, err := service.GetComments(ctx, req)
		assert.Error(t, err, "Should return error for empty post URI")
	})
}

// TestCommentQuery_HTTPHandler tests the HTTP handler end-to-end
func TestCommentQuery_HTTPHandler(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	testUser := createTestUser(t, db, "http.test", "did:plc:http123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "httpcomm", "ownerhttp.test")
	require.NoError(t, err)

	postURI := createTestPost(t, db, testCommunity, testUser.DID, "HTTP Handler Test", 0, time.Now())

	// Create test comments
	createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Test comment 1", 5, 0, time.Now().Add(-30*time.Minute))
	createTestCommentWithScore(t, db, testUser.DID, postURI, postURI, "Test comment 2", 3, 0, time.Now().Add(-15*time.Minute))

	// Setup service adapter for HTTP handler
	service := setupCommentServiceAdapter(db)
	handler := &testGetCommentsHandler{service: service}

	t.Run("Valid GET request", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/xrpc/social.coves.feed.getComments?post=%s&sort=hot&depth=10&limit=50", postURI), nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

		var resp comments.GetCommentsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Len(t, resp.Comments, 2, "Should return 2 comments")
	})

	t.Run("Missing post parameter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.feed.getComments?sort=hot", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Invalid depth parameter", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/xrpc/social.coves.feed.getComments?post=%s&depth=invalid", postURI), nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// Helper: setupCommentService creates a comment service for testing
func setupCommentService(db *sql.DB) comments.Service {
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	return comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo)
}

// Helper: createTestCommentWithScore creates a comment with specific vote counts
func createTestCommentWithScore(t *testing.T, db *sql.DB, commenterDID, rootURI, parentURI, content string, upvotes, downvotes int, createdAt time.Time) string {
	t.Helper()

	ctx := context.Background()
	rkey := generateTID()
	uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", commenterDID, rkey)

	// Insert comment directly for speed
	_, err := db.ExecContext(ctx, `
		INSERT INTO comments (
			uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, created_at, indexed_at,
			upvote_count, downvote_count, score
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, NOW(),
			$11, $12, $13
		)
	`, uri, fmt.Sprintf("bafyc%s", rkey), rkey, commenterDID,
		rootURI, "bafyroot", parentURI, "bafyparent",
		content, createdAt,
		upvotes, downvotes, upvotes-downvotes)

	require.NoError(t, err, "Failed to create test comment")

	// Update reply count on parent if it's a nested comment
	if parentURI != rootURI {
		_, _ = db.ExecContext(ctx, `
			UPDATE comments
			SET reply_count = reply_count + 1
			WHERE uri = $1
		`, parentURI)
	} else {
		// Update comment count on post if top-level
		_, _ = db.ExecContext(ctx, `
			UPDATE posts
			SET comment_count = comment_count + 1
			WHERE uri = $1
		`, parentURI)
	}

	return uri
}

// Helper: Service adapter for HTTP handler testing
type testCommentServiceAdapter struct {
	service comments.Service
}

func (s *testCommentServiceAdapter) GetComments(r *http.Request, req *testGetCommentsRequest) (*comments.GetCommentsResponse, error) {
	ctx := r.Context()

	serviceReq := &comments.GetCommentsRequest{
		PostURI:   req.PostURI,
		Sort:      req.Sort,
		Timeframe: req.Timeframe,
		Depth:     req.Depth,
		Limit:     req.Limit,
		Cursor:    req.Cursor,
		ViewerDID: req.ViewerDID,
	}

	return s.service.GetComments(ctx, serviceReq)
}

type testGetCommentsRequest struct {
	PostURI   string
	Sort      string
	Timeframe string
	Depth     int
	Limit     int
	Cursor    *string
	ViewerDID *string
}

func setupCommentServiceAdapter(db *sql.DB) *testCommentServiceAdapter {
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	service := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo)
	return &testCommentServiceAdapter{service: service}
}

// Helper: Simple HTTP handler wrapper for testing
type testGetCommentsHandler struct {
	service *testCommentServiceAdapter
}

func (h *testGetCommentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	post := query.Get("post")

	if post == "" {
		http.Error(w, "post parameter is required", http.StatusBadRequest)
		return
	}

	sort := query.Get("sort")
	if sort == "" {
		sort = "hot"
	}

	depth := 10
	if d := query.Get("depth"); d != "" {
		if _, err := fmt.Sscanf(d, "%d", &depth); err != nil {
			http.Error(w, "invalid depth", http.StatusBadRequest)
			return
		}
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if _, err := fmt.Sscanf(l, "%d", &limit); err != nil {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
	}

	req := &testGetCommentsRequest{
		PostURI: post,
		Sort:    sort,
		Depth:   depth,
		Limit:   limit,
	}

	resp, err := h.service.GetComments(r, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
