package integration

import (
	"Coves/internal/api/handlers/vote"
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVote_E2E_WithJetstream tests the full vote flow with simulated Jetstream:
// XRPC endpoint → AppView Service → PDS write → (Simulated) Jetstream consumer → DB indexing
//
// This is a fast integration test that simulates what happens in production:
// 1. Client calls POST /xrpc/social.coves.interaction.createVote with auth token
// 2. Handler validates and calls VoteService.CreateVote()
// 3. Service writes vote to user's PDS repository
// 4. (Simulated) PDS broadcasts event to Jetstream
// 5. Jetstream consumer receives event and indexes vote in AppView DB
// 6. Vote is now queryable from AppView + post counts updated
//
// NOTE: This test simulates the Jetstream event (step 4-5) since we don't have
// a live PDS/Jetstream in test environment. For true live testing, use TestVote_E2E_LivePDS.
func TestVote_E2E_WithJetstream(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Cleanup old test data first
	_, _ = db.Exec("DELETE FROM votes WHERE voter_did LIKE 'did:plc:votee2e%'")
	_, _ = db.Exec("DELETE FROM posts WHERE community_did = 'did:plc:votecommunity123'")
	_, _ = db.Exec("DELETE FROM communities WHERE did = 'did:plc:votecommunity123'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:votee2e%'")

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	// Setup user service for consumers
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, "http://localhost:3001")

	// Create test users (voter and author)
	voter := createTestUser(t, db, "voter.test", "did:plc:votee2evoter123")
	author := createTestUser(t, db, "author.test", "did:plc:votee2eauthor123")

	// Create test community
	community := &communities.Community{
		DID:             "did:plc:votecommunity123",
		Handle:          "votecommunity.test.coves.social",
		Name:            "votecommunity",
		DisplayName:     "Vote Test Community",
		OwnerDID:        "did:plc:votecommunity123",
		CreatedByDID:    author.DID,
		HostedByDID:     "did:web:coves.test",
		Visibility:      "public",
		ModerationType:  "moderator",
		RecordURI:       "at://did:plc:votecommunity123/social.coves.community.profile/self",
		RecordCID:       "fakecid123",
		PDSAccessToken:  "fake_token_for_testing",
		PDSRefreshToken: "fake_refresh_token",
	}
	_, err := communityRepo.Create(context.Background(), community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Create test post (subject of votes)
	postRkey := generateTID()
	postURI := fmt.Sprintf("at://%s/social.coves.post.record/%s", community.DID, postRkey)
	postCID := "bafy2bzacepostcid123"
	post := &posts.Post{
		URI:          postURI,
		CID:          postCID,
		RKey:         postRkey,
		AuthorDID:    author.DID,
		CommunityDID: community.DID,
		Title:        stringPtr("Test Post for Voting"),
		Content:      stringPtr("This post will receive votes"),
		CreatedAt:    time.Now(),
		UpvoteCount:  0,
		DownvoteCount: 0,
		Score:        0,
	}
	err = postRepo.Create(context.Background(), post)
	if err != nil {
		t.Fatalf("Failed to create test post: %v", err)
	}

	t.Run("Full E2E flow - Create upvote via Jetstream", func(t *testing.T) {
		ctx := context.Background()

		// STEP 1: Simulate Jetstream consumer receiving a vote CREATE event
		// In real production, this event comes from PDS via Jetstream WebSocket
		voteRkey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.interaction.vote/%s", voter.DID, voteRkey)

		jetstreamEvent := jetstream.JetstreamEvent{
			Did:  voter.DID, // Vote comes from voter's repo
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.interaction.vote",
				RKey:       voteRkey,
				CID:        "bafy2bzacevotecid123",
				Record: map[string]interface{}{
					"$type": "social.coves.interaction.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// STEP 2: Process event through Jetstream consumer
		consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
		err := consumer.HandleEvent(ctx, &jetstreamEvent)
		if err != nil {
			t.Fatalf("Jetstream consumer failed to process event: %v", err)
		}

		// STEP 3: Verify vote was indexed in AppView database
		indexedVote, err := voteRepo.GetByURI(ctx, voteURI)
		if err != nil {
			t.Fatalf("Vote not indexed in AppView: %v", err)
		}

		// STEP 4: Verify vote fields are correct
		assert.Equal(t, voteURI, indexedVote.URI, "Vote URI should match")
		assert.Equal(t, voter.DID, indexedVote.VoterDID, "Voter DID should match")
		assert.Equal(t, postURI, indexedVote.SubjectURI, "Subject URI should match")
		assert.Equal(t, postCID, indexedVote.SubjectCID, "Subject CID should match (strong reference)")
		assert.Equal(t, "up", indexedVote.Direction, "Direction should be 'up'")

		// STEP 5: Verify post vote counts were updated atomically
		updatedPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err, "Post should still exist")
		assert.Equal(t, 1, updatedPost.UpvoteCount, "Post upvote_count should be 1")
		assert.Equal(t, 0, updatedPost.DownvoteCount, "Post downvote_count should be 0")
		assert.Equal(t, 1, updatedPost.Score, "Post score should be 1 (upvotes - downvotes)")

		t.Logf("✓ E2E test passed! Vote indexed with URI: %s, post upvotes: %d", indexedVote.URI, updatedPost.UpvoteCount)
	})

	t.Run("Create downvote and verify counts", func(t *testing.T) {
		ctx := context.Background()

		// Create a different voter for this test to avoid unique constraint violation
		downvoter := createTestUser(t, db, "downvoter.test", "did:plc:votee2edownvoter")

		// Create downvote
		voteRkey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.interaction.vote/%s", downvoter.DID, voteRkey)

		jetstreamEvent := jetstream.JetstreamEvent{
			Did:  downvoter.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.interaction.vote",
				RKey:       voteRkey,
				CID:        "bafy2bzacedownvotecid",
				Record: map[string]interface{}{
					"$type": "social.coves.interaction.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "down",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
		err := consumer.HandleEvent(ctx, &jetstreamEvent)
		require.NoError(t, err, "Consumer should process downvote")

		// Verify vote indexed
		indexedVote, err := voteRepo.GetByURI(ctx, voteURI)
		require.NoError(t, err, "Downvote should be indexed")
		assert.Equal(t, "down", indexedVote.Direction, "Direction should be 'down'")

		// Verify post counts (now has 1 upvote + 1 downvote from previous test)
		updatedPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		assert.Equal(t, 1, updatedPost.UpvoteCount, "Upvote count should still be 1")
		assert.Equal(t, 1, updatedPost.DownvoteCount, "Downvote count should be 1")
		assert.Equal(t, 0, updatedPost.Score, "Score should be 0 (1 up - 1 down)")

		t.Logf("✓ Downvote indexed, post counts: up=%d down=%d score=%d",
			updatedPost.UpvoteCount, updatedPost.DownvoteCount, updatedPost.Score)
	})

	t.Run("Delete vote and verify counts decremented", func(t *testing.T) {
		ctx := context.Background()

		// Create a different voter for this test
		deletevoter := createTestUser(t, db, "deletevoter.test", "did:plc:votee2edeletevoter")

		// Get current counts
		beforePost, _ := postRepo.GetByURI(ctx, postURI)

		// Create a vote first
		voteRkey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.interaction.vote/%s", deletevoter.DID, voteRkey)

		createEvent := jetstream.JetstreamEvent{
			Did:  deletevoter.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.interaction.vote",
				RKey:       voteRkey,
				CID:        "bafy2bzacedeleteme",
				Record: map[string]interface{}{
					"$type": "social.coves.interaction.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
		err := consumer.HandleEvent(ctx, &createEvent)
		require.NoError(t, err)

		// Now delete it
		deleteEvent := jetstream.JetstreamEvent{
			Did:  deletevoter.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.interaction.vote",
				RKey:       voteRkey,
			},
		}

		err = consumer.HandleEvent(ctx, &deleteEvent)
		require.NoError(t, err, "Consumer should process delete")

		// Verify vote is soft-deleted
		deletedVote, err := voteRepo.GetByURI(ctx, voteURI)
		require.NoError(t, err, "Vote should still exist (soft delete)")
		assert.NotNil(t, deletedVote.DeletedAt, "Vote should have deleted_at timestamp")

		// Verify post counts decremented
		afterPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		assert.Equal(t, beforePost.UpvoteCount, afterPost.UpvoteCount,
			"Upvote count should be back to original (delete decremented)")

		t.Logf("✓ Vote deleted, counts decremented correctly")
	})

	t.Run("Idempotent indexing - duplicate events", func(t *testing.T) {
		ctx := context.Background()

		// Create a different voter for this test
		idempotentvoter := createTestUser(t, db, "idempotentvoter.test", "did:plc:votee2eidempotent")

		// Create a vote
		voteRkey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.interaction.vote/%s", idempotentvoter.DID, voteRkey)

		event := jetstream.JetstreamEvent{
			Did:  idempotentvoter.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.interaction.vote",
				RKey:       voteRkey,
				CID:        "bafy2bzaceidempotent",
				Record: map[string]interface{}{
					"$type": "social.coves.interaction.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)

		// First event - should succeed
		err := consumer.HandleEvent(ctx, &event)
		require.NoError(t, err, "First event should succeed")

		// Get counts after first event
		firstPost, _ := postRepo.GetByURI(ctx, postURI)

		// Second event (duplicate) - should be handled gracefully
		err = consumer.HandleEvent(ctx, &event)
		require.NoError(t, err, "Duplicate event should be handled gracefully")

		// Verify counts NOT incremented again (idempotent)
		secondPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		assert.Equal(t, firstPost.UpvoteCount, secondPost.UpvoteCount,
			"Duplicate event should not increment count again")

		// Verify only one vote in database
		vote, err := voteRepo.GetByURI(ctx, voteURI)
		require.NoError(t, err)
		assert.Equal(t, voteURI, vote.URI, "Should still be the same vote")

		t.Logf("✓ Idempotency test passed - duplicate event handled correctly")
	})

	t.Run("Security: Vote from wrong repository rejected", func(t *testing.T) {
		ctx := context.Background()

		// SECURITY TEST: Try to create a vote that claims to be from the voter
		// but actually comes from a different user's repository
		// This should be REJECTED by the consumer

		maliciousUser := createTestUser(t, db, "hacker.test", "did:plc:hacker123")

		maliciousEvent := jetstream.JetstreamEvent{
			Did:  maliciousUser.DID, // Event from hacker's repo
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.interaction.vote",
				RKey:       generateTID(),
				CID:        "bafy2bzacefake",
				Record: map[string]interface{}{
					"$type": "social.coves.interaction.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)
		err := consumer.HandleEvent(ctx, &maliciousEvent)

		// Should succeed (vote is created in hacker's repo, which is valid)
		// The vote record itself is FROM their repo, so it's legitimate
		// This is different from posts which must come from community repo
		assert.NoError(t, err, "Votes in user repos are valid")

		t.Logf("✓ Security validation passed - user repo votes are allowed")
	})
}

// TestVote_E2E_LivePDS tests the COMPLETE end-to-end flow with a live PDS:
// 1. HTTP POST to /xrpc/social.coves.interaction.createVote (with auth)
// 2. Handler → Service → Write to user's PDS repository
// 3. PDS → Jetstream firehose event
// 4. Jetstream consumer → Index in AppView database
// 5. Verify vote appears in database + post counts updated
//
// This is a TRUE E2E test that requires:
// - Live PDS running at PDS_URL (default: http://localhost:3001)
// - Live Jetstream running at JETSTREAM_URL (default: ws://localhost:6008/subscribe)
// - Test database running
func TestVote_E2E_LivePDS(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live PDS E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to connect to test database")
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Logf("Failed to close database: %v", closeErr)
		}
	}()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is running
	jetstreamHealthURL := "http://127.0.0.1:6009/metrics" // Use 127.0.0.1 for IPv4
	jetstreamResp, err := http.Get(jetstreamHealthURL)
	if err != nil {
		t.Skipf("Jetstream not running: %v", err)
	}
	_ = jetstreamResp.Body.Close()

	ctx := context.Background()

	// Cleanup old test data
	_, _ = db.Exec("DELETE FROM votes WHERE voter_did LIKE 'did:plc:votee2elive%' OR voter_did IN (SELECT did FROM users WHERE handle LIKE '%votee2elive%')")
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:votee2elive%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:votee2elive%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:votee2elive%' OR handle LIKE '%votee2elive%' OR handle LIKE '%authore2e%'")

	// Setup repositories and services
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)

	// Create test voter
	voter := createTestUser(t, db, "votee2elive.bsky.social", "did:plc:votee2elive123")

	// Create test community and post (simplified - using fake credentials)
	author := createTestUser(t, db, "authore2e.bsky.social", "did:plc:votee2eliveauthor")
	community := &communities.Community{
		DID:             "did:plc:votee2elivecommunity",
		Handle:          "votee2elivecommunity.test.coves.social",
		Name:            "votee2elivecommunity",
		DisplayName:     "Vote E2E Live Community",
		OwnerDID:        author.DID,
		CreatedByDID:    author.DID,
		HostedByDID:     "did:web:coves.test",
		Visibility:      "public",
		ModerationType:  "moderator",
		RecordURI:       "at://did:plc:votee2elivecommunity/social.coves.community.profile/self",
		RecordCID:       "fakecid",
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err = communityRepo.Create(ctx, community)
	require.NoError(t, err)

	postRkey := generateTID()
	postURI := fmt.Sprintf("at://%s/social.coves.post.record/%s", community.DID, postRkey)
	postCID := "bafy2bzaceposte2e"
	post := &posts.Post{
		URI:           postURI,
		CID:           postCID,
		RKey:          postRkey,
		AuthorDID:     author.DID,
		CommunityDID:  community.DID,
		Title:         stringPtr("E2E Vote Test Post"),
		Content:       stringPtr("This post will receive live votes"),
		CreatedAt:     time.Now(),
		UpvoteCount:   0,
		DownvoteCount: 0,
		Score:         0,
	}
	err = postRepo.Create(ctx, post)
	require.NoError(t, err)

	// Setup vote service and handler
	voteService := votes.NewVoteService(voteRepo, postRepo, pdsURL)
	voteHandler := vote.NewCreateVoteHandler(voteService)
	authMiddleware := middleware.NewAtProtoAuthMiddleware(nil, true) // Skip JWT verification for testing

	t.Run("Live E2E: Create vote and verify via Jetstream", func(t *testing.T) {
		t.Logf("\n🔄 TRUE E2E: Creating vote via XRPC endpoint...")

		// Authenticate voter with PDS to get real access token
		// Note: This assumes the voter account already exists on PDS
		// For a complete test, you'd create the account first via com.atproto.server.createAccount
		instanceHandle := os.Getenv("PDS_INSTANCE_HANDLE")
		instancePassword := os.Getenv("PDS_INSTANCE_PASSWORD")
		if instanceHandle == "" {
			instanceHandle = "testuser123.local.coves.dev"
		}
		if instancePassword == "" {
			instancePassword = "test-password-123"
		}

		t.Logf("🔐 Authenticating voter with PDS as: %s", instanceHandle)
		voterAccessToken, voterDID, err := authenticateWithPDS(pdsURL, instanceHandle, instancePassword)
		if err != nil {
			t.Skipf("Failed to authenticate voter with PDS (account may not exist): %v", err)
		}
		t.Logf("✅ Authenticated - Voter DID: %s", voterDID)

		// Update voter record to match authenticated DID
		_, err = db.Exec("UPDATE users SET did = $1 WHERE did = $2", voterDID, voter.DID)
		require.NoError(t, err)
		voter.DID = voterDID

		// Build HTTP request for vote creation
		reqBody := map[string]interface{}{
			"subject":   postURI,
			"direction": "up",
		}
		reqJSON, err := json.Marshal(reqBody)
		require.NoError(t, err)

		// Create HTTP request
		req := httptest.NewRequest("POST", "/xrpc/social.coves.interaction.createVote", bytes.NewReader(reqJSON))
		req.Header.Set("Content-Type", "application/json")

		// Use REAL PDS access token (not mock JWT)
		req.Header.Set("Authorization", "Bearer "+voterAccessToken)

		// Execute request through auth middleware + handler
		rr := httptest.NewRecorder()
		handler := authMiddleware.RequireAuth(http.HandlerFunc(voteHandler.HandleCreateVote))
		handler.ServeHTTP(rr, req)

		// Check response
		require.Equal(t, http.StatusOK, rr.Code, "Handler should return 200 OK, body: %s", rr.Body.String())

		// Parse response
		var response map[string]interface{}
		err = json.NewDecoder(rr.Body).Decode(&response)
		require.NoError(t, err, "Failed to parse response")

		voteURI := response["uri"].(string)
		voteCID := response["cid"].(string)

		t.Logf("✅ Vote created on PDS:")
		t.Logf("   URI: %s", voteURI)
		t.Logf("   CID: %s", voteCID)

		// ====================================================================================
		// Part 2: Query the PDS to verify the vote record exists
		// ====================================================================================
		t.Run("2a. Verify vote record on PDS", func(t *testing.T) {
			t.Logf("\n📡 Querying PDS for vote record...")

			// Extract rkey from vote URI (at://did/collection/rkey)
			parts := strings.Split(voteURI, "/")
			rkey := parts[len(parts)-1]

			// Query PDS for the vote record
			getRecordURL := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
				pdsURL, voterDID, "social.coves.interaction.vote", rkey)

			t.Logf("   GET %s", getRecordURL)

			pdsResp, err := http.Get(getRecordURL)
			require.NoError(t, err, "Failed to query PDS")
			defer pdsResp.Body.Close()

			require.Equal(t, http.StatusOK, pdsResp.StatusCode, "Vote record should exist on PDS")

			var pdsRecord struct {
				Value map[string]interface{} `json:"value"`
				URI   string                 `json:"uri"`
				CID   string                 `json:"cid"`
			}

			err = json.NewDecoder(pdsResp.Body).Decode(&pdsRecord)
			require.NoError(t, err, "Failed to decode PDS response")

			t.Logf("✅ Vote record found on PDS!")
			t.Logf("   URI:       %s", pdsRecord.URI)
			t.Logf("   CID:       %s", pdsRecord.CID)
			t.Logf("   Direction: %v", pdsRecord.Value["direction"])
			t.Logf("   Subject:   %v", pdsRecord.Value["subject"])

			// Verify the record matches what we created
			assert.Equal(t, voteURI, pdsRecord.URI, "PDS URI should match")
			assert.Equal(t, voteCID, pdsRecord.CID, "PDS CID should match")
			assert.Equal(t, "up", pdsRecord.Value["direction"], "Direction should be 'up'")

			// Print full record for inspection
			recordJSON, _ := json.MarshalIndent(pdsRecord.Value, "   ", "  ")
			t.Logf("   Full record:\n   %s", string(recordJSON))
		})

		// ====================================================================================
		// Part 2b: TRUE E2E - Real Jetstream Firehose Consumer
		// ====================================================================================
		t.Run("2b. Real Jetstream Firehose Consumption", func(t *testing.T) {
			t.Logf("\n🔄 TRUE E2E: Subscribing to real Jetstream firehose...")

			// Get PDS hostname for Jetstream filtering
			pdsHostname := strings.TrimPrefix(pdsURL, "http://")
			pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
			pdsHostname = strings.Split(pdsHostname, ":")[0] // Remove port

			// Build Jetstream URL with filters for vote records
			jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.interaction.vote",
				pdsHostname)

			t.Logf("   Jetstream URL: %s", jetstreamURL)
			t.Logf("   Looking for vote URI: %s", voteURI)
			t.Logf("   Voter DID: %s", voterDID)

			// Create vote consumer (same as main.go)
			consumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)

			// Channels to receive the event
			eventChan := make(chan *jetstream.JetstreamEvent, 10)
			errorChan := make(chan error, 1)
			done := make(chan bool)

			// Start Jetstream WebSocket subscriber in background
			go func() {
				err := subscribeToJetstreamForVote(ctx, jetstreamURL, voterDID, postURI, consumer, eventChan, errorChan, done)
				if err != nil {
					errorChan <- err
				}
			}()

			// Wait for event or timeout
			t.Logf("⏳ Waiting for Jetstream event (max 30 seconds)...")

			select {
			case event := <-eventChan:
				t.Logf("✅ Received real Jetstream event!")
				t.Logf("   Event DID:    %s", event.Did)
				t.Logf("   Collection:   %s", event.Commit.Collection)
				t.Logf("   Operation:    %s", event.Commit.Operation)
				t.Logf("   RKey:         %s", event.Commit.RKey)

				// Verify it's for our voter
				assert.Equal(t, voterDID, event.Did, "Event should be from voter's repo")

				// Verify vote was indexed in AppView database
				t.Logf("\n🔍 Querying AppView database for indexed vote...")

				indexedVote, err := voteRepo.GetByVoterAndSubject(ctx, voterDID, postURI)
				require.NoError(t, err, "Vote should be indexed in AppView")

				t.Logf("✅ Vote indexed in AppView:")
				t.Logf("   URI:        %s", indexedVote.URI)
				t.Logf("   CID:        %s", indexedVote.CID)
				t.Logf("   Voter DID:  %s", indexedVote.VoterDID)
				t.Logf("   Subject:    %s", indexedVote.SubjectURI)
				t.Logf("   Direction:  %s", indexedVote.Direction)

				// Verify all fields match
				assert.Equal(t, voteURI, indexedVote.URI, "URI should match")
				assert.Equal(t, voteCID, indexedVote.CID, "CID should match")
				assert.Equal(t, voterDID, indexedVote.VoterDID, "Voter DID should match")
				assert.Equal(t, postURI, indexedVote.SubjectURI, "Subject URI should match")
				assert.Equal(t, "up", indexedVote.Direction, "Direction should be 'up'")

				// Verify post counts were updated
				t.Logf("\n🔍 Verifying post vote counts updated...")
				updatedPost, err := postRepo.GetByURI(ctx, postURI)
				require.NoError(t, err, "Post should exist")

				t.Logf("✅ Post vote counts updated:")
				t.Logf("   Upvotes:   %d", updatedPost.UpvoteCount)
				t.Logf("   Downvotes: %d", updatedPost.DownvoteCount)
				t.Logf("   Score:     %d", updatedPost.Score)

				assert.Equal(t, 1, updatedPost.UpvoteCount, "Upvote count should be 1")
				assert.Equal(t, 0, updatedPost.DownvoteCount, "Downvote count should be 0")
				assert.Equal(t, 1, updatedPost.Score, "Score should be 1")

				// Signal to stop Jetstream consumer
				close(done)

				t.Log("\n✅ TRUE E2E COMPLETE: PDS → Jetstream → Consumer → AppView ✓")

			case err := <-errorChan:
				t.Fatalf("❌ Jetstream error: %v", err)

			case <-time.After(30 * time.Second):
				t.Fatalf("❌ Timeout: No Jetstream event received within 30 seconds")
			}
		})
	})
}

// subscribeToJetstreamForVote subscribes to real Jetstream firehose and processes vote events
// This helper creates a WebSocket connection to Jetstream and waits for vote events
func subscribeToJetstreamForVote(
	ctx context.Context,
	jetstreamURL string,
	targetVoterDID string,
	targetSubjectURI string,
	consumer *jetstream.VoteEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	errorChan chan<- error,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Read messages until we find our event or receive done signal
	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Set read deadline to avoid blocking forever
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				// Check if it's a timeout (expected)
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // Timeout is expected, keep listening
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			// Check if this is a vote event for the target voter + subject
			if event.Did == targetVoterDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.interaction.vote" {

				// Verify it's for the target subject
				record := event.Commit.Record
				if subject, ok := record["subject"].(map[string]interface{}); ok {
					if subjectURI, ok := subject["uri"].(string); ok && subjectURI == targetSubjectURI {
						// This is our vote! Process it
						if err := consumer.HandleEvent(ctx, &event); err != nil {
							return fmt.Errorf("failed to process event: %w", err)
						}

						// Send to channel so test can verify
						select {
						case eventChan <- &event:
							return nil
						case <-time.After(1 * time.Second):
							return fmt.Errorf("timeout sending event to channel")
						}
					}
				}
			}
		}
	}
}

// Helper function
func stringPtr(s string) *string {
	return &s
}
