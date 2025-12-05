package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrentVoting_MultipleUsersOnSamePost tests race conditions when multiple users
// vote on the same post simultaneously
func TestConcurrentVoting_MultipleUsersOnSamePost(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, nil, "http://localhost:3001")
	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)

	// Use fixed timestamp
	fixedTime := time.Date(2025, 11, 16, 12, 0, 0, 0, time.UTC)

	// Setup: Create test community and post
	testCommunity, err := createFeedTestCommunity(db, ctx, "concurrent-votes", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	testUser := createTestUser(t, db, "author.test", "did:plc:author123")
	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Post for concurrent voting", 0, fixedTime)

	t.Run("Multiple users upvoting same post concurrently", func(t *testing.T) {
		const numVoters = 20
		var wg sync.WaitGroup
		wg.Add(numVoters)

		// Channel to collect errors
		errors := make(chan error, numVoters)

		// Create voters and vote concurrently
		for i := 0; i < numVoters; i++ {
			go func(voterIndex int) {
				defer wg.Done()

				voterDID := fmt.Sprintf("did:plc:voter%d", voterIndex)
				voterHandle := fmt.Sprintf("voter%d.test", voterIndex)

				// Create user
				_, createErr := userService.CreateUser(ctx, users.CreateUserRequest{
					DID:    voterDID,
					Handle: voterHandle,
					PDSURL: "http://localhost:3001",
				})
				if createErr != nil {
					errors <- fmt.Errorf("voter %d: failed to create user: %w", voterIndex, createErr)
					return
				}

				// Create vote
				voteRKey := generateTID()
				voteEvent := &jetstream.JetstreamEvent{
					Did:  voterDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-%d", voterIndex),
						Operation:  "create",
						Collection: "social.coves.feed.vote",
						RKey:       voteRKey,
						CID:        fmt.Sprintf("bafyvote%d", voterIndex),
						Record: map[string]interface{}{
							"$type": "social.coves.feed.vote",
							"subject": map[string]interface{}{
								"uri": postURI,
								"cid": "bafypost",
							},
							"direction": "up",
							"createdAt": fixedTime.Format(time.RFC3339),
						},
					},
				}

				if handleErr := voteConsumer.HandleEvent(ctx, voteEvent); handleErr != nil {
					errors <- fmt.Errorf("voter %d: failed to handle vote event: %w", voterIndex, handleErr)
					return
				}
			}(i)
		}

		// Wait for all goroutines to complete
		wg.Wait()
		close(errors)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent voting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent voting, got %d errors", errorCount)
		}

		// Verify post vote counts are correct
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}

		if post.UpvoteCount != numVoters {
			t.Errorf("Expected upvote_count = %d, got %d (possible race condition in count update)", numVoters, post.UpvoteCount)
		}

		if post.Score != numVoters {
			t.Errorf("Expected score = %d, got %d (possible race condition in score calculation)", numVoters, post.Score)
		}

		// CRITICAL: Verify actual vote records in database to detect race conditions
		// This catches issues that aggregate counts might miss (e.g., duplicate votes, lost votes)
		var actualVoteCount int
		var distinctVoterCount int
		err = db.QueryRow("SELECT COUNT(*), COUNT(DISTINCT voter_did) FROM votes WHERE subject_uri = $1 AND direction = 'up'", postURI).
			Scan(&actualVoteCount, &distinctVoterCount)
		if err != nil {
			t.Fatalf("Failed to query vote records: %v", err)
		}

		if actualVoteCount != numVoters {
			t.Errorf("Expected %d vote records in database, got %d (possible race condition: votes lost or duplicated)", numVoters, actualVoteCount)
		}

		if distinctVoterCount != numVoters {
			t.Errorf("Expected %d distinct voters, got %d (possible race condition: duplicate votes from same voter)", numVoters, distinctVoterCount)
		}

		t.Logf("✓ %d concurrent upvotes processed correctly:", numVoters)
		t.Logf("  - Post counts: upvote_count=%d, score=%d", post.UpvoteCount, post.Score)
		t.Logf("  - Database records: %d votes from %d distinct voters (no duplicates)", actualVoteCount, distinctVoterCount)
	})

	t.Run("Concurrent upvotes and downvotes on same post", func(t *testing.T) {
		// Create a new post for this test
		testPost2URI := createTestPost(t, db, testCommunity, testUser.DID, "Post for mixed voting", 0, fixedTime)

		const numUpvoters = 15
		const numDownvoters = 10
		const totalVoters = numUpvoters + numDownvoters

		var wg sync.WaitGroup
		wg.Add(totalVoters)
		errors := make(chan error, totalVoters)

		// Upvoters
		for i := 0; i < numUpvoters; i++ {
			go func(voterIndex int) {
				defer wg.Done()

				voterDID := fmt.Sprintf("did:plc:upvoter%d", voterIndex)
				voterHandle := fmt.Sprintf("upvoter%d.test", voterIndex)

				_, createErr := userService.CreateUser(ctx, users.CreateUserRequest{
					DID:    voterDID,
					Handle: voterHandle,
					PDSURL: "http://localhost:3001",
				})
				if createErr != nil {
					errors <- fmt.Errorf("upvoter %d: failed to create user: %w", voterIndex, createErr)
					return
				}

				voteRKey := generateTID()
				voteEvent := &jetstream.JetstreamEvent{
					Did:  voterDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-up-%d", voterIndex),
						Operation:  "create",
						Collection: "social.coves.feed.vote",
						RKey:       voteRKey,
						CID:        fmt.Sprintf("bafyup%d", voterIndex),
						Record: map[string]interface{}{
							"$type": "social.coves.feed.vote",
							"subject": map[string]interface{}{
								"uri": testPost2URI,
								"cid": "bafypost2",
							},
							"direction": "up",
							"createdAt": fixedTime.Format(time.RFC3339),
						},
					},
				}

				if handleErr := voteConsumer.HandleEvent(ctx, voteEvent); handleErr != nil {
					errors <- fmt.Errorf("upvoter %d: failed to handle event: %w", voterIndex, handleErr)
				}
			}(i)
		}

		// Downvoters
		for i := 0; i < numDownvoters; i++ {
			go func(voterIndex int) {
				defer wg.Done()

				voterDID := fmt.Sprintf("did:plc:downvoter%d", voterIndex)
				voterHandle := fmt.Sprintf("downvoter%d.test", voterIndex)

				_, createErr := userService.CreateUser(ctx, users.CreateUserRequest{
					DID:    voterDID,
					Handle: voterHandle,
					PDSURL: "http://localhost:3001",
				})
				if createErr != nil {
					errors <- fmt.Errorf("downvoter %d: failed to create user: %w", voterIndex, createErr)
					return
				}

				voteRKey := generateTID()
				voteEvent := &jetstream.JetstreamEvent{
					Did:  voterDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-down-%d", voterIndex),
						Operation:  "create",
						Collection: "social.coves.feed.vote",
						RKey:       voteRKey,
						CID:        fmt.Sprintf("bafydown%d", voterIndex),
						Record: map[string]interface{}{
							"$type": "social.coves.feed.vote",
							"subject": map[string]interface{}{
								"uri": testPost2URI,
								"cid": "bafypost2",
							},
							"direction": "down",
							"createdAt": fixedTime.Format(time.RFC3339),
						},
					},
				}

				if handleErr := voteConsumer.HandleEvent(ctx, voteEvent); handleErr != nil {
					errors <- fmt.Errorf("downvoter %d: failed to handle event: %w", voterIndex, handleErr)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent mixed voting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent voting, got %d errors", errorCount)
		}

		// Verify counts
		post, err := postRepo.GetByURI(ctx, testPost2URI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}

		expectedScore := numUpvoters - numDownvoters
		if post.UpvoteCount != numUpvoters {
			t.Errorf("Expected upvote_count = %d, got %d", numUpvoters, post.UpvoteCount)
		}
		if post.DownvoteCount != numDownvoters {
			t.Errorf("Expected downvote_count = %d, got %d", numDownvoters, post.DownvoteCount)
		}
		if post.Score != expectedScore {
			t.Errorf("Expected score = %d, got %d", expectedScore, post.Score)
		}

		// CRITICAL: Verify actual vote records to detect race conditions
		var actualUpvotes, actualDownvotes, distinctUpvoters, distinctDownvoters int
		err = db.QueryRow(`
			SELECT
				COUNT(*) FILTER (WHERE direction = 'up'),
				COUNT(*) FILTER (WHERE direction = 'down'),
				COUNT(DISTINCT voter_did) FILTER (WHERE direction = 'up'),
				COUNT(DISTINCT voter_did) FILTER (WHERE direction = 'down')
			FROM votes WHERE subject_uri = $1
		`, testPost2URI).Scan(&actualUpvotes, &actualDownvotes, &distinctUpvoters, &distinctDownvoters)
		if err != nil {
			t.Fatalf("Failed to query vote records: %v", err)
		}

		if actualUpvotes != numUpvoters {
			t.Errorf("Expected %d upvote records, got %d (possible race condition)", numUpvoters, actualUpvotes)
		}
		if actualDownvotes != numDownvoters {
			t.Errorf("Expected %d downvote records, got %d (possible race condition)", numDownvoters, actualDownvotes)
		}
		if distinctUpvoters != numUpvoters {
			t.Errorf("Expected %d distinct upvoters, got %d (duplicate votes detected)", numUpvoters, distinctUpvoters)
		}
		if distinctDownvoters != numDownvoters {
			t.Errorf("Expected %d distinct downvoters, got %d (duplicate votes detected)", numDownvoters, distinctDownvoters)
		}

		t.Logf("✓ Concurrent mixed voting processed correctly:")
		t.Logf("  - Post counts: upvotes=%d, downvotes=%d, score=%d", post.UpvoteCount, post.DownvoteCount, post.Score)
		t.Logf("  - Database records: %d upvotes from %d voters, %d downvotes from %d voters (no duplicates)",
			actualUpvotes, distinctUpvoters, actualDownvotes, distinctDownvoters)
	})
}

