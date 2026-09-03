//go:build integration

package postgres_test

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Row-level concurrency for the counters and unique indexes this package's
// repositories maintain.
//
// Every counter the AppView serves — a post's upvote_count, score and
// comment_count, a comment's reply_count, a community's subscriber_count — is
// denormalised: the firehose consumers insert a row and adjust a number in the
// same transaction, and the firehose delivers events for one subject in parallel.
// Whether that arithmetic survives contention is decided entirely by the SQL:
// whether the UPDATE reads the counter inside the transaction or outside it, and
// whether the unique index is the thing that arbitrates a duplicate rather than a
// prior SELECT. A mock repository cannot be wrong about any of it, and a
// single-threaded test would pass against an implementation that loses half its
// writes.
//
// So the tests below hammer one row from many goroutines and then assert TWICE:
// once on the denormalised counter, and once on a COUNT/COUNT(DISTINCT) over the
// underlying rows. The second assertion is the load-bearing one — a counter and a
// row set that disagree is exactly what a lost update looks like, and checking
// only the counter would miss votes that were written twice and counted once.
//
// The events are handed to the consumers directly rather than published to a
// Jetstream: what is under test is the write path's behaviour under contention,
// and a real socket would add delivery ordering to the list of things that could
// explain a failure. Every test therefore needs Postgres and nothing else, which
// is the floor this package's TestMain sets.
//
// This file is in the external test package because it uses Coves/tests/fixtures
// for row seeding; see that package's doc comment for why its callers must be.

// TestConcurrentVoting_MultipleUsersOnSamePost drives many vote commits at one
// post at once. votes carries a unique index on (voter_did, subject_uri), and the
// consumer updates the post's upvote_count, downvote_count and score in the same
// transaction that inserts the vote.
func TestConcurrentVoting_MultipleUsersOnSamePost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	postRepo := postgres.NewPostRepository(db)
	userService := users.NewUserService(
		postgres.NewUserRepository(db), nil, testkit.Endpoints().PDS.BaseURL, nil, "")
	voteConsumer := jetstream.NewVoteEventConsumer(postgres.NewVoteRepository(db), userService, db)

	// A fixed timestamp keeps the seeded rows out of the assertions: nothing
	// here depends on age, and a moving createdAt would only add noise.
	fixedTime := time.Date(2025, 11, 16, 12, 0, 0, 0, time.UTC)

	testID := testkit.UniqueID(t)
	testCommunity, err := fixtures.Community(ctx, db, "convotes-"+testID, "owner-"+testID+".test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	authorDID := "did:plc:author" + testID
	fixtures.User(t, db, "author-"+testID+".test", authorDID)
	postURI := fixtures.Post(t, db, testCommunity, authorDID, "Post for concurrent voting", 0, fixedTime)

	t.Run("Multiple users upvoting same post concurrently", func(t *testing.T) {
		const numVoters = 20
		var wg sync.WaitGroup
		wg.Add(numVoters)

		errors := make(chan error, numVoters)

		for i := 0; i < numVoters; i++ {
			go func(voterIndex int) {
				defer wg.Done()

				voterDID := fmt.Sprintf("did:plc:voter%d", voterIndex)

				// The consumer indexes the voter on demand, so creating the user
				// concurrently is part of the contention being tested.
				_, createErr := userService.CreateUser(ctx, users.CreateUserRequest{
					DID:    voterDID,
					Handle: fmt.Sprintf("voter%d.test", voterIndex),
					PDSURL: testkit.Endpoints().PDS.BaseURL,
				})
				if createErr != nil {
					errors <- fmt.Errorf("voter %d: failed to create user: %w", voterIndex, createErr)
					return
				}

				voteEvent := &jetstream.JetstreamEvent{
					Did:  voterDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-%d", voterIndex),
						Operation:  "create",
						Collection: "social.coves.feed.vote",
						RKey:       testkit.TID(),
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

		wg.Wait()
		close(errors)

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent voting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent voting, got %d errors", errorCount)
		}

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}

		if post.UpvoteCount != numVoters {
			t.Errorf("Expected upvote_count = %d, got %d (possible race condition in count update)", numVoters, post.UpvoteCount)
		}

		if post.Score != numVoters {
			t.Errorf("Expected score = %d, got %d (possible race condition in score calculation)", numVoters, post.Score)
		}

		// The rows themselves, not just the counter: a counter that matches over
		// a row set that does not is a lost update wearing a correct answer.
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
	})

	t.Run("Concurrent upvotes and downvotes on same post", func(t *testing.T) {
		// Mixed directions matter separately: score is the difference of two
		// counters, so an update that is atomic per direction can still be wrong
		// when both directions contend for the same row.
		testPost2URI := fixtures.Post(t, db, testCommunity, authorDID, "Post for mixed voting", 0, fixedTime)

		const numUpvoters = 15
		const numDownvoters = 10
		const totalVoters = numUpvoters + numDownvoters

		var wg sync.WaitGroup
		wg.Add(totalVoters)
		errors := make(chan error, totalVoters)

		castVote := func(direction, label string, voterIndex int) {
			defer wg.Done()

			voterDID := fmt.Sprintf("did:plc:%s%d", label, voterIndex)

			_, createErr := userService.CreateUser(ctx, users.CreateUserRequest{
				DID:    voterDID,
				Handle: fmt.Sprintf("%s%d.test", label, voterIndex),
				PDSURL: testkit.Endpoints().PDS.BaseURL,
			})
			if createErr != nil {
				errors <- fmt.Errorf("%s %d: failed to create user: %w", label, voterIndex, createErr)
				return
			}

			voteEvent := &jetstream.JetstreamEvent{
				Did:  voterDID,
				Kind: "commit",
				Commit: &jetstream.CommitEvent{
					Rev:        fmt.Sprintf("rev-%s-%d", label, voterIndex),
					Operation:  "create",
					Collection: "social.coves.feed.vote",
					RKey:       testkit.TID(),
					CID:        fmt.Sprintf("bafy%s%d", label, voterIndex),
					Record: map[string]interface{}{
						"$type": "social.coves.feed.vote",
						"subject": map[string]interface{}{
							"uri": testPost2URI,
							"cid": "bafypost2",
						},
						"direction": direction,
						"createdAt": fixedTime.Format(time.RFC3339),
					},
				},
			}

			if handleErr := voteConsumer.HandleEvent(ctx, voteEvent); handleErr != nil {
				errors <- fmt.Errorf("%s %d: failed to handle event: %w", label, voterIndex, handleErr)
			}
		}

		for i := 0; i < numUpvoters; i++ {
			go castVote("up", "upvoter", i)
		}
		for i := 0; i < numDownvoters; i++ {
			go castVote("down", "downvoter", i)
		}

		wg.Wait()
		close(errors)

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent mixed voting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent voting, got %d errors", errorCount)
		}

		post, err := postRepo.GetRawIndexedRow(ctx, testPost2URI)
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
	})
}

