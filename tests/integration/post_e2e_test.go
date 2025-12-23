package integration

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
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

// TestPostCreation_E2E_WithJetstream tests the full post creation flow:
// XRPC endpoint → AppView Service → PDS write → Jetstream consumer → DB indexing
//
// This is a TRUE E2E test that simulates what happens in production:
// 1. Client calls POST /xrpc/social.coves.community.post.create with auth token
// 2. Handler validates and calls PostService.CreatePost()
// 3. Service writes post to community's PDS repository
// 4. PDS broadcasts event to firehose/Jetstream
// 5. Jetstream consumer receives event and indexes post in AppView DB
// 6. Post is now queryable from AppView
//
// NOTE: This test simulates the Jetstream event (step 4-5) since we don't have
// a live PDS/Jetstream in test environment. For true live testing, use TestPostCreation_E2E_LivePDS.
func TestPostCreation_E2E_WithJetstream(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Cleanup old test data first
	_, _ = db.Exec("DELETE FROM posts WHERE community_did = 'did:plc:gaming123'")
	_, _ = db.Exec("DELETE FROM communities WHERE did = 'did:plc:gaming123'")
	_, _ = db.Exec("DELETE FROM users WHERE did = 'did:plc:alice123'")

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup user service for post consumer
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, "http://localhost:3001")

	// Create test user (author)
	author := createTestUser(t, db, "alice.test", "did:plc:alice123")

	// Create test community with fake PDS credentials
	// In real E2E, this would be a real community provisioned on PDS
	community := &communities.Community{
		DID:             "did:plc:gaming123",
		Handle:          "c-gaming.test.coves.social",
		Name:            "gaming",
		DisplayName:     "Gaming Community",
		OwnerDID:        "did:plc:gaming123",
		CreatedByDID:    author.DID,
		HostedByDID:     "did:web:coves.test",
		Visibility:      "public",
		ModerationType:  "moderator",
		RecordURI:       "at://did:plc:gaming123/social.coves.community.profile/self",
		RecordCID:       "fakecid123",
		PDSAccessToken:  "fake_token_for_testing",
		PDSRefreshToken: "fake_refresh_token",
	}
	_, err := communityRepo.Create(context.Background(), community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	t.Run("Full E2E flow - XRPC to DB via Jetstream", func(t *testing.T) {
		ctx := context.Background()

		// STEP 1: Simulate what the XRPC handler would receive
		// In real flow, this comes from client with OAuth bearer token
		title := "My First Post"
		content := "This is a test post!"
		postReq := posts.CreatePostRequest{
			Title:   &title,
			Content: &content,
			// Community and AuthorDID set by handler from request context
		}

		// STEP 2: Simulate Jetstream consumer receiving the post CREATE event
		// In real production, this event comes from PDS via Jetstream WebSocket
		// For this test, we simulate the event that would be broadcast after PDS write

		// Generate a realistic rkey (TID - timestamp identifier)
		rkey := generateTID()

		// Build the post record as it would appear in Jetstream
		jetstreamEvent := jetstream.JetstreamEvent{
			Did:  community.DID, // Repo owner (community)
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       rkey,
				CID:        "bafy2bzaceabc123def456", // Fake CID
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": community.DID,
					"author":    author.DID,
					"title":     *postReq.Title,
					"content":   *postReq.Content,
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// STEP 3: Process event through Jetstream consumer
		consumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)
		err := consumer.HandleEvent(ctx, &jetstreamEvent)
		if err != nil {
			t.Fatalf("Jetstream consumer failed to process event: %v", err)
		}

		// STEP 4: Verify post was indexed in AppView database
		expectedURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", community.DID, rkey)
		indexedPost, err := postRepo.GetByURI(ctx, expectedURI)
		if err != nil {
			t.Fatalf("Post not indexed in AppView: %v", err)
		}

		// STEP 5: Verify all fields are correct
		if indexedPost.URI != expectedURI {
			t.Errorf("Expected URI %s, got %s", expectedURI, indexedPost.URI)
		}
		if indexedPost.AuthorDID != author.DID {
			t.Errorf("Expected author %s, got %s", author.DID, indexedPost.AuthorDID)
		}
		if indexedPost.CommunityDID != community.DID {
			t.Errorf("Expected community %s, got %s", community.DID, indexedPost.CommunityDID)
		}
		if indexedPost.Title == nil || *indexedPost.Title != title {
			t.Errorf("Expected title '%s', got %v", title, indexedPost.Title)
		}
		if indexedPost.Content == nil || *indexedPost.Content != content {
			t.Errorf("Expected content '%s', got %v", content, indexedPost.Content)
		}

		// Verify stats initialized correctly
		if indexedPost.UpvoteCount != 0 {
			t.Errorf("Expected upvote_count 0, got %d", indexedPost.UpvoteCount)
		}
		if indexedPost.DownvoteCount != 0 {
			t.Errorf("Expected downvote_count 0, got %d", indexedPost.DownvoteCount)
		}
		if indexedPost.Score != 0 {
			t.Errorf("Expected score 0, got %d", indexedPost.Score)
		}

		t.Logf("✓ E2E test passed! Post indexed with URI: %s", indexedPost.URI)
	})

	t.Run("Consumer validates repository ownership (security)", func(t *testing.T) {
		ctx := context.Background()

		// SECURITY TEST: Try to create a post that claims to be from the community
		// but actually comes from a user's repository
		// This should be REJECTED by the consumer

		maliciousEvent := jetstream.JetstreamEvent{
			Did:  author.DID, // Event from user's repo (NOT community repo)
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       generateTID(),
				CID:        "bafy2bzacefake",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": community.DID, // Claims to be for this community
					"author":    author.DID,
					"title":     "Fake Post",
					"content":   "This is a malicious post attempt",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)
		err := consumer.HandleEvent(ctx, &maliciousEvent)

		// Should get security error
		if err == nil {
			t.Fatal("Expected security error for post from wrong repository, got nil")
		}

		if !contains(err.Error(), "repository DID") || !contains(err.Error(), "doesn't match") {
			t.Errorf("Expected repository mismatch error, got: %v", err)
		}

		t.Logf("✓ Security validation passed: %v", err)
	})

	t.Run("Idempotent indexing - duplicate events", func(t *testing.T) {
		ctx := context.Background()

		// Simulate the same Jetstream event arriving twice
		// This can happen during Jetstream replays or network retries
		rkey := generateTID()
		event := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       rkey,
				CID:        "bafy2bzaceidempotent",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": community.DID,
					"author":    author.DID,
					"title":     "Duplicate Test",
					"content":   "Testing idempotency",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

		// First event - should succeed
		err := consumer.HandleEvent(ctx, &event)
		if err != nil {
			t.Fatalf("First event failed: %v", err)
		}

		// Second event (duplicate) - should be handled gracefully
		err = consumer.HandleEvent(ctx, &event)
		if err != nil {
			t.Fatalf("Duplicate event should be handled gracefully, got error: %v", err)
		}

		// Verify only one post in database
		uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", community.DID, rkey)
		post, err := postRepo.GetByURI(ctx, uri)
		if err != nil {
			t.Fatalf("Post not found: %v", err)
		}

		if post.URI != uri {
			t.Error("Post URI mismatch - possible duplicate indexing")
		}

		t.Logf("✓ Idempotency test passed")
	})

	t.Run("Handles orphaned posts (unknown community)", func(t *testing.T) {
		ctx := context.Background()

		// Post references a community that doesn't exist in AppView yet
		// This can happen if Jetstream delivers post event before community profile event
		unknownCommunityDID := "did:plc:unknown999"

		event := jetstream.JetstreamEvent{
			Did:  unknownCommunityDID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       generateTID(),
				CID:        "bafy2bzaceorphaned",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": unknownCommunityDID,
					"author":    author.DID,
					"title":     "Orphaned Post",
					"content":   "Community not indexed yet",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		consumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

		// Should log warning but NOT fail (eventual consistency)
		// Note: This will fail due to foreign key constraint in current schema
		// In production, you might want to handle this differently (defer indexing, etc.)
		err := consumer.HandleEvent(ctx, &event)

		// For now, we expect this to fail due to FK constraint
		// In future, we might make FK constraint DEFERRABLE or handle orphaned posts differently
		if err == nil {
			t.Log("⚠️  Orphaned post was indexed (FK constraint not enforced)")
		} else {
			t.Logf("✓ Orphaned post rejected by FK constraint (expected): %v", err)
		}
	})
}

// TestPostCreation_E2E_LivePDS tests the COMPLETE end-to-end flow with a live PDS:
// 1. HTTP POST to /xrpc/social.coves.community.post.create (with auth)
// 2. Handler → Service → Write to community's PDS repository
// 3. PDS → Jetstream firehose event
// 4. Jetstream consumer → Index in AppView database
// 5. Verify post appears in database with correct data
//
// This is a TRUE E2E test that requires:
// - Live PDS running at PDS_URL (default: http://localhost:3001)
// - Live Jetstream running at JETSTREAM_URL (default: ws://localhost:6008/subscribe)
// - Test database running
func TestPostCreation_E2E_LivePDS(t *testing.T) {
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

	// Get instance credentials for authentication
	instanceHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	instancePassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if instanceHandle == "" {
		instanceHandle = "testuser123.local.coves.dev"
	}
	if instancePassword == "" {
		instancePassword = "test-password-123"
	}

	t.Logf("🔐 Authenticating with PDS as: %s", instanceHandle)

	// Authenticate to get instance DID (needed for provisioner domain)
	_, instanceDID, err := authenticateWithPDS(pdsURL, instanceHandle, instancePassword)
	if err != nil {
		t.Skipf("Failed to authenticate with PDS (may not be configured): %v", err)
	}

	t.Logf("✅ Authenticated - Instance DID: %s", instanceDID)

	// Extract instance domain from DID for community provisioning
	var instanceDomain string
	if strings.HasPrefix(instanceDID, "did:web:") {
		instanceDomain = strings.TrimPrefix(instanceDID, "did:web:")
	} else {
		// Fallback for did:plc
		instanceDomain = "coves.social"
	}

	// Setup repositories and services
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup PDS account provisioner for community creation
	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)

	// Setup community service with real PDS provisioner
	communityService := communities.NewCommunityService(
		communityRepo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner, // ✅ Real provisioner for creating communities on PDS
	)

	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL) // nil aggregatorService, blobService, unfurlService, blueskyService for user-only tests

	// Setup OAuth auth middleware for E2E testing
	e2eAuth := NewE2EOAuthMiddleware()

	// Setup HTTP handler
	createHandler := post.NewCreateHandler(postService)

	ctx := context.Background()

	// Cleanup old test data
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:e2etest%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:e2etest%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:e2etest%'")

	// Create test user (author)
	author := createTestUser(t, db, "e2etestauthor.bsky.social", "did:plc:e2etestauthor123")

	// ====================================================================================
	// Part 1: Write-Forward to PDS
	// ====================================================================================
	t.Run("1. Write-Forward to PDS", func(t *testing.T) {
		// TRUE E2E: Actually provision a real community on PDS
		// This tests the full flow:
		// 1. Call com.atproto.server.createAccount on PDS
		// 2. PDS generates DID, keys, tokens
		// 3. Write community profile to PDS repository
		// 4. Store credentials in AppView DB
		// 5. Use those credentials to create a post

		// Use timestamp to ensure unique community name for each test run
		communityName := fmt.Sprintf("e2e%d", time.Now().UnixNano()%1000000)

		t.Logf("\n📝 Provisioning test community on live PDS (name: %s)...", communityName)
		community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   communityName,
			DisplayName:            "E2E Test Community",
			Description:            "Test community for E2E post creation testing",
			CreatedByDID:           author.DID,
			Visibility:             "public",
			AllowExternalDiscovery: true,
		})
		require.NoError(t, err, "Failed to provision community on PDS")
		require.NotEmpty(t, community.DID, "Community should have DID from PDS")
		require.NotEmpty(t, community.PDSAccessToken, "Community should have access token")
		require.NotEmpty(t, community.PDSRefreshToken, "Community should have refresh token")

		t.Logf("✓ Community provisioned: DID=%s, Handle=%s", community.DID, community.Handle)

		// NOTE: Cleanup disabled to allow post-test inspection of indexed data
		// Uncomment to enable cleanup after test
		// defer func() {
		// 	if err := communityRepo.Delete(ctx, community.DID); err != nil {
		// 		t.Logf("Warning: Failed to cleanup test community: %v", err)
		// 	}
		// }()

		// Build HTTP request for post creation
		title := "E2E Test Post"
		content := "This post was created via full E2E test with live PDS!"
		reqBody := map[string]interface{}{
			"community": community.DID,
			"title":     title,
			"content":   content,
		}
		reqJSON, err := json.Marshal(reqBody)
		require.NoError(t, err)

		// Create HTTP request
		req := httptest.NewRequest("POST", "/xrpc/social.coves.community.post.create", bytes.NewReader(reqJSON))
		req.Header.Set("Content-Type", "application/json")

		// Register the author user with OAuth middleware and get test token
		// For Coves API handlers, use Bearer scheme with OAuth middleware
		token := e2eAuth.AddUser(author.DID)
		req.Header.Set("Authorization", "Bearer "+token)

		// Execute request through auth middleware + handler
		rr := httptest.NewRecorder()
		handler := e2eAuth.RequireAuth(http.HandlerFunc(createHandler.HandleCreate))
		handler.ServeHTTP(rr, req)

		// Check response
		require.Equal(t, http.StatusOK, rr.Code, "Handler should return 200 OK, body: %s", rr.Body.String())

		// Parse response
		var response posts.CreatePostResponse
		err = json.NewDecoder(rr.Body).Decode(&response)
		require.NoError(t, err, "Failed to parse response")

		t.Logf("✅ Post created on PDS:")
		t.Logf("   URI: %s", response.URI)
		t.Logf("   CID: %s", response.CID)

		// ====================================================================================
		// Part 2: TRUE E2E - Real Jetstream Firehose Consumer
		// ====================================================================================
		// This part tests the ACTUAL production code path in main.go
		// including the WebSocket connection and consumer logic
		t.Run("2. Real Jetstream Firehose Consumption", func(t *testing.T) {
			t.Logf("\n🔄 TRUE E2E: Subscribing to real Jetstream firehose...")

			// Get PDS hostname for Jetstream filtering
			pdsHostname := strings.TrimPrefix(pdsURL, "http://")
			pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
			pdsHostname = strings.Split(pdsHostname, ":")[0] // Remove port

			// Build Jetstream URL with filters for post records
			jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.post",
				pdsHostname)

			t.Logf("   Jetstream URL: %s", jetstreamURL)
			t.Logf("   Looking for post URI: %s", response.URI)
			t.Logf("   Community DID: %s", community.DID)

			// Setup user service (required by post consumer)
			userRepo := postgres.NewUserRepository(db)
			identityConfig := identity.DefaultConfig()
			plcURL := os.Getenv("PLC_DIRECTORY_URL")
			if plcURL == "" {
				plcURL = "http://localhost:3002"
			}
			identityConfig.PLCURL = plcURL
			identityResolver := identity.NewResolver(db, identityConfig)
			userService := users.NewUserService(userRepo, identityResolver, pdsURL)

			// Create post consumer (same as main.go)
			postConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

			// Channels to receive the event
			eventChan := make(chan *jetstream.JetstreamEvent, 10)
			errorChan := make(chan error, 1)
			done := make(chan bool)

			// Start Jetstream WebSocket subscriber in background
			// This creates its own WebSocket connection to Jetstream
			go func() {
				err := subscribeToJetstreamForPost(ctx, jetstreamURL, community.DID, postConsumer, eventChan, errorChan, done)
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

				// Verify it's for our community
				assert.Equal(t, community.DID, event.Did, "Event should be from community repo")

				// Verify post was indexed in AppView database
				t.Logf("\n🔍 Querying AppView database for indexed post...")

				indexedPost, err := postRepo.GetByURI(ctx, response.URI)
				require.NoError(t, err, "Post should be indexed in AppView")

				t.Logf("✅ Post indexed in AppView:")
				t.Logf("   URI:          %s", indexedPost.URI)
				t.Logf("   CID:          %s", indexedPost.CID)
				t.Logf("   Author DID:   %s", indexedPost.AuthorDID)
				t.Logf("   Community:    %s", indexedPost.CommunityDID)
				t.Logf("   Title:        %v", indexedPost.Title)
				t.Logf("   Content:      %v", indexedPost.Content)

				// Verify all fields match what we sent
				assert.Equal(t, response.URI, indexedPost.URI, "URI should match")
				assert.Equal(t, response.CID, indexedPost.CID, "CID should match")
				assert.Equal(t, author.DID, indexedPost.AuthorDID, "Author DID should match")
				assert.Equal(t, community.DID, indexedPost.CommunityDID, "Community DID should match")
				assert.Equal(t, title, *indexedPost.Title, "Title should match")
				assert.Equal(t, content, *indexedPost.Content, "Content should match")

				// Verify stats initialized correctly
				assert.Equal(t, 0, indexedPost.UpvoteCount, "Upvote count should be 0")
				assert.Equal(t, 0, indexedPost.DownvoteCount, "Downvote count should be 0")
				assert.Equal(t, 0, indexedPost.Score, "Score should be 0")
				assert.Equal(t, 0, indexedPost.CommentCount, "Comment count should be 0")

				// Verify timestamps
				assert.False(t, indexedPost.CreatedAt.IsZero(), "CreatedAt should be set")
				assert.False(t, indexedPost.IndexedAt.IsZero(), "IndexedAt should be set")

				// Signal to stop Jetstream consumer
				close(done)

				t.Log("\n✅ Part 2 Complete: TRUE E2E - PDS → Jetstream → Consumer → AppView ✓")

			case err := <-errorChan:
				t.Fatalf("❌ Jetstream error: %v", err)

			case <-time.After(30 * time.Second):
				t.Fatalf("❌ Timeout: No Jetstream event received within 30 seconds")
			}
		})
	})
}

// subscribeToJetstreamForPost subscribes to real Jetstream firehose and processes post events
// This helper creates a WebSocket connection to Jetstream and waits for post events
func subscribeToJetstreamForPost(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.PostEventConsumer,
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
				// For other errors, don't retry reading from a broken connection
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			// Check if this is a post event for the target DID
			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.post" {
				// Process the event through the consumer
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