// TestConcurrentCommenting_MultipleUsersOnSamePost tests race conditions when multiple users
// comment on the same post simultaneously
func TestConcurrentCommenting_MultipleUsersOnSamePost(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	fixedTime := time.Date(2025, 11, 16, 12, 0, 0, 0, time.UTC)

	// Setup: Create test community and post
	testCommunity, err := createFeedTestCommunity(db, ctx, "concurrent-comments", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	testUser := createTestUser(t, db, "author.test", "did:plc:author456")
	postURI := createTestPost(t, db, testCommunity, testUser.DID, "Post for concurrent commenting", 0, fixedTime)

	t.Run("Multiple users commenting simultaneously", func(t *testing.T) {
		const numCommenters = 25
		var wg sync.WaitGroup
		wg.Add(numCommenters)

		errors := make(chan error, numCommenters)
		commentURIs := make(chan string, numCommenters)

		for i := 0; i < numCommenters; i++ {
			go func(commenterIndex int) {
				defer wg.Done()

				commenterDID := fmt.Sprintf("did:plc:commenter%d", commenterIndex)
				commentRKey := fmt.Sprintf("%s-comment%d", generateTID(), commenterIndex)
				commentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", commenterDID, commentRKey)

				commentEvent := &jetstream.JetstreamEvent{
					Did:  commenterDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-comment-%d", commenterIndex),
						Operation:  "create",
						Collection: "social.coves.community.comment",
						RKey:       commentRKey,
						CID:        fmt.Sprintf("bafycomment%d", commenterIndex),
						Record: map[string]interface{}{
							"$type":   "social.coves.community.comment",
							"content": fmt.Sprintf("Concurrent comment #%d", commenterIndex),
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
							"createdAt": fixedTime.Add(time.Duration(commenterIndex) * time.Millisecond).Format(time.RFC3339),
						},
					},
				}

				if handleErr := commentConsumer.HandleEvent(ctx, commentEvent); handleErr != nil {
					errors <- fmt.Errorf("commenter %d: failed to handle comment event: %w", commenterIndex, handleErr)
					return
				}

				commentURIs <- commentURI
			}(i)
		}

		wg.Wait()
		close(errors)
		close(commentURIs)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent commenting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent commenting, got %d errors", errorCount)
		}

		// Verify post comment count updated correctly
		post, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}

		if post.CommentCount != numCommenters {
			t.Errorf("Expected comment_count = %d, got %d (possible race condition in count update)", numCommenters, post.CommentCount)
		}

		// CRITICAL: Verify actual comment records to detect race conditions
		var actualCommentCount int
		var distinctCommenters int
		err = db.QueryRow(`
			SELECT COUNT(*), COUNT(DISTINCT author_did)
			FROM comments
			WHERE post_uri = $1 AND parent_comment_uri IS NULL
		`, postURI).Scan(&actualCommentCount, &distinctCommenters)
		if err != nil {
			t.Fatalf("Failed to query comment records: %v", err)
		}

		if actualCommentCount != numCommenters {
			t.Errorf("Expected %d comment records in database, got %d (possible race condition: comments lost or duplicated)", numCommenters, actualCommentCount)
		}

		if distinctCommenters != numCommenters {
			t.Errorf("Expected %d distinct commenters, got %d (possible duplicate comments from same author)", numCommenters, distinctCommenters)
		}

		// Verify all comments are retrievable via service
		// Use factory constructor with nil factory - this test only uses the read path (GetComments)
		commentService := comments.NewCommentServiceWithPDSFactory(commentRepo, userRepo, postRepo, communityRepo, nil, nil)
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI:   postURI,
			Sort:      "new",
			Depth:     10,
			Limit:     100,
			ViewerDID: nil,
		})
		if err != nil {
			t.Fatalf("Failed to get comments: %v", err)
		}

		if len(response.Comments) != numCommenters {
			t.Errorf("Expected %d comments in response, got %d", numCommenters, len(response.Comments))
		}

		t.Logf("✓ %d concurrent comments processed correctly:", numCommenters)
		t.Logf("  - Post comment_count: %d", post.CommentCount)
		t.Logf("  - Database records: %d comments from %d distinct authors (no duplicates)", actualCommentCount, distinctCommenters)
	})

	t.Run("Concurrent replies to same comment", func(t *testing.T) {
		// Create a parent comment first
		parentCommentRKey := generateTID()
		parentCommentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, parentCommentRKey)

		parentEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "parent-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       parentCommentRKey,
				CID:        "bafyparent",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Parent comment for replies",
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
					"createdAt": fixedTime.Format(time.RFC3339),
				},
			},
		}

		if err := commentConsumer.HandleEvent(ctx, parentEvent); err != nil {
			t.Fatalf("Failed to create parent comment: %v", err)
		}

		// Now create concurrent replies
		const numRepliers = 15
		var wg sync.WaitGroup
		wg.Add(numRepliers)
		errors := make(chan error, numRepliers)

		for i := 0; i < numRepliers; i++ {
			go func(replierIndex int) {
				defer wg.Done()

				replierDID := fmt.Sprintf("did:plc:replier%d", replierIndex)
				replyRKey := fmt.Sprintf("%s-reply%d", generateTID(), replierIndex)

				replyEvent := &jetstream.JetstreamEvent{
					Did:  replierDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-reply-%d", replierIndex),
						Operation:  "create",
						Collection: "social.coves.community.comment",
						RKey:       replyRKey,
						CID:        fmt.Sprintf("bafyreply%d", replierIndex),
						Record: map[string]interface{}{
							"$type":   "social.coves.community.comment",
							"content": fmt.Sprintf("Concurrent reply #%d", replierIndex),
							"reply": map[string]interface{}{
								"root": map[string]interface{}{
									"uri": postURI,
									"cid": "bafypost",
								},
								"parent": map[string]interface{}{
									"uri": parentCommentURI,
									"cid": "bafyparent",
								},
							},
							"createdAt": fixedTime.Add(time.Duration(replierIndex) * time.Millisecond).Format(time.RFC3339),
						},
					},
				}

				if handleErr := commentConsumer.HandleEvent(ctx, replyEvent); handleErr != nil {
					errors <- fmt.Errorf("replier %d: failed to handle reply event: %w", replierIndex, handleErr)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent replying: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent replying, got %d errors", errorCount)
		}

		// Verify parent comment reply count
		parentComment, err := commentRepo.GetByURI(ctx, parentCommentURI)
		if err != nil {
			t.Fatalf("Failed to get parent comment: %v", err)
		}

		if parentComment.ReplyCount != numRepliers {
			t.Errorf("Expected reply_count = %d on parent comment, got %d (possible race condition)", numRepliers, parentComment.ReplyCount)
		}

		t.Logf("✓ %d concurrent replies processed correctly, reply_count=%d", numRepliers, parentComment.ReplyCount)
	})
}

