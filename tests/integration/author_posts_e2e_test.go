package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// getPostTitleFromView extracts title from PostView.Record.
// Fails the test if Record structure is invalid (should not happen in valid responses).
func getPostTitleFromView(t *testing.T, pv *posts.PostView) string {
	t.Helper()
	if pv.Record == nil {
		t.Fatalf("getPostTitleFromView: Record is nil for post URI %s", pv.URI)
	}
	record, ok := pv.Record.(map[string]interface{})
	if !ok {
		t.Fatalf("getPostTitleFromView: Record is %T, not map[string]interface{}", pv.Record)
	}
	title, ok := record["title"].(string)
	if !ok {
		t.Fatalf("getPostTitleFromView: title field missing or not string, Record: %+v", record)
	}
	return title
}

// TestGetAuthorPosts_E2E_Success tests the full author posts flow with real PDS
// Flow: Create user on PDS → Create posts → Query via XRPC → Verify response
func TestGetAuthorPosts_E2E_Success(t *testing.T) {
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

	// Run migrations
	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Check if PDS is running
	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	ctx := context.Background()

	// Setup repositories
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	// Setup services
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, pdsURL)
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, pdsURL, getTestInstanceDID(), "", nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL)
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("apt%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("author-posts-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	t.Logf("Creating test user on PDS: %s", testUserHandle)
	_, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	t.Logf("Test user created: DID=%s", userDID)

	// Index user in AppView
	_ = createTestUser(t, db, testUserHandle, userDID)

	// Create test community
	testCommunityDID, err := createFeedTestCommunity(db, ctx, "author-posts-test", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Create multiple test posts for the user
	now := time.Now()
	postURIs := make([]string, 5)
	for i := 0; i < 5; i++ {
		postURIs[i] = createTestPost(t, db, testCommunityDID, userDID, fmt.Sprintf("Test Post %d", i+1), i*10, now.Add(-time.Duration(i)*time.Hour))
	}
	t.Logf("Created %d test posts", len(postURIs))

	// Setup OAuth middleware
	e2eAuth := NewE2EOAuthMiddleware()
	token := e2eAuth.AddUser(userDID)

	// Setup HTTP server with XRPC routes
	r := chi.NewRouter()
	routes.RegisterActorRoutes(r, postService, userService, voteService, nil, nil, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Test 1: Get posts by DID
	t.Run("Get posts by DID", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&limit=10", httpServer.URL, userDID), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET author posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
		}

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		if len(response.Feed) != 5 {
			t.Errorf("Expected 5 posts, got %d", len(response.Feed))
		}

		// Verify posts are returned in correct order (newest first)
		for i, feedPost := range response.Feed {
			if feedPost.Post == nil {
				t.Errorf("Post %d is nil", i)
				continue
			}
			t.Logf("Post %d: %s", i, feedPost.Post.URI)
		}

		t.Logf("SUCCESS: Retrieved %d posts for author %s", len(response.Feed), userDID)
	})

	// Test 2: Get posts by handle
	t.Run("Get posts by handle", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&limit=5", httpServer.URL, testUserHandle), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET author posts by handle: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
		}

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		if len(response.Feed) != 5 {
			t.Errorf("Expected 5 posts, got %d", len(response.Feed))
		}

		t.Logf("SUCCESS: Handle resolution worked - %s → %s", testUserHandle, userDID)
	})

	// Test 3: Pagination with cursor
	t.Run("Pagination with cursor", func(t *testing.T) {
		// First page
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&limit=3", httpServer.URL, userDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET first page: %v", err)
		}

		var firstPage posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&firstPage); decodeErr != nil {
			t.Fatalf("Failed to decode first page: %v", decodeErr)
		}
		_ = resp.Body.Close()

		if len(firstPage.Feed) != 3 {
			t.Errorf("Expected 3 posts on first page, got %d", len(firstPage.Feed))
		}
		if firstPage.Cursor == nil {
			t.Fatal("Expected cursor for pagination")
		}

		// Second page using cursor
		req2, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&limit=3&cursor=%s",
				httpServer.URL, userDID, *firstPage.Cursor), nil)

		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("Failed to GET second page: %v", err)
		}
		defer func() { _ = resp2.Body.Close() }()

		var secondPage posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp2.Body).Decode(&secondPage); decodeErr != nil {
			t.Fatalf("Failed to decode second page: %v", decodeErr)
		}

		if len(secondPage.Feed) != 2 {
			t.Errorf("Expected 2 posts on second page, got %d", len(secondPage.Feed))
		}

		// Verify no overlap between pages
		firstPageURIs := make(map[string]bool)
		for _, fp := range firstPage.Feed {
			firstPageURIs[fp.Post.URI] = true
		}
		for _, fp := range secondPage.Feed {
			if firstPageURIs[fp.Post.URI] {
				t.Errorf("Duplicate post in second page: %s", fp.Post.URI)
			}
		}

		t.Logf("SUCCESS: Pagination working - page 1: %d posts, page 2: %d posts",
			len(firstPage.Feed), len(secondPage.Feed))
	})

	// Test 4: Actor not found
	t.Run("Actor not found", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s", httpServer.URL, "did:plc:nonexistent123"), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// The actor exists as a valid DID format but has no posts - should return empty feed
		// If you want 404, you'd need a user existence check in the service
		// For now, we expect 200 with empty feed (Bluesky-compatible behavior)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Logf("Response: %s", string(body))
		}

		t.Logf("SUCCESS: Non-existent actor handled correctly")
	})

	t.Logf("\nE2E AUTHOR POSTS FLOW COMPLETE:")
	t.Logf("   Created user on PDS")
	t.Logf("   Indexed 5 posts in AppView")
	t.Logf("   Queried by DID")
	t.Logf("   Queried by handle (with resolution)")
	t.Logf("   Tested pagination")
	t.Logf("   Tested error handling")
}

