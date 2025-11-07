package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCommentConsumer_CreateComment(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Setup test data
	testUser := createTestUser(t, db, "commenter.test", "did:plc:commenter123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "testcommunity", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Test Post", 0, time.Now())

	t.Run("Create comment on post", func(t *testing.T) {
		rkey := generateTID()
		uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)

		// Simulate Jetstream comment create event
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafytest123",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "This is a test comment on a post!",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the event
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle comment create event: %v", err)
		}

		// Verify comment was indexed
		comment, err := commentRepo.GetByURI(ctx, uri)
		if err != nil {
			t.Fatalf("Failed to get indexed comment: %v", err)
		}

		if comment.URI != uri {
			t.Errorf("Expected URI %s, got %s", uri, comment.URI)
		}

		if comment.CommenterDID != testUser.DID {
			t.Errorf("Expected commenter %s, got %s", testUser.DID, comment.CommenterDID)
		}

		if comment.Content != "This is a test comment on a post!" {
			t.Errorf("Expected content 'This is a test comment on a post!', got %s", comment.Content)
		}

		if comment.RootURI != testPostURI {
			t.Errorf("Expected root URI %s, got %s", testPostURI, comment.RootURI)
		}

		if comment.ParentURI != testPostURI {
			t.Errorf("Expected parent URI %s, got %s", testPostURI, comment.ParentURI)
		}

		// Verify post comment count was incremented
		var commentCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&commentCount)
		if err != nil {
			t.Fatalf("Failed to get post comment count: %v", err)
		}

		if commentCount != 1 {
			t.Errorf("Expected post comment_count to be 1, got %d", commentCount)
		}
	})

	t.Run("Idempotent create - duplicate event", func(t *testing.T) {
		rkey := generateTID()

		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafytest456",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Idempotent test comment",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// First creation
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("First creation failed: %v", err)
		}

		// Get initial comment count
		var initialCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&initialCount)
		if err != nil {
			t.Fatalf("Failed to get initial comment count: %v", err)
		}

		// Duplicate creation - should be idempotent
		err = consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Duplicate event should be handled gracefully: %v", err)
		}

		// Verify count wasn't incremented again
		var finalCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&finalCount)
		if err != nil {
			t.Fatalf("Failed to get final comment count: %v", err)
		}

		if finalCount != initialCount {
			t.Errorf("Comment count should not increase on duplicate event. Initial: %d, Final: %d", initialCount, finalCount)
		}
	})
}

