package integration

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// TestPostDeletion_JetstreamConsumer tests that the Jetstream consumer
// correctly handles post deletion events by soft-deleting posts in the AppView database.
func TestPostDeletion_JetstreamConsumer(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()

	// Cleanup old test data
	_, _ = db.Exec("DELETE FROM posts WHERE community_did = 'did:plc:deltest123'")
	_, _ = db.Exec("DELETE FROM communities WHERE did = 'did:plc:deltest123'")
	_, _ = db.Exec("DELETE FROM users WHERE did = 'did:plc:delauthor123'")

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup user service for post consumer
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, "http://localhost:3001")

	// Create test user (author)
	author := createTestUser(t, db, "delauthor.test", "did:plc:delauthor123")

	// Create test community
	community := &communities.Community{
		DID:             "did:plc:deltest123",
		Handle:          "c-deltest.test.coves.social",
		Name:            "deltest",
		DisplayName:     "Delete Test Community",
		OwnerDID:        "did:plc:deltest123",
		CreatedByDID:    author.DID,
		HostedByDID:     "did:web:coves.test",
		Visibility:      "public",
		ModerationType:  "moderator",
		RecordURI:       "at://did:plc:deltest123/social.coves.community.profile/self",
		RecordCID:       "fakecid123",
		PDSAccessToken:  "fake_token_for_testing",
		PDSRefreshToken: "fake_refresh_token",
	}
	_, err := communityRepo.Create(ctx, community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	consumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

	t.Run("Create then delete post via Jetstream", func(t *testing.T) {
		rkey := generateTID()
		title := "Post to be deleted"
		content := "This post will be deleted"

		// Step 1: Create the post via Jetstream event
		createEvent := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       rkey,
				CID:        "bafy2bzacedeltest1",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": community.DID,
					"author":    author.DID,
					"title":     title,
					"content":   content,
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, &createEvent)
		if err != nil {
			t.Fatalf("Failed to create post: %v", err)
		}

		// Verify post was created
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", community.DID, rkey)
		createdPost, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Post not indexed after create: %v", err)
		}
		if createdPost.DeletedAt != nil {
			t.Fatal("Post should not be deleted initially")
		}

		t.Logf("✓ Post created: %s", postURI)

		// Step 2: Delete the post via Jetstream event
		deleteEvent := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.community.post",
				RKey:       rkey,
			},
		}

		err = consumer.HandleEvent(ctx, &deleteEvent)
		if err != nil {
			t.Fatalf("Failed to delete post: %v", err)
		}

		// Step 3: Verify post was soft-deleted
		deletedPost, err := postRepo.GetByURI(ctx, postURI)
		if err != nil {
			t.Fatalf("Post should still exist after soft delete: %v", err)
		}
		if deletedPost.DeletedAt == nil {
			t.Fatal("Post should have deleted_at set after delete")
		}

		t.Logf("✓ Post soft-deleted: deleted_at=%v", deletedPost.DeletedAt)
		t.Log("✅ Delete flow complete: Create → Delete → Verify soft-deleted")
	})

	t.Run("Delete is idempotent", func(t *testing.T) {
		rkey := generateTID()

		// Create a post first
		createEvent := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       rkey,
				CID:        "bafy2bzaceidempotentdel",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": community.DID,
					"author":    author.DID,
					"title":     "Idempotent delete test",
					"content":   "Testing idempotent deletion",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}
		err := consumer.HandleEvent(ctx, &createEvent)
		if err != nil {
			t.Fatalf("Failed to create post: %v", err)
		}

		// Delete once
		deleteEvent := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.community.post",
				RKey:       rkey,
			},
		}
		err = consumer.HandleEvent(ctx, &deleteEvent)
		if err != nil {
			t.Fatalf("First delete failed: %v", err)
		}

		// Delete again (should be idempotent)
		err = consumer.HandleEvent(ctx, &deleteEvent)
		if err != nil {
			t.Fatalf("Second delete should be idempotent, got error: %v", err)
		}

		t.Log("✓ Delete is idempotent - second delete did not fail")
	})

	t.Run("Delete non-existent post is idempotent", func(t *testing.T) {
		// Try to delete a post that was never created
		deleteEvent := jetstream.JetstreamEvent{
			Did:  community.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "delete",
				Collection: "social.coves.community.post",
				RKey:       "nonexistent123",
			},
		}

		err := consumer.HandleEvent(ctx, &deleteEvent)
		if err != nil {
			t.Fatalf("Delete of non-existent post should be idempotent, got error: %v", err)
		}

		t.Log("✓ Delete of non-existent post is idempotent")
	})
}

