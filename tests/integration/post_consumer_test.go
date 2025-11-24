package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
)

// TestPostConsumer_CommentCountReconciliation tests that post comment_count
// is correctly reconciled when comments arrive before the parent post.
//
// This addresses the issue identified in comment_consumer.go:362 where the FIXME
// comment suggests reconciliation is not implemented. This test verifies that
// the reconciliation logic in post_consumer.go:210-226 works correctly.
func TestPostConsumer_CommentCountReconciliation(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()

	// Set up repositories and consumers
	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, nil, getTestPDSURL())

	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
	postConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

	// Setup test data
	testUser := createTestUser(t, db, "reconcile.test", "did:plc:reconcile123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "reconcile-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	t.Run("Single comment arrives before post - count reconciled", func(t *testing.T) {
		// Scenario: User creates a post
		//           Another user creates a comment on that post
		//           Due to Jetstream ordering, comment event arrives BEFORE post event
		//           When post is finally indexed, comment_count should be 1, not 0

		postRkey := generateTID()
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", testCommunity, postRkey)

		commentRkey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRkey)

		// Step 1: Index comment FIRST (before parent post exists)
		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "comment-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRkey,
				CID:        "bafycomment",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment arriving before parent post!",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI, // Points to post that doesn't exist yet
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := commentConsumer.HandleEvent(ctx, commentEvent)
		if err != nil {
			t.Fatalf("Failed to handle comment event: %v", err)
		}

		// Verify comment was indexed
		comment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Comment not indexed: %v", err)
		}
		if comment.ParentURI != postURI {
			t.Errorf("Expected comment parent_uri %s, got %s", postURI, comment.ParentURI)
		}

		// Step 2: Now index post (arrives late due to Jetstream ordering)
		postEvent := &jetstream.JetstreamEvent{
			Did:  testCommunity,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "post-rev",
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       postRkey,
				CID:        "bafypost",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": testCommunity,
					"author":    testUser.DID,
					"title":     "Post arriving after comment",
					"content":   "This post's comment arrived first!",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = postConsumer.HandleEvent(ctx, postEvent)
		if err != nil {
			t.Fatalf("Failed to handle post event: %v", err)
		}

		// Step 3: Verify post was indexed with CORRECT comment_count
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Post not indexed: %v", err)
		}

		// THIS IS THE KEY TEST: Post should have comment_count = 1 due to reconciliation
		if post.CommentCount != 1 {
			t.Errorf("Expected post comment_count to be 1 (reconciled), got %d", post.CommentCount)
			t.Logf("This indicates the reconciliation logic in post_consumer.go is not working!")
			t.Logf("The FIXME comment at comment_consumer.go:362 may still be valid.")
		}

		// Verify via direct query as well
		var dbCommentCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", postURI).Scan(&dbCommentCount)
		if err != nil {
			t.Fatalf("Failed to query post comment_count: %v", err)
		}
		if dbCommentCount != 1 {
			t.Errorf("Expected DB comment_count to be 1, got %d", dbCommentCount)
		}
	})

	t.Run("Multiple comments arrive before post - count reconciled to correct total", func(t *testing.T) {
		postRkey := generateTID()
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", testCommunity, postRkey)

		// Step 1: Index 3 comments BEFORE the post exists
		for i := 1; i <= 3; i++ {
			commentRkey := generateTID()
			commentEvent := &jetstream.JetstreamEvent{
				Did:  testUser.DID,
				Kind: "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        fmt.Sprintf("comment-%d-rev", i),
					Operation:  "create",
					Collection: "social.coves.community.comment",
					RKey:       commentRkey,
					CID:        fmt.Sprintf("bafycomment%d", i),
					Record: map[string]interface{}{
						"$type":   "social.coves.community.comment",
						"content": fmt.Sprintf("Comment %d before post", i),
						"reply": map[string]interface{}{
							"root": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost2",
							},
							"parent": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost2",
							},
						},
						"createdAt": time.Now().Format(time.RFC3339),
					},
				},
			}

			err := commentConsumer.HandleEvent(ctx, commentEvent)
			if err != nil {
				t.Fatalf("Failed to handle comment %d event: %v", i, err)
			}
		}

		// Step 2: Now index the post
		postEvent := &jetstream.JetstreamEvent{
			Did:  testCommunity,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "post2-rev",
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       postRkey,
				CID:        "bafypost2",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": testCommunity,
					"author":    testUser.DID,
					"title":     "Post with 3 pre-existing comments",
					"content":   "All 3 comments arrived before this post!",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := postConsumer.HandleEvent(ctx, postEvent)
		if err != nil {
			t.Fatalf("Failed to handle post event: %v", err)
		}

		// Step 3: Verify post has comment_count = 3
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Post not indexed: %v", err)
		}

		if post.CommentCount != 3 {
			t.Errorf("Expected post comment_count to be 3 (reconciled), got %d", post.CommentCount)
		}
	})

	t.Run("Comments before and after post - count remains accurate", func(t *testing.T) {
		postRkey := generateTID()
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", testCommunity, postRkey)

		// Step 1: Index 2 comments BEFORE post
		for i := 1; i <= 2; i++ {
			commentRkey := generateTID()
			commentEvent := &jetstream.JetstreamEvent{
				Did:  testUser.DID,
				Kind: "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        fmt.Sprintf("before-%d-rev", i),
					Operation:  "create",
					Collection: "social.coves.community.comment",
					RKey:       commentRkey,
					CID:        fmt.Sprintf("bafybefore%d", i),
					Record: map[string]interface{}{
						"$type":   "social.coves.community.comment",
						"content": fmt.Sprintf("Before comment %d", i),
						"reply": map[string]interface{}{
							"root": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost3",
							},
							"parent": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost3",
							},
						},
						"createdAt": time.Now().Format(time.RFC3339),
					},
				},
			}

			err := commentConsumer.HandleEvent(ctx, commentEvent)
			if err != nil {
				t.Fatalf("Failed to handle before-comment %d: %v", i, err)
			}
		}

		// Step 2: Index the post (should reconcile to 2)
		postEvent := &jetstream.JetstreamEvent{
			Did:  testCommunity,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "post3-rev",
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       postRkey,
				CID:        "bafypost3",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": testCommunity,
					"author":    testUser.DID,
					"title":     "Post with before and after comments",
					"content":   "Testing mixed ordering",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := postConsumer.HandleEvent(ctx, postEvent)
		if err != nil {
			t.Fatalf("Failed to handle post event: %v", err)
		}

		// Verify count is 2
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Post not indexed: %v", err)
		}
		if post.CommentCount != 2 {
			t.Errorf("Expected comment_count=2 after reconciliation, got %d", post.CommentCount)
		}

		// Step 3: Add 1 more comment AFTER post exists
		commentRkey := generateTID()
		afterCommentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "after-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRkey,
				CID:        "bafyafter",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment after post exists",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost3",
						},
						"parent": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost3",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = commentConsumer.HandleEvent(ctx, afterCommentEvent)
		if err != nil {
			t.Fatalf("Failed to handle after-comment: %v", err)
		}

		// Verify count incremented to 3
		post, err = postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post after increment: %v", err)
		}
		if post.CommentCount != 3 {
			t.Errorf("Expected comment_count=3 after increment, got %d", post.CommentCount)
		}
	})

	t.Run("Idempotent post indexing preserves comment_count", func(t *testing.T) {
		postRkey := generateTID()
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", testCommunity, postRkey)

		// Create comment first
		commentRkey := generateTID()
		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "idem-comment-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRkey,
				CID:        "bafyidemcomment",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment for idempotent test",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI,
							"cid": "bafyidempost",
						},
						"parent": map[string]interface{}{
							"uri": postURI,
							"cid": "bafyidempost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := commentConsumer.HandleEvent(ctx, commentEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Index post (should reconcile to 1)
		postEvent := &jetstream.JetstreamEvent{
			Did:  testCommunity,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "idem-post-rev",
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       postRkey,
				CID:        "bafyidempost",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": testCommunity,
					"author":    testUser.DID,
					"title":     "Idempotent test post",
					"content":   "Testing idempotent indexing",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = postConsumer.HandleEvent(ctx, postEvent)
		if err != nil {
			t.Fatalf("Failed to index post first time: %v", err)
		}

		// Verify count is 1
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}
		if post.CommentCount != 1 {
			t.Errorf("Expected comment_count=1 after first index, got %d", post.CommentCount)
		}

		// Replay same post event (idempotent - should skip)
		err = postConsumer.HandleEvent(ctx, postEvent)
		if err != nil {
			t.Fatalf("Idempotent post event should not error: %v", err)
		}

		// Verify count still 1 (not reset to 0)
		post, err = postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post after replay: %v", err)
		}
		if post.CommentCount != 1 {
			t.Errorf("Expected comment_count=1 after replay (idempotent), got %d", post.CommentCount)
		}
	})
}