func TestCommentConsumer_Threading(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Setup test data
	testUser := createTestUser(t, db, "threader.test", "did:plc:threader123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "threadcommunity", "owner2.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Threading Test", 0, time.Now())

	t.Run("Create nested comment replies", func(t *testing.T) {
		// Create first-level comment on post
		comment1Rkey := generateTID()
		comment1URI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, comment1Rkey)

		event1 := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       comment1Rkey,
				CID:        "bafycomment1",
				Record: map[string]interface{}{
					"content": "First level comment",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event1)
		if err != nil {
			t.Fatalf("Failed to create first-level comment: %v", err)
		}

		// Create second-level comment (reply to first comment)
		comment2Rkey := generateTID()
		comment2URI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, comment2Rkey)

		event2 := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       comment2Rkey,
				CID:        "bafycomment2",
				Record: map[string]interface{}{
					"content": "Second level comment (reply to first)",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": comment1URI,
							"cid": "bafycomment1",
						},
					},
					"createdAt": time.Now().Add(1 * time.Second).Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, event2)
		if err != nil {
			t.Fatalf("Failed to create second-level comment: %v", err)
		}

		// Verify threading structure
		comment1, err := commentRepo.GetByURI(ctx, comment1URI)
		if err != nil {
			t.Fatalf("Failed to get first comment: %v", err)
		}

		comment2, err := commentRepo.GetByURI(ctx, comment2URI)
		if err != nil {
			t.Fatalf("Failed to get second comment: %v", err)
		}

		// Both should have same root (original post)
		if comment1.RootURI != testPostURI {
			t.Errorf("Comment1 root should be post URI, got %s", comment1.RootURI)
		}

		if comment2.RootURI != testPostURI {
			t.Errorf("Comment2 root should be post URI, got %s", comment2.RootURI)
		}

		// Comment1 parent should be post
		if comment1.ParentURI != testPostURI {
			t.Errorf("Comment1 parent should be post URI, got %s", comment1.ParentURI)
		}

		// Comment2 parent should be comment1
		if comment2.ParentURI != comment1URI {
			t.Errorf("Comment2 parent should be comment1 URI, got %s", comment2.ParentURI)
		}

		// Verify reply count on comment1
		if comment1.ReplyCount != 1 {
			t.Errorf("Comment1 should have 1 reply, got %d", comment1.ReplyCount)
		}

		// Query all comments by root
		allComments, err := commentRepo.ListByRoot(ctx, testPostURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list comments by root: %v", err)
		}

		if len(allComments) != 2 {
			t.Errorf("Expected 2 comments in thread, got %d", len(allComments))
		}

		// Query direct replies to post
		directReplies, err := commentRepo.ListByParent(ctx, testPostURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list direct replies to post: %v", err)
		}

		if len(directReplies) != 1 {
			t.Errorf("Expected 1 direct reply to post, got %d", len(directReplies))
		}

		// Query replies to comment1
		comment1Replies, err := commentRepo.ListByParent(ctx, comment1URI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list replies to comment1: %v", err)
		}

		if len(comment1Replies) != 1 {
			t.Errorf("Expected 1 reply to comment1, got %d", len(comment1Replies))
		}
	})
}

