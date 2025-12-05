package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

// TestCommentVote_CreateAndUpdate tests voting on comments and vote count updates
func TestCommentVote_CreateAndUpdate(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	voteRepo := postgres.NewVoteRepository(db)
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, nil, "http://localhost:3001")

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Use fixed timestamp to prevent flaky tests
	fixedTime := time.Date(2025, 11, 6, 12, 0, 0, 0, time.UTC)

	// Setup test data
	testUser := createTestUser(t, db, "voter.test", "did:plc:voter123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "testcommunity", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Test Post", 0, fixedTime)

	t.Run("Upvote on comment increments count", func(t *testing.T) {
		// Create a comment
		commentRKey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRKey)
		commentCID := "bafycomment123"

		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        commentCID,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment to vote on",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, commentEvent); err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Verify initial counts
		comment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Failed to get comment: %v", err)
		}
		if comment.UpvoteCount != 0 {
			t.Errorf("Expected initial upvote_count = 0, got %d", comment.UpvoteCount)
		}

		// Create upvote on comment
		voteRKey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", testUser.DID, voteRKey)

		voteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvote123",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": commentURI,
						"cid": commentCID,
					},
					"direction": "up",
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := voteConsumer.HandleEvent(ctx, voteEvent); err != nil {
			t.Fatalf("Failed to create vote: %v", err)
		}

		// Verify vote was indexed
		vote, err := voteRepo.GetByURI(ctx, voteURI)
		if err != nil {
			t.Fatalf("Failed to get vote: %v", err)
		}
		if vote.SubjectURI != commentURI {
			t.Errorf("Expected vote subject_uri = %s, got %s", commentURI, vote.SubjectURI)
		}
		if vote.Direction != "up" {
			t.Errorf("Expected vote direction = 'up', got %s", vote.Direction)
		}

		// Verify comment counts updated
		updatedComment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Failed to get updated comment: %v", err)
		}
		if updatedComment.UpvoteCount != 1 {
			t.Errorf("Expected upvote_count = 1, got %d", updatedComment.UpvoteCount)
		}
		if updatedComment.Score != 1 {
			t.Errorf("Expected score = 1, got %d", updatedComment.Score)
		}
	})

	t.Run("Downvote on comment increments downvote count", func(t *testing.T) {
		// Create a comment
		commentRKey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRKey)
		commentCID := "bafycomment456"

		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        commentCID,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment to downvote",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, commentEvent); err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Create downvote
		voteRKey := generateTID()

		voteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvote456",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": commentURI,
						"cid": commentCID,
					},
					"direction": "down",
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := voteConsumer.HandleEvent(ctx, voteEvent); err != nil {
			t.Fatalf("Failed to create downvote: %v", err)
		}

		// Verify comment counts
		updatedComment, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Failed to get updated comment: %v", err)
		}
		if updatedComment.DownvoteCount != 1 {
			t.Errorf("Expected downvote_count = 1, got %d", updatedComment.DownvoteCount)
		}
		if updatedComment.Score != -1 {
			t.Errorf("Expected score = -1, got %d", updatedComment.Score)
		}
	})

	t.Run("Delete vote decrements comment counts", func(t *testing.T) {
		// Create comment
		commentRKey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRKey)
		commentCID := "bafycomment789"

		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        commentCID,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment for vote deletion test",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, commentEvent); err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Create vote
		voteRKey := generateTID()

		createVoteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvote789",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": commentURI,
						"cid": commentCID,
					},
					"direction": "up",
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := voteConsumer.HandleEvent(ctx, createVoteEvent); err != nil {
			t.Fatalf("Failed to create vote: %v", err)
		}

		// Verify vote exists
		commentAfterVote, _ := commentRepo.GetByURI(ctx, commentURI)
		if commentAfterVote.UpvoteCount != 1 {
			t.Fatalf("Expected upvote_count = 1 before delete, got %d", commentAfterVote.UpvoteCount)
		}

		// Delete vote
		deleteVoteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "delete",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
			},
		}

		if err := voteConsumer.HandleEvent(ctx, deleteVoteEvent); err != nil {
			t.Fatalf("Failed to delete vote: %v", err)
		}

		// Verify counts decremented
		commentAfterDelete, err := commentRepo.GetByURI(ctx, commentURI)
		if err != nil {
			t.Fatalf("Failed to get comment after vote delete: %v", err)
		}
		if commentAfterDelete.UpvoteCount != 0 {
			t.Errorf("Expected upvote_count = 0 after delete, got %d", commentAfterDelete.UpvoteCount)
		}
		if commentAfterDelete.Score != 0 {
			t.Errorf("Expected score = 0 after delete, got %d", commentAfterDelete.Score)
		}
	})
}