// TestConcurrentCommenting_MultipleUsersOnSamePost drives many comment commits at
// one post at once. The consumer inserts the comment and increments the post's
// comment_count — or the parent comment's reply_count — in one transaction, so
// both counters are exposed to the same lost-update risk as the vote counts.
func TestConcurrentCommenting_MultipleUsersOnSamePost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	fixedTime := time.Date(2025, 11, 16, 12, 0, 0, 0, time.UTC)

	testID := testkit.UniqueID(t)
	testCommunity, err := fixtures.Community(ctx, db, "concomments-"+testID, "owner-"+testID+".test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	authorDID := "did:plc:author" + testID
	fixtures.User(t, db, "author-"+testID+".test", authorDID)
	postURI := fixtures.Post(t, db, testCommunity, authorDID, "Post for concurrent commenting", 0, fixedTime)

	t.Run("Multiple users commenting simultaneously", func(t *testing.T) {
		const numCommenters = 25
		var wg sync.WaitGroup
		wg.Add(numCommenters)

		errors := make(chan error, numCommenters)

		for i := 0; i < numCommenters; i++ {
			go func(commenterIndex int) {
				defer wg.Done()

				commenterDID := fmt.Sprintf("did:plc:commenter%d", commenterIndex)
				commentRKey := fmt.Sprintf("%s-comment%d", testkit.TID(), commenterIndex)

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
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent commenting: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent commenting, got %d errors", errorCount)
		}

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to get post: %v", err)
		}

		if post.CommentCount != numCommenters {
			t.Errorf("Expected comment_count = %d, got %d (possible race condition in count update)", numCommenters, post.CommentCount)
		}

		var actualCommentCount int
		var distinctCommenters int
		err = db.QueryRow(`
			SELECT COUNT(*), COUNT(DISTINCT commenter_did)
			FROM comments
			WHERE root_uri = $1 AND parent_uri = root_uri
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

		// The read path has to agree with the row count: a thread query that
		// dropped rows under contention would still leave the counters right.
		// A nil PDS-client factory is enough because only GetComments is used.
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
	})

	t.Run("Concurrent replies to same comment", func(t *testing.T) {
		// reply_count lives on the parent comment row rather than the post, so
		// it is a second counter with its own UPDATE and its own race.
		parentCommentRKey := testkit.TID()
		parentCommentURI := fmt.Sprintf("at://%s/social.coves.community.comment/%s", authorDID, parentCommentRKey)

		parentEvent := &jetstream.JetstreamEvent{
			Did:  authorDID,
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

		const numRepliers = 15
		var wg sync.WaitGroup
		wg.Add(numRepliers)
		errors := make(chan error, numRepliers)

		for i := 0; i < numRepliers; i++ {
			go func(replierIndex int) {
				defer wg.Done()

				replierDID := fmt.Sprintf("did:plc:replier%d", replierIndex)

				replyEvent := &jetstream.JetstreamEvent{
					Did:  replierDID,
					Kind: "commit",
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-reply-%d", replierIndex),
						Operation:  "create",
						Collection: "social.coves.community.comment",
						RKey:       fmt.Sprintf("%s-reply%d", testkit.TID(), replierIndex),
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

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent replying: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent replying, got %d errors", errorCount)
		}

		parentComment, err := commentRepo.GetByURI(ctx, parentCommentURI)
		if err != nil {
			t.Fatalf("Failed to get parent comment: %v", err)
		}

		if parentComment.ReplyCount != numRepliers {
			t.Errorf("Expected reply_count = %d on parent comment, got %d (possible race condition)", numRepliers, parentComment.ReplyCount)
		}
	})
}

// TestConcurrentCommunityCreation_DuplicateHandle asserts the handle uniqueness
// guarantee is the database's, not the application's.
//
// A check-then-insert in Go is a race: two goroutines can both find the handle
// free and both insert. The only correct arbiter is the unique index, and the
// only way to prove the repository defers to it — and maps its violation to a
// conflict error rather than leaking a driver error — is to make many goroutines
// race for one handle and require that EXACTLY one wins.
func TestConcurrentCommunityCreation_DuplicateHandle(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())

	t.Run("Concurrent creation with same handle should fail", func(t *testing.T) {
		const numAttempts = 10
		// Identifiers are minted on the test goroutine: testkit.UniqueID reports
		// failure through t, which is only valid outside a spawned goroutine.
		runID := testkit.UniqueID(t)
		sameHandle := fmt.Sprintf("duplicate-handle-%s.test.coves.social", runID)

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

				// Distinct DIDs, so the DID index cannot be what rejects these.
				community := &communities.Community{
					DID:          fmt.Sprintf("did:plc:dupcommunity%s%d", runID, attemptIndex),
					Handle:       sameHandle,
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

		successCount := 0
		duplicateErrors := 0

		for res := range results {
			if res.success {
				successCount++
			} else if communities.IsConflict(res.err) {
				duplicateErrors++
			} else {
				t.Errorf("Expected a conflict error, got %T: %v", res.err, res.err)
			}
		}

		if successCount != 1 {
			t.Errorf("Expected exactly 1 successful creation, got %d (DATABASE CONSTRAINT VIOLATION - race condition detected)", successCount)
		}

		if duplicateErrors != numAttempts-1 {
			t.Errorf("Expected %d duplicate errors, got %d", numAttempts-1, duplicateErrors)
		}
	})

	t.Run("Concurrent creation with different handles should succeed", func(t *testing.T) {
		// The control case: without it, a repository that rejected every
		// concurrent insert would pass the test above.
		const numAttempts = 10
		var wg sync.WaitGroup
		wg.Add(numAttempts)

		// Minted up front, for the same reason as above.
		ids := make([]string, numAttempts)
		for i := range ids {
			ids[i] = testkit.UniqueID(t)
		}

		errors := make(chan error, numAttempts)

		for i := 0; i < numAttempts; i++ {
			go func(attemptIndex int) {
				defer wg.Done()

				id := ids[attemptIndex]
				community := &communities.Community{
					DID:          "did:plc:test" + id,
					Handle:       fmt.Sprintf("unique-handle-%s.test.coves.social", id),
					Name:         fmt.Sprintf("unique-test-%s", id),
					DisplayName:  fmt.Sprintf("Unique Test %d", attemptIndex),
					Description:  "Testing concurrent unique handle creation",
					OwnerDID:     "did:web:test.local",
					CreatedByDID: "did:plc:creator",
					HostedByDID:  "did:web:test.local",
					Visibility:   "public",
					CreatedAt:    time.Now(),
					UpdatedAt:    time.Now(),
				}

				if _, createErr := repo.Create(ctx, community); createErr != nil {
					errors <- createErr
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent unique creation: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected all %d creations to succeed, but %d failed", numAttempts, errorCount)
		}
	})
}

// TestConcurrentSubscription_RaceConditions drives subscription commits at one
// community at once. subscriber_count is the counter most visible to users and
// the one most exposed: a database trigger adjusts it alongside every hard
// insert and hard delete of a subscription row.
func TestConcurrentSubscription_RaceConditions(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	// did:web verification is skipped: these events are synthesised locally and
	// there is no PLC directory in this package's infrastructure floor.
	consumer := jetstream.NewCommunityEventConsumer(communityRepo, "did:web:coves.local", true, nil)

	id := testkit.UniqueID(t)
	community := &communities.Community{
		DID:          "did:plc:testsubrace" + id,
		Handle:       fmt.Sprintf("sub-race-%s.test.coves.social", id),
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

				event := &jetstream.JetstreamEvent{
					Did:    fmt.Sprintf("did:plc:subscriber%d", subscriberIndex),
					Kind:   "commit",
					TimeUS: time.Now().UnixMicro(),
					Commit: &jetstream.CommitEvent{
						Rev:        fmt.Sprintf("rev-%d", subscriberIndex),
						Operation:  "create",
						Collection: "social.coves.community.subscription",
						RKey:       fmt.Sprintf("sub-%d", subscriberIndex),
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

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent subscription: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent subscription, got %d errors", errorCount)
		}

		updatedCommunity, err := communityRepo.GetByDID(ctx, created.DID)
		if err != nil {
			t.Fatalf("Failed to get updated community: %v", err)
		}

		if updatedCommunity.SubscriberCount != numSubscribers {
			t.Errorf("Expected subscriber_count = %d, got %d (RACE CONDITION in subscriber count update)", numSubscribers, updatedCommunity.SubscriberCount)
		}

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
	})

	t.Run("Concurrent subscribe and unsubscribe", func(t *testing.T) {
		// Subscribe and unsubscribe move subscriber_count in opposite directions,
		// so the pair is where a non-transactional counter drifts: every user
		// nets to zero, and any residue is an increment or decrement that was
		// applied against a stale read.
		id := testkit.UniqueID(t)
		community2 := &communities.Community{
			DID:          "did:plc:testsubunsub" + id,
			Handle:       fmt.Sprintf("sub-unsub-%s.test.coves.social", id),
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
		wg.Add(numUsers)

		errors := make(chan error, numUsers*2)

		for i := 0; i < numUsers; i++ {
			go func(userIndex int) {
				defer wg.Done()

				userDID := fmt.Sprintf("did:plc:subunsubuser%d", userIndex)
				rkey := fmt.Sprintf("subunsub-%d", userIndex)

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

				// No pause between the two: HandleEvent is synchronous and
				// returns only once its transaction has committed, so the
				// subscription row is durable before the delete is submitted.
				// The contention this test is after is between users, not
				// within one.
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
			}(i)
		}

		wg.Wait()
		close(errors)

		var errorCount int
		for err := range errors {
			t.Logf("Error during concurrent sub/unsub: %v", err)
			errorCount++
		}

		if errorCount > 0 {
			t.Errorf("Expected no errors during concurrent sub/unsub, got %d errors", errorCount)
		}

		finalCommunity, err := communityRepo.GetByDID(ctx, created2.DID)
		if err != nil {
			t.Fatalf("Failed to get final community: %v", err)
		}

		if finalCommunity.SubscriberCount != 0 {
			t.Errorf("Expected subscriber_count = 0 after all unsubscribed, got %d (RACE CONDITION detected)", finalCommunity.SubscriberCount)
		}

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
	})
}