func TestCommentConsumer_UpdateComment(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Setup test data
	testUser := createTestUser(t, db, "editor.test", "did:plc:editor123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "editcommunity", "owner3.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Edit Test", 0, time.Now())

	t.Run("Update comment content preserves vote counts", func(t *testing.T) {
		rkey := generateTID()
		uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)

		// Create initial comment
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafyoriginal",
				Record: map[string]interface{}{
					"content": "Original comment content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Manually set vote counts to simulate votes
		_, err = db.ExecContext(ctx, `
			UPDATE comments
			SET upvote_count = 5, downvote_count = 2, score = 3
			WHERE uri = $1
		`, uri)
		if err != nil {
			t.Fatalf("Failed to set vote counts: %v", err)
		}

		// Update the comment
		updateEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "update",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafyupdated",
				Record: map[string]interface{}{
					"content": "EDITED: Updated comment content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, updateEvent)
		if err != nil {
			t.Fatalf("Failed to update comment: %v", err)
		}

		// Verify content updated
		comment, err := commentRepo.GetByURI(ctx, uri)
		if err != nil {
			t.Fatalf("Failed to get updated comment: %v", err)
		}

		if comment.Content != "EDITED: Updated comment content" {
			t.Errorf("Expected updated content, got %s", comment.Content)
		}

		// Verify CID updated
		if comment.CID != "bafyupdated" {
			t.Errorf("Expected CID to be updated to bafyupdated, got %s", comment.CID)
		}

		// Verify vote counts preserved
		if comment.UpvoteCount != 5 {
			t.Errorf("Expected upvote_count preserved at 5, got %d", comment.UpvoteCount)
		}

		if comment.DownvoteCount != 2 {
			t.Errorf("Expected downvote_count preserved at 2, got %d", comment.DownvoteCount)
		}

		if comment.Score != 3 {
			t.Errorf("Expected score preserved at 3, got %d", comment.Score)
		}
	})
}

func TestCommentConsumer_DeleteComment(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Setup test data
	testUser := createTestUser(t, db, "deleter.test", "did:plc:deleter123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "deletecommunity", "owner4.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Delete Test", 0, time.Now())

	t.Run("Delete comment decrements parent count", func(t *testing.T) {
		rkey := generateTID()
		uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)

		// Create comment
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafydelete",
				Record: map[string]interface{}{
					"content": "Comment to be deleted",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Get initial post comment count
		var initialCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&initialCount)
		if err != nil {
			t.Fatalf("Failed to get initial comment count: %v", err)
		}

		// Delete comment
		deleteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
			},
		}

		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Failed to delete comment: %v", err)
		}

		// Verify soft delete
		comment, err := commentRepo.GetByURI(ctx, uri)
		if err != nil {
			t.Fatalf("Failed to get deleted comment: %v", err)
		}

		if comment.DeletedAt == nil {
			t.Error("Expected deleted_at to be set, got nil")
		}

		// Verify post comment count decremented
		var finalCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&finalCount)
		if err != nil {
			t.Fatalf("Failed to get final comment count: %v", err)
		}

		if finalCount != initialCount-1 {
			t.Errorf("Expected comment count to decrease by 1. Initial: %d, Final: %d", initialCount, finalCount)
		}
	})

	t.Run("Delete is idempotent", func(t *testing.T) {
		rkey := generateTID()

		// Create comment
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafyidempdelete",
				Record: map[string]interface{}{
					"content": "Idempotent delete test",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// First delete
		deleteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
			},
		}

		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("First delete failed: %v", err)
		}

		// Get count after first delete
		var countAfterFirstDelete int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&countAfterFirstDelete)
		if err != nil {
			t.Fatalf("Failed to get count after first delete: %v", err)
		}

		// Second delete (idempotent)
		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Second delete should be idempotent: %v", err)
		}

		// Verify count didn't change
		var countAfterSecondDelete int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", testPostURI).Scan(&countAfterSecondDelete)
		if err != nil {
			t.Fatalf("Failed to get count after second delete: %v", err)
		}

		if countAfterSecondDelete != countAfterFirstDelete {
			t.Errorf("Count should not change on duplicate delete. After first: %d, After second: %d", countAfterFirstDelete, countAfterSecondDelete)
		}
	})
}

func TestCommentConsumer_SecurityValidation(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	testUser := createTestUser(t, db, "security.test", "did:plc:security123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "seccommunity", "owner5.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Security Test", 0, time.Now())

	t.Run("Reject comment with empty content", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafyinvalid",
				Record: map[string]interface{}{
					"content": "", // Empty content
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for empty content, got nil")
		}
	})

	t.Run("Reject comment with invalid root reference", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafyinvalid2",
				Record: map[string]interface{}{
					"content": "Valid content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": "", // Missing URI
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for invalid root reference, got nil")
		}
	})

	t.Run("Reject comment with invalid parent reference", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafyinvalid3",
				Record: map[string]interface{}{
					"content": "Valid content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "", // Missing CID
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for invalid parent reference, got nil")
		}
	})

	t.Run("Reject comment with invalid DID format", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  "invalid-did-format", // Bad DID
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafyinvalid4",
				Record: map[string]interface{}{
					"content": "Valid content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for invalid DID format, got nil")
		}
	})

	t.Run("Reject comment exceeding max content length", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafytoobig",
				Record: map[string]interface{}{
					"content": string(make([]byte, 30001)), // Exceeds 30000 byte limit
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for oversized content, got nil")
		}
		if err != nil && !contains(err.Error(), "exceeds maximum length") {
			t.Errorf("Expected 'exceeds maximum length' error, got: %v", err)
		}
	})

	t.Run("Reject comment with malformed parent URI", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafymalformed",
				Record: map[string]interface{}{
					"content": "Valid content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": "at://malformed", // Invalid: missing collection/rkey
							"cid": "bafyparent",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for malformed AT-URI, got nil")
		}
		if err != nil && !contains(err.Error(), "invalid parent URI") {
			t.Errorf("Expected 'invalid parent URI' error, got: %v", err)
		}
	})

	t.Run("Reject comment with malformed root URI", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       generateTID(),
				CID:        "bafymalformed2",
				Record: map[string]interface{}{
					"content": "Valid content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": "at://did:plc:test123", // Invalid: missing collection/rkey
							"cid": "bafyroot",
						},
						"parent": map[string]interface{}{
							"uri": testPostURI,
							"cid": "bafyparent",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Expected error for malformed AT-URI, got nil")
		}
		if err != nil && !contains(err.Error(), "invalid root URI") {
			t.Errorf("Expected 'invalid root URI' error, got: %v", err)
		}
	})
}