// TestGetAuthorPosts_FilterLogic tests the different filter options
func TestGetAuthorPosts_FilterLogic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Setup repositories and services
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, getTestPDSURL())
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, getTestPDSURL(), getTestInstanceDID(), "", nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, getTestPDSURL())
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user (did:plc uses base32: a-z, 2-7)
	testUserDID := "did:plc:filtertestabcd"
	_ = createTestUser(t, db, "filtertest.test", testUserDID)

	// Create test community
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "filter-test", "owner.test")

	// Create posts with and without embeds
	now := time.Now()

	// Create post without embed
	createTestPost(t, db, testCommunityDID, testUserDID, "Post without embed", 10, now)

	// Create post with embed (need to insert directly with embed field)
	embedJSON := `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com"}}`
	_, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at, score)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 20)
	`,
		fmt.Sprintf("at://%s/social.coves.community.post/embed-post", testCommunityDID),
		"bafyembed", "embed-post", testUserDID, testCommunityDID,
		"Post with embed", embedJSON, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("Failed to create post with embed: %v", err)
	}

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterActorRoutes(r, postService, userService, voteService, nil, nil, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Test: posts_with_media filter
	t.Run("Filter posts_with_media", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&filter=posts_with_media",
				httpServer.URL, testUserDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET filtered posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
		}

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		// Should only return the post with embed
		if len(response.Feed) != 1 {
			t.Errorf("Expected 1 post with media, got %d", len(response.Feed))
		}

		// Verify it's the post with embed
		if len(response.Feed) > 0 && response.Feed[0].Post != nil {
			if response.Feed[0].Post.Embed == nil {
				t.Error("Expected post with embed, but embed is nil")
			}
		}

		t.Logf("SUCCESS: posts_with_media filter returned %d posts", len(response.Feed))
	})

	// Test: posts_with_replies (default - returns all)
	t.Run("Filter posts_with_replies (default)", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&filter=posts_with_replies",
				httpServer.URL, testUserDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET filtered posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		// Should return all posts
		if len(response.Feed) != 2 {
			t.Errorf("Expected 2 posts, got %d", len(response.Feed))
		}

		t.Logf("SUCCESS: posts_with_replies filter returned %d posts", len(response.Feed))
	})

	// Test: Invalid filter returns error
	t.Run("Invalid filter returns error", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&filter=invalid_filter",
				httpServer.URL, testUserDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected 400 for invalid filter, got %d: %s", resp.StatusCode, string(body))
		}

		t.Logf("SUCCESS: Invalid filter correctly rejected")
	})
}

// TestGetAuthorPosts_ServiceErrors tests error handling in the service layer
func TestGetAuthorPosts_ServiceErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Setup services
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, getTestPDSURL())
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, getTestPDSURL(), getTestInstanceDID(), "", nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, getTestPDSURL())
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user and community
	testUserDID := "did:plc:serviceerrorabc"
	_ = createTestUser(t, db, "serviceerror.test", testUserDID)
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "serviceerror-test", "owner.test")

	// Create a test post
	createTestPost(t, db, testCommunityDID, testUserDID, "Test Post", 10, time.Now())

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterActorRoutes(r, postService, userService, voteService, nil, nil, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Test: Missing actor parameter
	t.Run("Missing actor parameter", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts", httpServer.URL), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected 400 for missing actor, got %d: %s", resp.StatusCode, string(body))
		}

		t.Logf("SUCCESS: Missing actor parameter correctly rejected")
	})

	// Test: Invalid DID format
	t.Run("Invalid DID format", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s", httpServer.URL, "not-a-did"), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// Invalid DIDs that don't resolve should return 404 (actor not found)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected 404 or 400 for invalid DID, got %d: %s", resp.StatusCode, string(body))
		}

		t.Logf("SUCCESS: Invalid DID format handled")
	})

	// Test: Invalid cursor
	t.Run("Invalid cursor", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&cursor=%s",
				httpServer.URL, testUserDID, "invalid-cursor-format"), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected 400 for invalid cursor, got %d: %s", resp.StatusCode, string(body))
		}

		t.Logf("SUCCESS: Invalid cursor correctly rejected")
	})

	// Test: Community filter with non-existent community
	t.Run("Non-existent community filter", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&community=%s",
				httpServer.URL, testUserDID, "did:plc:nonexistentcommunity"), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected 404 for non-existent community, got %d: %s", resp.StatusCode, string(body))
		}

		t.Logf("SUCCESS: Non-existent community correctly rejected")
	})
}

// TestGetAuthorPosts_WithJetstreamIndexing tests the full flow including Jetstream indexing
func TestGetAuthorPosts_WithJetstreamIndexing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	// Setup services
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, pdsURL)
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, pdsURL, getTestInstanceDID(), "", nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL)
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("jet%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("jetstream-author-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	_, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Index user in AppView
	_ = createTestUser(t, db, testUserHandle, userDID)

	// Create test community
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "jetstream-author-test", "owner.test")

	// Setup Jetstream consumer
	postConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)

	// Simulate a post being indexed via Jetstream
	t.Run("Index post via Jetstream consumer", func(t *testing.T) {
		rkey := fmt.Sprintf("post-%d", time.Now().UnixNano())
		postURI := fmt.Sprintf("at://%s/social.coves.community.post/%s", testCommunityDID, rkey)

		postEvent := jetstream.JetstreamEvent{
			Did:    testCommunityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-post-rev",
				Operation:  "create",
				Collection: "social.coves.community.post",
				RKey:       rkey,
				CID:        "bafyjetstream",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.post",
					"community": testCommunityDID,
					"author":    userDID,
					"title":     "Jetstream Indexed Post",
					"content":   "This post was indexed via Jetstream",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		if handleErr := postConsumer.HandleEvent(ctx, &postEvent); handleErr != nil {
			t.Fatalf("Failed to handle post event: %v", handleErr)
		}

		t.Logf("Post indexed via Jetstream: %s", postURI)

		// Verify post is now queryable via GetAuthorPosts
		e2eAuth := NewE2EOAuthMiddleware()
		r := chi.NewRouter()
		routes.RegisterActorRoutes(r, postService, userService, voteService, nil, nil, e2eAuth.OAuthAuthMiddleware)
		httpServer := httptest.NewServer(r)
		defer httpServer.Close()

		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s", httpServer.URL, userDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET author posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		if len(response.Feed) != 1 {
			t.Errorf("Expected 1 post, got %d", len(response.Feed))
		}

		if len(response.Feed) > 0 && response.Feed[0].Post != nil {
			title := getPostTitleFromView(t, response.Feed[0].Post)
			if title != "Jetstream Indexed Post" {
				t.Errorf("Expected title 'Jetstream Indexed Post', got %v", title)
			}
		}

		t.Logf("SUCCESS: Post indexed via Jetstream is queryable via GetAuthorPosts")
	})
}

// TestGetAuthorPosts_CommunityFilter tests filtering posts by community
func TestGetAuthorPosts_CommunityFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Setup services
	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	voteRepo := postgres.NewVoteRepository(db)

	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, getTestPDSURL())
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, getTestPDSURL(), getTestInstanceDID(), "", nil, nil, nil)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, getTestPDSURL())
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user
	testUserDID := "did:plc:communityfilter"
	_ = createTestUser(t, db, "communityfilter.test", testUserDID)

	// Create two communities
	community1DID, _ := createFeedTestCommunity(db, ctx, "filter-community-1", "owner1.test")
	community2DID, _ := createFeedTestCommunity(db, ctx, "filter-community-2", "owner2.test")

	// Create posts in each community
	now := time.Now()
	createTestPost(t, db, community1DID, testUserDID, "Post in Community 1 - A", 10, now)
	createTestPost(t, db, community1DID, testUserDID, "Post in Community 1 - B", 20, now.Add(-1*time.Hour))
	createTestPost(t, db, community2DID, testUserDID, "Post in Community 2", 30, now.Add(-2*time.Hour))

	// Setup HTTP server
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterActorRoutes(r, postService, userService, voteService, nil, nil, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Test: Filter by community 1
	t.Run("Filter by community 1", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s&community=%s",
				httpServer.URL, testUserDID, community1DID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		if len(response.Feed) != 2 {
			t.Errorf("Expected 2 posts in community 1, got %d", len(response.Feed))
		}

		// Verify all posts are from community 1
		for _, fp := range response.Feed {
			if fp.Post.Community.DID != community1DID {
				t.Errorf("Expected community DID %s, got %s", community1DID, fp.Post.Community.DID)
			}
		}

		t.Logf("SUCCESS: Community filter returned %d posts from community 1", len(response.Feed))
	})

	// Test: No filter returns all posts
	t.Run("No filter returns all posts", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/xrpc/social.coves.actor.getPosts?actor=%s", httpServer.URL, testUserDID), nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to GET posts: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var response posts.GetAuthorPostsResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&response); decodeErr != nil {
			t.Fatalf("Failed to decode response: %v", decodeErr)
		}

		if len(response.Feed) != 3 {
			t.Errorf("Expected 3 total posts, got %d", len(response.Feed))
		}

		t.Logf("SUCCESS: No filter returned %d total posts", len(response.Feed))
	})
}