// TestCommentVote_ViewerState tests viewer vote state in comment query responses
func TestCommentVote_ViewerState(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	userService := users.NewUserService(userRepo, nil, "http://localhost:3001")

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Use fixed timestamp to prevent flaky tests
	fixedTime := time.Date(2025, 11, 6, 12, 0, 0, 0, time.UTC)

	// Setup test data
	testUser := createTestUser(t, db, "viewer.test", "did:plc:viewer123")
	testCommunity, err := createFeedTestCommunity(db, ctx, "testcommunity", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := createTestPost(t, db, testCommunity, testUser.DID, "Test Post", 0, fixedTime)

	t.Run("Viewer with vote sees vote state", func(t *testing.T) {
		// Create comment
		commentRKey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRKey)
		commentCID := "bafycomment111"

		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        commentCID,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment with viewer vote",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, commentEvent); err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Create vote
		voteRKey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", testUser.DID, voteRKey)

		voteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvote111",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": commentURI,
						"cid": commentCID,
					},
					"direction": "up",
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := voteConsumer.HandleEvent(ctx, voteEvent); err != nil {
			t.Fatalf("Failed to create vote: %v", err)
		}

		// Query comments with viewer authentication
		// Use factory constructor with nil factory - this test only uses the read path (GetComments)
		commentService := comments.NewCommentServiceWithPDSFactory(commentRepo, userRepo, postRepo, communityRepo, nil, nil)
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI:   testPostURI,
			Sort:      "new",
			Depth:     10,
			Limit:     100,
			ViewerDID: &testUser.DID,
		})
		if err != nil {
			t.Fatalf("Failed to get comments: %v", err)
		}

		if len(response.Comments) == 0 {
			t.Fatal("Expected at least one comment in response")
		}

		// Find our comment
		var foundComment *comments.CommentView
		for _, threadView := range response.Comments {
			if threadView.Comment.URI == commentURI {
				foundComment = threadView.Comment
				break
			}
		}

		if foundComment == nil {
			t.Fatal("Expected to find test comment in response")
		}

		// Verify viewer state
		if foundComment.Viewer == nil {
			t.Fatal("Expected viewer state for authenticated request")
		}
		if foundComment.Viewer.Vote == nil {
			t.Error("Expected viewer.vote to be populated")
		} else if *foundComment.Viewer.Vote != "up" {
			t.Errorf("Expected viewer.vote = 'up', got %s", *foundComment.Viewer.Vote)
		}
		if foundComment.Viewer.VoteURI == nil {
			t.Error("Expected viewer.voteUri to be populated")
		} else if *foundComment.Viewer.VoteURI != voteURI {
			t.Errorf("Expected viewer.voteUri = %s, got %s", voteURI, *foundComment.Viewer.VoteURI)
		}
	})

	t.Run("Viewer without vote sees empty state", func(t *testing.T) {
		// Create comment (no vote)
		commentRKey := generateTID()
		commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, commentRKey)

		commentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        "bafycomment222",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Comment without viewer vote",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, commentEvent); err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Query with authentication but no vote
		// Use factory constructor with nil factory - this test only uses the read path (GetComments)
		commentService := comments.NewCommentServiceWithPDSFactory(commentRepo, userRepo, postRepo, communityRepo, nil, nil)
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI:   testPostURI,
			Sort:      "new",
			Depth:     10,
			Limit:     100,
			ViewerDID: &testUser.DID,
		})
		if err != nil {
			t.Fatalf("Failed to get comments: %v", err)
		}

		if len(response.Comments) == 0 {
			t.Fatal("Expected at least one comment in response")
		}

		// Find our comment
		var foundComment *comments.CommentView
		for _, threadView := range response.Comments {
			if threadView.Comment.URI == commentURI {
				foundComment = threadView.Comment
				break
			}
		}

		if foundComment == nil {
			t.Fatal("Expected to find test comment in response")
		}

		// Verify viewer state exists but no vote
		if foundComment.Viewer == nil {
			t.Fatal("Expected viewer state for authenticated request")
		}
		if foundComment.Viewer.Vote != nil {
			t.Errorf("Expected viewer.vote = nil (no vote), got %v", *foundComment.Viewer.Vote)
		}
		if foundComment.Viewer.VoteURI != nil {
			t.Errorf("Expected viewer.voteUri = nil (no vote), got %v", *foundComment.Viewer.VoteURI)
		}
	})

	t.Run("Unauthenticated request has no viewer state", func(t *testing.T) {
		// Query without authentication
		// Use factory constructor with nil factory - this test only uses the read path (GetComments)
		commentService := comments.NewCommentServiceWithPDSFactory(commentRepo, userRepo, postRepo, communityRepo, nil, nil)
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI:   testPostURI,
			Sort:      "new",
			Depth:     10,
			Limit:     100,
			ViewerDID: nil, // No authentication
		})
		if err != nil {
			t.Fatalf("Failed to get comments: %v", err)
		}

		if len(response.Comments) > 0 {
			// Verify no viewer state
			if response.Comments[0].Comment.Viewer != nil {
				t.Error("Expected viewer = nil for unauthenticated request")
			}
		}
	})
}