func TestCommentRepository_Queries(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)

	// Clean up any existing test data from previous runs
	_, err := db.ExecContext(ctx, "DELETE FROM comments WHERE commenter_did LIKE 'did:plc:%'")
	if err != nil {
		t.Fatalf("Failed to clean up test comments: %v", err)
	}

	testUser := createTestUser(t, db, "query.test", "did:plc:query123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "querycommunity", "owner6.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Query Test", 0, time.Now())

	// Create a comment tree
	// Post
	//  |- Comment 1
	//      |- Comment 2
	//      |- Comment 3
	//  |- Comment 4

	comment1 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.feed.comment/1", testUser.DID),
		CID:          "bafyc1",
		RKey:         "1",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    postURI,
		ParentCID:    "bafypost",
		Content:      "Comment 1",
		Langs:        []string{},
		CreatedAt:    time.Now(),
	}

	comment2 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.feed.comment/2", testUser.DID),
		CID:          "bafyc2",
		RKey:         "2",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    comment1.URI,
		ParentCID:    "bafyc1",
		Content:      "Comment 2 (reply to 1)",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(1 * time.Second),
	}

	comment3 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.feed.comment/3", testUser.DID),
		CID:          "bafyc3",
		RKey:         "3",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    comment1.URI,
		ParentCID:    "bafyc1",
		Content:      "Comment 3 (reply to 1)",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(2 * time.Second),
	}

	comment4 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.feed.comment/4", testUser.DID),
		CID:          "bafyc4",
		RKey:         "4",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    postURI,
		ParentCID:    "bafypost",
		Content:      "Comment 4",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(3 * time.Second),
	}

	// Create all comments
	for i, c := range []*comments.Comment{comment1, comment2, comment3, comment4} {
		if err := commentRepo.Create(ctx, c); err != nil {
			t.Fatalf("Failed to create comment %d: %v", i+1, err)
		}
		t.Logf("Created comment %d: %s", i+1, c.URI)
	}

	// Verify comments were created
	verifyCount, err := commentRepo.CountByParent(ctx, postURI)
	if err != nil {
		t.Fatalf("Failed to count comments: %v", err)
	}
	t.Logf("Direct replies to post after creation: %d", verifyCount)

	t.Run("ListByRoot returns all comments in thread", func(t *testing.T) {
		comments, err := commentRepo.ListByRoot(ctx, postURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list by root: %v", err)
		}

		if len(comments) != 4 {
			t.Errorf("Expected 4 comments, got %d", len(comments))
		}
	})

	t.Run("ListByParent returns direct replies", func(t *testing.T) {
		// Direct replies to post
		postReplies, err := commentRepo.ListByParent(ctx, postURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list post replies: %v", err)
		}

		if len(postReplies) != 2 {
			t.Errorf("Expected 2 direct replies to post, got %d", len(postReplies))
		}

		// Direct replies to comment1
		comment1Replies, err := commentRepo.ListByParent(ctx, comment1.URI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list comment1 replies: %v", err)
		}

		if len(comment1Replies) != 2 {
			t.Errorf("Expected 2 direct replies to comment1, got %d", len(comment1Replies))
		}
	})

	t.Run("CountByParent returns correct counts", func(t *testing.T) {
		postCount, err := commentRepo.CountByParent(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to count post replies: %v", err)
		}

		if postCount != 2 {
			t.Errorf("Expected 2 direct replies to post, got %d", postCount)
		}

		comment1Count, err := commentRepo.CountByParent(ctx, comment1.URI)
		if err != nil {
			t.Fatalf("Failed to count comment1 replies: %v", err)
		}

		if comment1Count != 2 {
			t.Errorf("Expected 2 direct replies to comment1, got %d", comment1Count)
		}
	})

	t.Run("ListByCommenter returns user's comments", func(t *testing.T) {
		userComments, err := commentRepo.ListByCommenter(ctx, testUser.DID, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list by commenter: %v", err)
		}

		if len(userComments) != 4 {
			t.Errorf("Expected 4 comments by user, got %d", len(userComments))
		}
	})
}

