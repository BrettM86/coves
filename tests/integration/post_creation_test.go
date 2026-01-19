package integration

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostCreation_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Setup: Initialize services
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	communityRepo := postgres.NewCommunityRepository(db)
	// Note: Provisioner not needed for this test (we're not actually creating communities)
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil, // provisioner
		nil, // pdsClientFactory
		nil, // blobService
	)

	postRepo := postgres.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, "http://localhost:3001") // nil aggregatorService, blobService, unfurlService, blueskyService for user-only tests

	ctx := context.Background()

	// Cleanup: Remove any existing test data
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:test%'")

	// Setup: Create test user
	testUserDID := generateTestDID("postauthor")
	testUserHandle := "postauthor.test"

	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: testUserHandle,
		PDSURL: "http://localhost:3001",
	})
	require.NoError(t, err, "Failed to create test user")

	// Setup: Create test community (insert directly to DB for speed)
	testCommunity := &communities.Community{
		DID:            generateTestDID("testcommunity"),
		Handle:         "c-testcommunity.test.coves.social", // Canonical atProto handle (no ! prefix, c- format)
		Name:           "testcommunity",
		DisplayName:    "Test Community",
		Description:    "A community for testing posts",
		Visibility:     "public",
		CreatedByDID:   testUserDID,
		HostedByDID:    "did:web:test.coves.social",
		PDSURL:         "http://localhost:3001",
		PDSAccessToken: "fake_token_for_test", // Won't actually call PDS in unit test
	}

	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err, "Failed to create test community")

	t.Run("Create text post successfully (with DID)", func(t *testing.T) {
		// NOTE: This test validates the service layer logic only
		// It will fail when trying to write to PDS because we're using a fake token
		// For full E2E testing, you'd need a real PDS instance

		content := "This is a test post"
		title := "Test Post Title"

		req := posts.CreatePostRequest{
			Community: testCommunity.DID, // Using DID directly
			Title:     &title,
			Content:   &content,
			AuthorDID: testUserDID,
		}

		// This will fail at token refresh step (expected for unit test)
		// We're using a fake token that can't be parsed
		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)

		// For now, we expect an error because token is fake
		// In a full E2E test with real PDS, this would succeed
		require.Error(t, err)
		t.Logf("Expected error (fake token): %v", err)
		// Verify the error is from token refresh or PDS, not validation
		assert.Contains(t, err.Error(), "failed to refresh community credentials")
	})

	t.Run("Create text post with community handle", func(t *testing.T) {
		// Test that we can use community handle instead of DID
		// This validates at-identifier resolution per atProto best practices

		content := "Post using handle instead of DID"
		title := "Handle Test"

		req := posts.CreatePostRequest{
			Community: testCommunity.Handle, // Using canonical atProto handle
			Title:     &title,
			Content:   &content,
			AuthorDID: testUserDID,
		}

		// Should resolve handle to DID and proceed
		// Will still fail at token refresh (expected with fake token)
		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		// Should fail at token refresh, not community resolution
		assert.Contains(t, err.Error(), "failed to refresh community credentials")
	})

	t.Run("Create text post with ! prefix handle", func(t *testing.T) {
		// Test that we can also use ! prefix with scoped format: !name@instance
		// This is Coves-specific UX shorthand for c-name.instance

		content := "Post using !-prefixed handle"
		title := "Prefixed Handle Test"

		// Extract name from handle: "c-gardening.coves.social" -> "gardening"
		// Scoped format: !gardening@coves.social
		handleParts := strings.Split(testCommunity.Handle, ".")
		communityNameWithPrefix := handleParts[0] // "c-gardening"
		communityName := strings.TrimPrefix(communityNameWithPrefix, "c-") // "gardening"
		instanceDomain := strings.Join(handleParts[1:], ".") // "coves.social"
		scopedHandle := fmt.Sprintf("!%s@%s", communityName, instanceDomain)

		req := posts.CreatePostRequest{
			Community: scopedHandle, // !gardening@coves.social
			Title:     &title,
			Content:   &content,
			AuthorDID: testUserDID,
		}

		// Should resolve handle to DID and proceed
		// Will still fail at token refresh (expected with fake token)
		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		// Should fail at token refresh, not community resolution
		assert.Contains(t, err.Error(), "failed to refresh community credentials")
	})

	t.Run("Reject post with missing community", func(t *testing.T) {
		content := "Post without community"

		req := posts.CreatePostRequest{
			Community: "", // Missing!
			Content:   &content,
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err))
	})

	t.Run("Reject post with non-existent community handle", func(t *testing.T) {
		content := "Post with non-existent handle"

		req := posts.CreatePostRequest{
			Community: "c-nonexistent.test.coves.social", // Valid canonical handle format, but doesn't exist
			Content:   &content,
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		// Should fail with community not found (wrapped in error)
		assert.Contains(t, err.Error(), "community not found")
	})

	t.Run("Reject post with missing author DID", func(t *testing.T) {
		content := "Post without author"

		req := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Content:   &content,
			AuthorDID: "", // Missing!
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err))
		assert.Contains(t, err.Error(), "authorDid")
	})

	t.Run("Reject post in non-existent community", func(t *testing.T) {
		content := "Post in fake community"

		req := posts.CreatePostRequest{
			Community: "did:plc:nonexistent",
			Content:   &content,
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		assert.Equal(t, posts.ErrCommunityNotFound, err)
	})

	t.Run("Reject post with too-long content", func(t *testing.T) {
		// Create content longer than 100k characters (maxContentLength = 100000)
		longContent := string(make([]byte, 100001))

		req := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Content:   &longContent,
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err))
		assert.Contains(t, err.Error(), "too long")
	})

	t.Run("Reject post with invalid content label", func(t *testing.T) {
		content := "Post with invalid label"

		req := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Content:   &content,
			Labels: &posts.SelfLabels{
				Values: []posts.SelfLabel{
					{Val: "invalid_label"}, // Not in known values!
				},
			},
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		assert.True(t, posts.IsValidationError(err))
		assert.Contains(t, err.Error(), "unknown content label")
	})

	t.Run("Accept post with valid content labels", func(t *testing.T) {
		content := "Post with valid labels"

		req := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Content:   &content,
			Labels: &posts.SelfLabels{
				Values: []posts.SelfLabel{
					{Val: "nsfw"},
					{Val: "spoiler"},
				},
			},
			AuthorDID: testUserDID,
		}

		// Will fail at token refresh (expected with fake token)
		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, req)
		require.Error(t, err)
		// Should fail at token refresh, not validation
		assert.Contains(t, err.Error(), "failed to refresh community credentials")
	})
}