// TestPostDeletion_Authorization tests that only the post author can delete their posts
func TestPostDeletion_Authorization(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping authorization test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Create a mock community service for testing
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		"did:web:test",
		"test.coves.social",
		nil, // No provisioner needed for this test
		nil, // No PDS factory
		nil, // No blob service
	)

	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL)

	// Create test user (attacker trying to delete another user's post)
	attackerHandle := fmt.Sprintf("attacker%d.local.coves.dev", time.Now().UnixNano()%1000000)
	attackerEmail := fmt.Sprintf("attacker-%d@test.local", time.Now().Unix())
	attackerToken, attackerDID, err := createPDSAccount(pdsURL, attackerHandle, attackerEmail, "password123")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth session for attacker
	parsedDID, err := syntax.ParseDID(attackerDID)
	if err != nil {
		t.Fatalf("Failed to parse attacker DID: %v", err)
	}
	attackerSession := &oauthlib.ClientSessionData{
		AccountDID:  parsedDID,
		AccessToken: attackerToken,
		HostURL:     pdsURL,
	}

	// Create post URI belonging to a DIFFERENT user (the owner)
	ownerDID := "did:plc:owner123"
	postURI := fmt.Sprintf("at://%s/social.coves.community.post/test123", ownerDID)

	t.Run("Non-author cannot delete post - URI contains wrong DID", func(t *testing.T) {
		// The post URI contains ownerDID in the community position
		// This should fail because attacker is not the community owner
		// and wouldn't have credentials to delete from that repo

		deleteReq := posts.DeletePostRequest{
			URI: postURI,
		}

		err := postService.DeletePost(ctx, attackerSession, deleteReq)

		// We expect an error - either NotAuthorized or CommunityNotFound
		// since the community doesn't exist in our test DB
		if err == nil {
			t.Fatal("Expected error when non-author tries to delete post, got nil")
		}

		t.Logf("✓ Non-author blocked from deleting post: %v", err)
	})

	t.Run("Invalid URI format returns error", func(t *testing.T) {
		deleteReq := posts.DeletePostRequest{
			URI: "invalid-uri-format",
		}

		err := postService.DeletePost(ctx, attackerSession, deleteReq)

		if err == nil {
			t.Fatal("Expected error for invalid URI, got nil")
		}

		// Should be a validation error
		if !posts.IsValidationError(err) {
			t.Logf("Got error (expected validation): %v", err)
		}

		t.Logf("✓ Invalid URI rejected: %v", err)
	})

	t.Run("Empty URI returns error", func(t *testing.T) {
		deleteReq := posts.DeletePostRequest{
			URI: "",
		}

		err := postService.DeletePost(ctx, attackerSession, deleteReq)

		if err == nil {
			t.Fatal("Expected error for empty URI, got nil")
		}

		t.Logf("✓ Empty URI rejected: %v", err)
	})

	t.Run("Nil session returns error", func(t *testing.T) {
		deleteReq := posts.DeletePostRequest{
			URI: postURI,
		}

		err := postService.DeletePost(ctx, nil, deleteReq)

		if err == nil {
			t.Fatal("Expected error for nil session, got nil")
		}

		t.Logf("✓ Nil session rejected: %v", err)
	})
}