// TestCommentConsumer_OutOfOrderReconciliation tests that parent counts are
// correctly reconciled when child comments arrive before their parent
func TestCommentConsumer_OutOfOrderReconciliation(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	testUser := createTestUser(t, db, "outoforder.test", "did:plc:outoforder123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "ooo-community", "owner7.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	postURI := createTestPost(t, db, testCommunity, testUser.DID, "OOO Test Post", 0, time.Now())

	t.Run("Child arrives before parent - count reconciled", func(t *testing.T) {
		// Scenario: User A creates comment C1 on post
		//           User B creates reply C2 to C1
		//           Jetstream delivers C2 before C1 (different repos)
		//           When C1 finally arrives, its reply_count should be 1, not 0

		parentRkey := generateTID()
		parentURI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, parentRkey)

		childRkey := generateTID()
		childURI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, childRkey)

		// Step 1: Index child FIRST (before parent exists)
		childEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "child-rev",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       childRkey,
				CID:        "bafychild",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "This is a reply to a comment that doesn't exist yet!",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI,
							"cid": "bafypost",
						},
						"parent": map[string]interface{}{
							"uri": parentURI, // Points to parent that doesn't exist yet
							"cid": "bafyparent",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, childEvent)
		if err != nil {
			t.Fatalf("Failed to handle child event: %v", err)
		}

		// Verify child was indexed
		childComment, err := commentRepo.GetByURI(ctx, childURI)
		if err != nil {
			t.Fatalf("Child comment not indexed: %v", err)
		}
		if childComment.ParentURI != parentURI {
			t.Errorf("Expected child parent_uri %s, got %s", parentURI, childComment.ParentURI)
		}

		// Step 2: Now index parent (arrives late due to Jetstream ordering)
		parentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "parent-rev",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       parentRkey,
				CID:        "bafyparent",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "This is the parent comment arriving late",
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
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, parentEvent)
		if err != nil {
			t.Fatalf("Failed to handle parent event: %v", err)
		}

		// Step 3: Verify parent was indexed with CORRECT reply_count
		parentComment, err := commentRepo.GetByURI(ctx, parentURI)
		if err != nil {
			t.Fatalf("Parent comment not indexed: %v", err)
		}

		// THIS IS THE KEY TEST: Parent should have reply_count = 1 due to reconciliation
		if parentComment.ReplyCount != 1 {
			t.Errorf("Expected parent reply_count to be 1 (reconciled), got %d", parentComment.ReplyCount)
			t.Logf("This indicates out-of-order reconciliation failed!")
		}

		// Verify via query as well
		count, err := commentRepo.CountByParent(ctx, parentURI)
		if err != nil {
			t.Fatalf("Failed to count parent replies: %v", err)
		}
		if count != 1 {
			t.Errorf("Expected 1 reply to parent, got %d", count)
		}
	})

	t.Run("Multiple children arrive before parent", func(t *testing.T) {
		parentRkey := generateTID()
		parentURI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, parentRkey)

		// Index 3 children before parent
		for i := 1; i <= 3; i++ {
			childRkey := generateTID()
			childEvent := &jetstream.JetstreamEvent{
				Did:  testUser.DID,
				Kind: "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        fmt.Sprintf("child-%d-rev", i),
					Operation:  "create",
					Collection: "social.coves.feed.comment",
					RKey:       childRkey,
					CID:        fmt.Sprintf("bafychild%d", i),
					Record: map[string]interface{}{
						"$type":   "social.coves.feed.comment",
						"content": fmt.Sprintf("Reply %d before parent", i),
						"reply": map[string]interface{}{
							"root": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost",
							},
							"parent": map[string]interface{}{
								"uri": parentURI,
								"cid": "bafyparent2",
							},
						},
						"createdAt": time.Now().Format(time.RFC3339),
					},
				},
			}

			err := consumer.HandleEvent(ctx, childEvent)
			if err != nil {
				t.Fatalf("Failed to handle child %d event: %v", i, err)
			}
		}

		// Now index parent
		parentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "parent2-rev",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       parentRkey,
				CID:        "bafyparent2",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Parent with 3 pre-existing children",
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
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, parentEvent)
		if err != nil {
			t.Fatalf("Failed to handle parent event: %v", err)
		}

		// Verify parent has reply_count = 3
		parentComment, err := commentRepo.GetByURI(ctx, parentURI)
		if err != nil {
			t.Fatalf("Parent comment not indexed: %v", err)
		}

		if parentComment.ReplyCount != 3 {
			t.Errorf("Expected parent reply_count to be 3 (reconciled), got %d", parentComment.ReplyCount)
		}
	})
}