// TestConcurrentCommunityCreation tests race conditions when multiple goroutines
// try to create communities with the same handle
func TestConcurrentCommunityCreation_DuplicateHandle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	repo := postgres.NewCommunityRepository(db)

	t.Run("Concurrent creation with same handle should fail", func(t *testing.T) {
		const numAttempts = 10
		sameHandle := fmt.Sprintf("duplicate-handle-%d.test.coves.social", time.Now().UnixNano())

		var wg sync.WaitGroup
		wg.Add(numAttempts)

		type result struct {
			err     error
			success bool
		}
		results := make(chan result, numAttempts)

		for i := 0; i < numAttempts; i++ {
			go func(attemptIndex int) {
				defer wg.Done()

				// Each attempt uses a unique DID but same handle
				uniqueDID := fmt.Sprintf("did:plc:dup-community-%d-%d", time.Now().UnixNano(), attemptIndex)

				community := &communities.Community{
					DID:          uniqueDID,
					Handle:       sameHandle, // SAME HANDLE
					Name:         fmt.Sprintf("dup-test-%d", attemptIndex),
					DisplayName:  fmt.Sprintf("Duplicate Test %d", attemptIndex),
					Description:  "Testing duplicate handle prevention",
					OwnerDID:     "did:web:test.local",
					CreatedByDID: "did:plc:creator",
					HostedByDID:  "did:web:test.local",
					Visibility:   "public",
					CreatedAt:    time.Now(),
					UpdatedAt:    time.Now(),
				}

				_, createErr := repo.Create(ctx, community)
				results <- result{
					success: createErr == nil,
					err:     createErr,
				}
			}(i)
		}

		wg.Wait()
		close(results)

		// Collect results
		successCount := 0
		duplicateErrors := 0

		for res := range results {
			if res.success {
				successCount++
			} else if communities.IsConflict(res.err) {
				duplicateErrors++
			} else {
				t.Logf("Unexpected error type: %v", res.err)
			}
		}

		// CRITICAL: Exactly ONE should succeed, rest should fail with duplicate error
		if successCount != 1 {
			t.Errorf("Expected exactly 1 successful creation, got %d (DATABASE CONSTRAINT VIOLATION - race condition detected)", successCount)
		}

		if duplicateErrors != numAttempts-1 {
			t.Errorf("Expected %d duplicate errors, got %d", numAttempts-1, duplicateErrors)
		}

		t.Logf("✓ Duplicate handle protection: %d successful, %d duplicate errors (database constraint working)", successCount, duplicateErrors)
	})

	t.Run("Concurrent creation with different handles should succeed", func(t *testing.T) {
		const numAttempts = 10
		var wg sync.WaitGroup
		wg.Add(numAttempts)

		errors := make(chan error, numAttempts)

		for i := 0; i < numAttempts; i++ {
			go func(attemptIndex int) {
				defer wg.Done()

				uniqueSuffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), attemptIndex)
				community := &communities.Community{
					DID:          generateTestDID(uniqueSuffix),
					Handle:       fmt.Sprintf("unique-handle-%s.test.coves.social", uniqueSuffix),
					Name:         fmt.Sprintf("unique-test-%s", uniqueSuffix),
					DisplayName:  fmt.Sprintf("Unique Test %d", attemptIndex),
					Description:  "Testing concurrent unique handle creation",
					OwnerDID:     "did:web:test.local",
					CreatedByDID: "did:plc:creator",
					HostedByDID:  "did:web:test.local",
					Visibility:   "public",
					CreatedAt:    time.Now(),
					UpdatedAt:    time.Now(),
				}

				_, createErr := repo.Create(ctx, community)
				if createErr != nil {
					errors <- createErr
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// All should succeed
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent unique creation: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected all %d creations to succeed, but %d failed", numAttempts, errorCount)
		}

		t.Logf("✓ All %d concurrent community creations with unique handles succeeded", numAttempts)
	})
}

