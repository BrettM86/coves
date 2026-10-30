//go:build integration

// Votes cast on comments: the counters the vote consumer maintains on a comment
// row.
//
// This is the one place they are proved against real SQL. The counters
// (upvote_count, downvote_count, score) are maintained by the vote consumer
// inside the same transaction as the vote row, so no fake can stand in for
// them. The viewer's own vote state is filled by the comment handlers, not the
// comment service (docs/PRD_VOTE_STATE_FROM_INDEX.md §4).
//
// It lives beside the comments domain rather than beside votes because what is
// asserted is the state of a COMMENT: the vote record is the input, the comment
// row is the output under test.
//
// The counters are driven by feeding synthetic Jetstream events to the comment
// and vote consumers, which is how those rows are written in production; see
// comment_consumer_test.go for why the consumer is fed directly rather than
// over a websocket.

package comments_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
)

// TestCommentVote_CreateAndUpdate tests voting on comments and vote count updates
func TestCommentVote_CreateAndUpdate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	voteRepo := postgres.NewVoteRepository(db)
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, nil, testkit.Endpoints().PDS.BaseURL, nil, "")

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	// Use fixed timestamp to prevent flaky tests
	fixedTime := time.Date(2025, 11, 6, 12, 0, 0, 0, time.UTC)

	// Setup test data
	testUser := fixtures.User(t, db, "voter.test", "did:plc:voter123")
	testCommunity, err := fixtures.Community(ctx, db, "testcommunity", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	testPostURI := fixtures.Post(t, db, testCommunity, testUser.DID, "Test Post", 0, fixedTime)

	t.Run("Upvote on comment increments count", func(t *testing.T) {
		// Create a comment
		commentRKey := testkit.TID()
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
		voteRKey := testkit.TID()
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
		commentRKey := testkit.TID()
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
		voteRKey := testkit.TID()

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
		commentRKey := testkit.TID()
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
		voteRKey := testkit.TID()

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

		// Delete vote. The rev must be LATER than the create's: revs are the
		// repo's monotonic commit IDs, and the rev gate treats an equal rev as
		// the same event replayed (a no-op by design).
		deleteVoteEvent := &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-2",
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