// TestCommentConsumer_Resurrection tests that soft-deleted comments can be recreated
// In atProto, deleted records' rkeys become available for reuse
func TestCommentConsumer_Resurrection(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	testUser := createTestUser(t, db, "resurrect.test", "did:plc:resurrect123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "resurrect-community", "owner8.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Resurrection Test", 0, time.Now())

	rkey := generateTID()
	commentURI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)

	t.Run("Recreate deleted comment with same rkey", func(t *testing.T) {
		// Step 1: Create initial comment
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v1",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafyoriginal",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Original comment content",
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
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create initial comment: %v", err)
		}

		// Verify comment exists
		comment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Comment not found after creation: %v", err)
		}
		if comment.Content != "Original comment content" {
			t.Errorf("Expected content 'Original comment content', got '%s'", comment.Content)
		}
		if comment.DeletedAt != nil {
			t.Errorf("Expected deleted_at to be nil, got %v", comment.DeletedAt)
		}

		// Step 2: Delete the comment
		deleteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v2",
				Operation:  "delete",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
			},
		}

		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Failed to delete comment: %v", err)
		}

		// Verify comment is soft-deleted
		comment, err = commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Comment not found after deletion: %v", err)
		}
		if comment.DeletedAt == nil {
			t.Error("Expected deleted_at to be set, got nil")
		}

		// Step 3: Recreate comment with same rkey (resurrection)
		// In atProto, this is a valid operation - user can reuse the rkey
		recreateEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v3",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey, // Same rkey!
				CID:        "bafyresurrected",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Resurrected comment with new content",
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
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, recreateEvent)
		if err != nil {
			t.Fatalf("Failed to resurrect comment: %v", err)
		}

		// Step 4: Verify comment is resurrected with new content
		comment, err = commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Comment not found after resurrection: %v", err)
		}

		if comment.DeletedAt != nil {
			t.Errorf("Expected deleted_at to be NULL after resurrection, got %v", comment.DeletedAt)
		}
		if comment.Content != "Resurrected comment with new content" {
			t.Errorf("Expected resurrected content, got '%s'", comment.Content)
		}
		if comment.CID != "bafyresurrected" {
			t.Errorf("Expected CID 'bafyresurrected', got '%s'", comment.CID)
		}

		// Verify parent count was restored (post should have comment_count = 1)
		var postCommentCount int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", postURI).Scan(&postCommentCount)
		if err != nil {
			t.Fatalf("Failed to check post comment count: %v", err)
		}
		if postCommentCount != 1 {
			t.Errorf("Expected post comment_count to be 1 after resurrection, got %d", postCommentCount)
		}
	})

	t.Run("Recreate deleted comment with DIFFERENT parent", func(t *testing.T) {
		// Create two posts
		post1URI := createTestPost(t, db, testCommunity, testUser.DID, "Post 1", 0, time.Now())
		post2URI := createTestPost(t, db, testCommunity, testUser.DID, "Post 2", 0, time.Now())

		rkey2 := generateTID()
		commentURI2 := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey2)

		// Step 1: Create comment on Post 1
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v1",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey2,
				CID:        "bafyv1",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Original on Post 1",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": post1URI,
							"cid": "bafypost1",
						},
						"parent": map[string]interface{}{
							"uri": post1URI,
							"cid": "bafypost1",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment on Post 1: %v", err)
		}

		// Verify Post 1 has comment_count = 1
		var post1Count int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", post1URI).Scan(&post1Count)
		if err != nil {
			t.Fatalf("Failed to check post 1 count: %v", err)
		}
		if post1Count != 1 {
			t.Errorf("Expected Post 1 comment_count = 1, got %d", post1Count)
		}

		// Step 2: Delete comment
		deleteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v2",
				Operation:  "delete",
				Collection: "social.coves.feed.comment",
				RKey:       rkey2,
			},
		}

		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Failed to delete comment: %v", err)
		}

		// Verify Post 1 count decremented to 0
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", post1URI).Scan(&post1Count)
		if err != nil {
			t.Fatalf("Failed to check post 1 count after delete: %v", err)
		}
		if post1Count != 0 {
			t.Errorf("Expected Post 1 comment_count = 0 after delete, got %d", post1Count)
		}

		// Step 3: Recreate comment with same rkey but on Post 2 (different parent!)
		recreateEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v3",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey2, // Same rkey!
				CID:        "bafyv3",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "New comment on Post 2",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": post2URI, // Different root!
							"cid": "bafypost2",
						},
						"parent": map[string]interface{}{
							"uri": post2URI, // Different parent!
							"cid": "bafypost2",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, recreateEvent)
		if err != nil {
			t.Fatalf("Failed to resurrect comment on Post 2: %v", err)
		}

		// Step 4: Verify threading references updated correctly
		comment, err := commentRepo.GetByURI(ctx, commentURI2)
		if err != nil {
			t.Fatalf("Failed to get resurrected comment: %v", err)
		}

		// THIS IS THE CRITICAL TEST: Threading refs must point to Post 2, not Post 1
		if comment.ParentURI != post2URI {
			t.Errorf("Expected parent URI to be %s (Post 2), got %s (STALE!)", post2URI, comment.ParentURI)
		}
		if comment.RootURI != post2URI {
			t.Errorf("Expected root URI to be %s (Post 2), got %s (STALE!)", post2URI, comment.RootURI)
		}
		if comment.ParentCID != "bafypost2" {
			t.Errorf("Expected parent CID 'bafypost2', got '%s'", comment.ParentCID)
		}

		// Verify counts are correct
		var post2Count int
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", post2URI).Scan(&post2Count)
		if err != nil {
			t.Fatalf("Failed to check post 2 count: %v", err)
		}
		if post2Count != 1 {
			t.Errorf("Expected Post 2 comment_count = 1, got %d", post2Count)
		}

		// Verify Post 1 count still 0 (not incremented by resurrection on Post 2)
		err = db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", post1URI).Scan(&post1Count)
		if err != nil {
			t.Fatalf("Failed to check post 1 count after resurrection: %v", err)
		}
		if post1Count != 0 {
			t.Errorf("Expected Post 1 comment_count = 0 (unchanged), got %d", post1Count)
		}
	})
}