// TestPostDeletion_ServiceAuthorization tests the author verification logic in the service layer
// This test requires a live PDS to fully test the authorization flow
func TestPostDeletion_ServiceAuthorization_LivePDS(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live PDS test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Check if PDS is available
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}
	_ = healthResp.Body.Close()

	// Setup repositories
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Get instance credentials to determine correct domain
	instanceHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	instancePassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if instanceHandle == "" {
		instanceHandle = "testuser123.local.coves.dev"
	}
	if instancePassword == "" {
		instancePassword = "test-password-123"
	}

	_, instanceDID, err := authenticateWithPDS(pdsURL, instanceHandle, instancePassword)
	if err != nil {
		t.Skipf("Failed to authenticate with PDS: %v", err)
	}

	var instanceDomain string
	if strings.HasPrefix(instanceDID, "did:web:") {
		instanceDomain = strings.TrimPrefix(instanceDID, "did:web:")
	} else {
		instanceDomain = "local.coves.dev"
	}

	// Create provisioner for community creation
	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL)

	// Create two test users
	ownerHandle := fmt.Sprintf("postowner%d.local.coves.dev", time.Now().UnixNano()%1000000)
	ownerEmail := fmt.Sprintf("postowner-%d@test.local", time.Now().Unix())
	_, ownerDID, err := createPDSAccount(pdsURL, ownerHandle, ownerEmail, "password123")
	if err != nil {
		t.Skipf("Failed to create owner account: %v", err)
	}
	owner := createTestUser(t, db, ownerHandle, ownerDID)

	attackerHandle := fmt.Sprintf("postattacker%d.local.coves.dev", time.Now().UnixNano()%1000000)
	attackerEmail := fmt.Sprintf("postattacker-%d@test.local", time.Now().Unix())
	attackerToken, attackerDID, err := createPDSAccount(pdsURL, attackerHandle, attackerEmail, "password123")
	if err != nil {
		t.Skipf("Failed to create attacker account: %v", err)
	}
	_ = createTestUser(t, db, attackerHandle, attackerDID)

	// Setup attacker session
	parsedAttackerDID, _ := syntax.ParseDID(attackerDID)
	attackerSession := &oauthlib.ClientSessionData{
		AccountDID:  parsedAttackerDID,
		AccessToken: attackerToken,
		HostURL:     pdsURL,
	}

	// Create a test community
	communityName := fmt.Sprintf("delauth%d", time.Now().UnixNano()%1000000)
	community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:         communityName,
		DisplayName:  "Delete Auth Test Community",
		Description:  "Testing post deletion authorization",
		CreatedByDID: owner.DID,
		Visibility:   "public",
	})
	if err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	t.Logf("✓ Community created: %s (%s)", community.Name, community.DID)

	// Create a post as the owner
	title := "Owner's Post"
	content := "This post belongs to the owner"
	createResp, err := postService.CreatePost(
		middleware.SetTestUserDID(ctx, owner.DID),
		posts.CreatePostRequest{
			Community: community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: owner.DID,
		},
	)
	if err != nil {
		t.Fatalf("Failed to create post: %v", err)
	}

	t.Logf("✓ Post created by owner: %s", createResp.URI)

	t.Run("Attacker cannot delete owner's post", func(t *testing.T) {
		deleteReq := posts.DeletePostRequest{
			URI: createResp.URI,
		}

		err := postService.DeletePost(ctx, attackerSession, deleteReq)

		if err == nil {
			t.Fatal("Expected ErrNotAuthorized when attacker tries to delete owner's post")
		}

		if !errors.Is(err, posts.ErrNotAuthorized) {
			t.Errorf("Expected ErrNotAuthorized, got: %v", err)
		}

		t.Logf("✅ Authorization check passed: attacker blocked with %v", err)
	})
}