// TestConcurrentSubscription tests race conditions when multiple users subscribe
// to the same community simultaneously
func TestConcurrentSubscription_RaceConditions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()
	communityRepo := postgres.NewCommunityRepository(db)
	consumer := jetstream.NewCommunityEventConsumer(communityRepo, "did:web:coves.local", true, nil)

	// Create test community
	testDID := fmt.Sprintf("did:plc:test-sub-race-%d", time.Now().UnixNano())
	community := &communities.Community{
		DID:          testDID,
		Handle:       fmt.Sprintf("sub-race-%d.test.coves.social", time.Now().UnixNano()),
		Name:         "sub-race-test",
		DisplayName:  "Subscription Race Test",
		Description:  "Testing subscription race conditions",
		OwnerDID:     "did:plc:owner",
		CreatedByDID: "did:plc:creator",
		HostedByDID:  "did:web:coves.local",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	created, err := communityRepo.Create(ctx, community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	t.Run("Multiple users subscribing concurrently", func(t *testing.T) {
		const numSubscribers = 30
		var wg sync.WaitGroup
		wg.Add(numSubscribers)

		errors := make(chan error, numSubscribers)

		for i := 0; i < numSubscribers; i++ {
			go func(subscriberIndex int) {
				defer wg.Done()

				userDID := fmt.Sprintf("did:plc:subscriber%d", subscriberIndex)
				rkey := fmt.Sprintf("sub-%d", subscriberIndex)

				event := &jetstream.JetstreamEvent{
					Did:    userDID,
					Kind:   "commit",
					TimeUS: time.Now().UnixMicro(),
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-%d", subscriberIndex),
						Operation:  "create",
						Collection: "social.coves.community.subscription",
						RKey:       rkey,
						CID:        fmt.Sprintf("bafysub%d", subscriberIndex),
						Record: map[string]interface{}{
							"$type":             "social.coves.community.subscription",
							"subject":           created.DID,
							"createdAt":         time.Now().Format(time.RFC3339),
							"contentVisibility": float64(3),
						},
					},
				}

				if handleErr := consumer.HandleEvent(ctx, event); handleErr != nil {
					errors <- fmt.Errorf("subscriber %d: failed to subscribe: %w", subscriberIndex, handleErr)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent subscription: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent subscription, got %d errors", errorCount)
		}

		// Verify subscriber count is correct
		updatedCommunity, err := communityRepo.GetByDID(ctx, created.DID)
		if err != nil {
			t.Fatalf("Failed to get updated community: %v", err)
		}

		if updatedCommunity.SubscriberCount != numSubscribers {
			t.Errorf("Expected subscriber_count = %d, got %d (RACE CONDITION in subscriber count update)", numSubscribers, updatedCommunity.SubscriberCount)
		}

		// CRITICAL: Verify actual subscription records to detect race conditions
		var actualSubscriptionCount int
		var distinctSubscribers int
		err = db.QueryRow(`
			SELECT COUNT(*), COUNT(DISTINCT user_did)
			FROM community_subscriptions
			WHERE community_did = $1
		`, created.DID).Scan(&actualSubscriptionCount, &distinctSubscribers)
		if err != nil {
			t.Fatalf("Failed to query subscription records: %v", err)
		}

		if actualSubscriptionCount != numSubscribers {
			t.Errorf("Expected %d subscription records, got %d (possible race condition: subscriptions lost or duplicated)", numSubscribers, actualSubscriptionCount)
		}

		if distinctSubscribers != numSubscribers {
			t.Errorf("Expected %d distinct subscribers, got %d (possible duplicate subscriptions)", numSubscribers, distinctSubscribers)
		}

		t.Logf("✓ %d concurrent subscriptions processed correctly:", numSubscribers)
		t.Logf("  - Community subscriber_count: %d", updatedCommunity.SubscriberCount)
		t.Logf("  - Database records: %d subscriptions from %d distinct users (no duplicates)", actualSubscriptionCount, distinctSubscribers)
	})

	t.Run("Concurrent subscribe and unsubscribe", func(t *testing.T) {
		// Create new community for this test
		testDID2 := fmt.Sprintf("did:plc:test-sub-unsub-%d", time.Now().UnixNano())
		community2 := &communities.Community{
			DID:          testDID2,
			Handle:       fmt.Sprintf("sub-unsub-%d.test.coves.social", time.Now().UnixNano()),
			Name:         "sub-unsub-test",
			DisplayName:  "Subscribe/Unsubscribe Race Test",
			Description:  "Testing concurrent subscribe/unsubscribe",
			OwnerDID:     "did:plc:owner",
			CreatedByDID: "did:plc:creator",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		created2, err := communityRepo.Create(ctx, community2)
		if err != nil {
			t.Fatalf("Failed to create test community: %v", err)
		}

		const numUsers = 20
		var wg sync.WaitGroup
		wg.Add(numUsers * 2) // Each user subscribes then unsubscribes

		errors := make(chan error, numUsers*2)

		for i := 0; i < numUsers; i++ {
			go func(userIndex int) {
				userDID := fmt.Sprintf("did:plc:subunsubuser%d", userIndex)
				rkey := fmt.Sprintf("subunsub-%d", userIndex)

				// Subscribe
				subscribeEvent := &jetstream.JetstreamEvent{
					Did:    userDID,
					Kind:   "commit",
					TimeUS: time.Now().UnixMicro(),
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-sub-%d", userIndex),
						Operation:  "create",
						Collection: "social.coves.community.subscription",
						RKey:       rkey,
						CID:        fmt.Sprintf("bafysubscribe%d", userIndex),
						Record: map[string]interface{}{
							"$type":             "social.coves.community.subscription",
							"subject":           created2.DID,
							"createdAt":         time.Now().Format(time.RFC3339),
							"contentVisibility": float64(3),
						},
					},
				}

				if handleErr := consumer.HandleEvent(ctx, subscribeEvent); handleErr != nil {
					errors <- fmt.Errorf("user %d: subscribe failed: %w", userIndex, handleErr)
				}
				wg.Done()

				// Small delay to ensure subscribe happens first
				time.Sleep(10 * time.Millisecond)

				// Unsubscribe
				unsubscribeEvent := &jetstream.JetstreamEvent{
					Did:    userDID,
					Kind:   "commit",
					TimeUS: time.Now().UnixMicro(),
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-unsub-%d", userIndex),
						Operation:  "delete",
						Collection: "social.coves.community.subscription",
						RKey:       rkey,
						CID:        "",
						Record:     nil,
					},
				}

				if handleErr := consumer.HandleEvent(ctx, unsubscribeEvent); handleErr != nil {
					errors <- fmt.Errorf("user %d: unsubscribe failed: %w", userIndex, handleErr)
				}
				wg.Done()
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for errors
		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent sub/unsub: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent sub/unsub, got %d errors", errorCount)
		}

		// Final subscriber count should be 0 (all unsubscribed)
		finalCommunity, err := communityRepo.GetByDID(ctx, created2.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		if finalCommunity.SubscriberCount != 0 {
			t.Errorf("Expected subscriber_count = 0 after all unsubscribed, got %d (RACE CONDITION detected)", finalCommunity.SubscriberCount)
		}

		// CRITICAL: Verify no subscription records remain in database
		var remainingSubscriptions int
		err = db.QueryRow(`
			SELECT COUNT(*)
			FROM community_subscriptions
			WHERE community_did = $1
		`, created2.DID).Scan(&remainingSubscriptions)
		if err != nil {
			t.Fatalf("Failed to query subscription records: %v", err)
		}

		if remainingSubscriptions != 0 {
			t.Errorf("Expected 0 subscription records after all unsubscribed, got %d (orphaned subscriptions detected)", remainingSubscriptions)
		}

		t.Logf("✓ Concurrent subscribe/unsubscribe handled correctly:")
		t.Logf("  - Community subscriber_count: %d", finalCommunity.SubscriberCount)
		t.Logf("  - Database records: %d subscriptions remaining (clean unsubscribe)", remainingSubscriptions)
	})
}