// TestCommentConsumer_ThreadingImmutability tests that UPDATE events cannot change threading refs
func TestCommentConsumer_ThreadingImmutability(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	consumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	testUser := createTestUser(t, db, "immutable.test", "did:plc:immutable123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "immutable-community", "owner9.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	postURI1 := createTestPost(t, db, testCommunity, testUser.DID, "Post 1", 0, time.Now())
	postURI2 := createTestPost(t, db, testCommunity, testUser.DID, "Post 2", 0, time.Now())

	rkey := generateTID()
	commentURI := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey)

	t.Run("Reject UPDATE that changes parent URI", func(t *testing.T) {
		// Create comment on Post 1
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v1",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafycomment1",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Comment on Post 1",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI1,
							"cid": "bafypost1",
						},
						"parent": map[string]interface{}{
							"uri": postURI1,
							"cid": "bafypost1",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Attempt to update comment to move it to Post 2 (should fail)
		updateEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v2",
				Operation:  "update",
				Collection: "social.coves.feed.comment",
				RKey:       rkey,
				CID:        "bafycomment2",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Trying to hijack this comment to Post 2",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI2, // Changed!
							"cid": "bafypost2",
						},
						"parent": map[string]interface{}{
							"uri": postURI2, // Changed!
							"cid": "bafypost2",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, updateEvent)
		if err == nil {
			t.Error("Expected error when changing threading references, got nil")
		}
		if err != nil && !contains(err.Error(), "threading references cannot be changed") {
			t.Errorf("Expected 'threading references cannot be changed' error, got: %v", err)
		}

		// Verify comment still points to Post 1
		comment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Failed to get comment: %v", err)
		}
		if comment.ParentURI != postURI1 {
			t.Errorf("Expected parent URI to remain %s, got %s", postURI1, comment.ParentURI)
		}
		if comment.RootURI != postURI1 {
			t.Errorf("Expected root URI to remain %s, got %s", postURI1, comment.RootURI)
		}
		// Content should NOT have been updated since the operation was rejected
		if comment.Content != "Comment on Post 1" {
			t.Errorf("Expected original content, got '%s'", comment.Content)
		}
	})

	t.Run("Allow UPDATE that only changes content (threading unchanged)", func(t *testing.T) {
		rkey2 := generateTID()
		commentURI2 := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", testUser.DID, rkey2)

		// Create comment
		createEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v1",
				Operation:  "create",
				Collection: "social.coves.feed.comment",
				RKey:       rkey2,
				CID:        "bafycomment3",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Original content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI1,
							"cid": "bafypost1",
						},
						"parent": map[string]interface{}{
							"uri": postURI1,
							"cid": "bafypost1",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Update content only (threading unchanged - should succeed)
		updateEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "v2",
				Operation:  "update",
				Collection: "social.coves.feed.comment",
				RKey:       rkey2,
				CID:        "bafycomment4",
				Record: map[string]interface{}{
					"$type":   "social.coves.feed.comment",
					"content": "Updated content",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI1, // Same
							"cid": "bafypost1",
						},
						"parent": map[string]interface{}{
							"uri": postURI1, // Same
							"cid": "bafypost1",
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, updateEvent)
		if err != nil {
			t.Fatalf("Expected update to succeed when threading unchanged, got error: %v", err)
		}

		// Verify content was updated
		comment, err := commentRepo.GetByURI(ctx, commentURI2)
		if err != nil {
			t.Fatalf("Failed to get comment: %v", err)
		}
		if comment.Content != "Updated content" {
			t.Errorf("Expected updated content, got '%s'", comment.Content)
		}
		// Threading should remain unchanged
		if comment.ParentURI != postURI1 {
			t.Errorf("Expected parent URI %s, got %s", postURI1, comment.ParentURI)
		}
	})
}