// TestPostE2E_DeleteWithJetstream tests post deletion with real PDS and Jetstream
// This is a TRUE E2E test that follows the complete flow:
// 1. Create real community on PDS
// 2. Create real post on PDS
// 3. Subscribe to Jetstream
// 4. Delete post via service (which deletes from PDS)
// 5. Receive delete event from Jetstream
// 6. Verify post is soft-deleted in AppView DB
func TestPostE2E_DeleteWithJetstream(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer func() { _ = db.Close() }()

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check Jetstream is available
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.post", pdsHostname)

	testConn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		t.Skipf("Jetstream not running at %s: %v", jetstreamURL, err)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup identity resolver for user service
	identityConfig := identity.DefaultConfig()
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL)

	// Setup community service with provisioner for real PDS
	var instanceDomain string
	instanceHandle := os.Getenv("PDS_INSTANCE_HANDLE")
	instancePassword := os.Getenv("PDS_INSTANCE_PASSWORD")
	if instanceHandle == "" {
		instanceHandle = "testuser123.local.coves.dev"
	}
	if instancePassword == "" {
		instancePassword = "test-password-123"
	}

	_, instanceDID, err := authenticateWithPDS(pdsURL, instanceHandle, instancePassword)
	if err != nil {
		t.Skipf("Failed to authenticate with PDS: %v", err)
	}

	if strings.HasPrefix(instanceDID, "did:web:") {
		instanceDomain = strings.TrimPrefix(instanceDID, "did:web:")
	} else {
		instanceDomain = "coves.social"
	}

	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		instanceDomain,
		provisioner,
		nil,
		nil,
	)

	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL)

	// Create test user
	testID := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	testUserHandle := fmt.Sprintf("postdel%s.local.coves.dev", testID)
	testUserEmail := fmt.Sprintf("postdel%s@test.local", testID)
	testUserPassword := "test-password-123"

	_, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test community on real PDS
	communityName := fmt.Sprintf("postdel%s", testID)
	t.Logf("\n📝 Creating community on PDS: %s", communityName)

	community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:         communityName,
		DisplayName:  "Post Delete E2E Test Community",
		Description:  "Testing post deletion E2E flow",
		CreatedByDID: testUser.DID,
		Visibility:   "public",
	})
	if err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	t.Logf("✅ Community created: %s (%s)", community.Name, community.DID)

	// Setup Jetstream consumer
	postConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

	t.Run("delete post with real Jetstream indexing", func(t *testing.T) {
		// Create post via service (writes to real PDS)
		createEventChan := make(chan *jetstream.JetstreamEvent, 10)
		createDone := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForPostCreate(ctx, jetstreamURL, community.DID, postConsumer, createEventChan, createDone)
			if subscribeErr != nil {
				t.Logf("Create subscription error: %v", subscribeErr)
			}
		}()

		time.Sleep(500 * time.Millisecond)

		title := "Post to delete via E2E"
		content := "This post will be deleted and we'll verify via Jetstream"
		t.Logf("\n📝 Creating post on PDS...")

		createResp, err := postService.CreatePost(
			middleware.SetTestUserDID(ctx, testUser.DID),
			posts.CreatePostRequest{
				Community: community.DID,
				Title:     &title,
				Content:   &content,
				AuthorDID: testUser.DID,
			},
		)
		if err != nil {
			t.Fatalf("Failed to create post: %v", err)
		}

		t.Logf("✅ Post created: %s", createResp.URI)

		// Wait for create event from Jetstream
		select {
		case <-createEventChan:
			t.Logf("✅ Create event received from Jetstream")
		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout waiting for create event")
		}
		close(createDone)

		// Verify post exists in AppView
		createdPost, err := postRepo.GetByURI(ctx, createResp.URI)
		if err != nil {
			t.Fatalf("Post should exist after create: %v", err)
		}
		if createdPost.DeletedAt != nil {
			t.Fatal("Post should not be deleted initially")
		}

		// Now delete the post
		t.Logf("\n🗑️ Deleting post via service...")

		deleteEventChan := make(chan *jetstream.JetstreamEvent, 10)
		deleteDone := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForPostDelete(ctx, jetstreamURL, community.DID, postConsumer, deleteEventChan, deleteDone)
			if subscribeErr != nil {
				t.Logf("Delete subscription error: %v", subscribeErr)
			}
		}()

		time.Sleep(500 * time.Millisecond)

		// Create OAuth session for the post author
		parsedDID, _ := syntax.ParseDID(testUser.DID)
		// Get fresh token for the user
		freshToken, _, err := authenticateWithPDS(pdsURL, testUserHandle, testUserPassword)
		if err != nil {
			t.Fatalf("Failed to get fresh token: %v", err)
		}
		session := &oauthlib.ClientSessionData{
			AccountDID:  parsedDID,
			AccessToken: freshToken,
			HostURL:     pdsURL,
		}

		err = postService.DeletePost(ctx, session, posts.DeletePostRequest{URI: createResp.URI})
		if err != nil {
			t.Fatalf("Failed to delete post: %v", err)
		}

		t.Logf("✅ Post delete request sent to PDS")

		// Wait for delete event from Jetstream
		t.Logf("\n⏳ Waiting for delete event from Jetstream...")

		select {
		case event := <-deleteEventChan:
			t.Logf("✅ Received delete event from Jetstream!")
			t.Logf("   Operation: %s", event.Commit.Operation)

			if event.Commit.Operation != "delete" {
				t.Errorf("Expected operation 'delete', got '%s'", event.Commit.Operation)
			}

			// Verify post is soft-deleted in AppView
			deletedPost, err := postRepo.GetByURI(ctx, createResp.URI)
			if err != nil {
				t.Fatalf("Failed to get deleted post: %v", err)
			}

			if deletedPost.DeletedAt == nil {
				t.Errorf("Expected post to be soft-deleted (deleted_at should be set)")
			} else {
				t.Logf("✅ Post soft-deleted in AppView at: %v", *deletedPost.DeletedAt)
			}

			close(deleteDone)

		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout: No delete event received within 30 seconds")
		}

		t.Logf("\n✅ TRUE E2E POST DELETE FLOW COMPLETE:")
		t.Logf("   Client → Service → PDS DeleteRecord → Jetstream → Consumer → AppView ✓")
	})
}

// subscribeToJetstreamForPostCreate subscribes for post create events
func subscribeToJetstreamForPostCreate(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.PostEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.post" &&
				event.Commit.Operation == "create" {

				if err := consumer.HandleEvent(ctx, &event); err != nil {
					return fmt.Errorf("failed to process event: %w", err)
				}

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

// subscribeToJetstreamForPostDelete subscribes for post delete events
func subscribeToJetstreamForPostDelete(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.PostEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.post" &&
				event.Commit.Operation == "delete" {

				if err := consumer.HandleEvent(ctx, &event); err != nil {
					return fmt.Errorf("failed to process event: %w", err)
				}

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