// TestPostRepository_Create tests the repository layer
func TestPostRepository_Create(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Cleanup first
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:test%'")

	// Setup: Create test user and community
	ctx := context.Background()
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)

	testUserDID := generateTestDID("postauthor2")
	_, err := userRepo.Create(ctx, &users.User{
		DID:    testUserDID,
		Handle: "postauthor2.test",
		PDSURL: "http://localhost:3001",
	})
	require.NoError(t, err)

	testCommunityDID := generateTestDID("testcommunity2")
	_, err = communityRepo.Create(ctx, &communities.Community{
		DID:          testCommunityDID,
		Handle:       "c-testcommunity2.test.coves.social", // Canonical format (no ! prefix)
		Name:         "testcommunity2",
		Visibility:   "public",
		CreatedByDID: testUserDID,
		HostedByDID:  "did:web:test.coves.social",
		PDSURL:       "http://localhost:3001",
	})
	require.NoError(t, err)

	postRepo := postgres.NewPostRepository(db)

	t.Run("Insert post successfully", func(t *testing.T) {
		content := "Test post content"
		title := "Test Title"

		post := &posts.Post{
			URI:          "at://" + testCommunityDID + "/social.coves.community.post/test123",
			CID:          "bafy2test123",
			RKey:         "test123",
			AuthorDID:    testUserDID,
			CommunityDID: testCommunityDID,
			Title:        &title,
			Content:      &content,
		}

		err := postRepo.Create(ctx, post)
		require.NoError(t, err)
		assert.NotZero(t, post.ID, "Post should have ID after insert")
		assert.NotZero(t, post.IndexedAt, "Post should have IndexedAt timestamp")
	})

	t.Run("Reject duplicate post URI", func(t *testing.T) {
		content := "Duplicate post"

		post1 := &posts.Post{
			URI:          "at://" + testCommunityDID + "/social.coves.community.post/duplicate",
			CID:          "bafy2duplicate1",
			RKey:         "duplicate",
			AuthorDID:    testUserDID,
			CommunityDID: testCommunityDID,
			Content:      &content,
		}

		err := postRepo.Create(ctx, post1)
		require.NoError(t, err)

		// Try to insert again with same URI
		post2 := &posts.Post{
			URI:          "at://" + testCommunityDID + "/social.coves.community.post/duplicate",
			CID:          "bafy2duplicate2",
			RKey:         "duplicate",
			AuthorDID:    testUserDID,
			CommunityDID: testCommunityDID,
			Content:      &content,
		}

		err = postRepo.Create(ctx, post2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already indexed")
	})
}
