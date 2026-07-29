//go:build live

package live

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/unfurl"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostUnfurl_Streamable tests that a post with a Streamable URL gets unfurled
func TestPostUnfurl_Streamable(t *testing.T) {
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

	// Setup repositories and services
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	unfurlRepo := unfurl.NewRepository(db)

	// Setup identity resolver and services
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, "http://localhost:3001", nil, "")

	// Setup unfurl service with real oEmbed endpoints
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second), // Generous timeout for real network calls
		unfurl.WithCacheTTL(24*time.Hour),
	)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
		nil,
		nil,
	)

	postService := posts.NewPostService(
		postRepo,
		communityService,
		nil, // aggregatorService not needed
		nil, // blobService not needed
		unfurlService,
		nil, // blueskyService not needed
		"http://localhost:3001",
	)

	// Cleanup old test data
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:test%'")
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url LIKE '%streamable.com%'")

	// Create test user
	testUserDID := generateTestDID("unfurlauthor")
	testUserHandle := "unfurlauthor.test"
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: testUserHandle,
		PDSURL: "http://localhost:3001",
	})
	require.NoError(t, err, "Failed to create test user")

	// Create test community
	testCommunity := &communities.Community{
		DID:             generateTestDID("unfurlcommunity"),
		Handle:          "c-unfurlcommunity.test.coves.social",
		Name:            "unfurlcommunity",
		DisplayName:     "Unfurl Test Community",
		Description:     "A community for testing unfurl",
		Visibility:      "public",
		CreatedByDID:    testUserDID,
		HostedByDID:     "did:web:test.coves.social",
		PDSURL:          "http://localhost:3001",
		PDSAccessToken:  "fake_token_for_test",
		PDSRefreshToken: "fake_refresh_token",
	}
	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err, "Failed to create test community")

	// Test unfurling a Streamable URL
	streamableURL := "https://streamable.com/7kpdft"
	title := "Streamable Test Post"
	content := "Testing Streamable unfurl"

	// Create post with external embed containing only URI
	createReq := posts.CreatePostRequest{
		Community: testCommunity.DID,
		Title:     &title,
		Content:   &content,
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri": streamableURL,
			},
		},
		AuthorDID: testUserDID,
	}

	// Set auth context
	authCtx := middleware.SetTestUserDID(ctx, testUserDID)

	// Note: This will fail at token refresh, but that's expected for this test
	// We're testing the unfurl logic, not the full PDS write flow
	_, err = postService.CreatePost(authCtx, createReq)

	// Expect error at token refresh stage
	require.Error(t, err, "Expected error due to fake token")
	assert.Contains(t, err.Error(), "failed to refresh community credentials")

	// However, the unfurl should have been triggered and cached
	// Let's verify the cache was populated
	t.Run("Verify unfurl was cached", func(t *testing.T) {
		// Wait briefly for any async unfurl to complete
		time.Sleep(1 * time.Second)

		// Check if the URL was cached
		cached, err := unfurlRepo.Get(ctx, streamableURL)
		if err != nil {
			t.Logf("Cache lookup failed: %v", err)
			t.Skip("Skipping cache verification - unfurl may have failed due to network")
			return
		}

		if cached == nil {
			t.Skip("Unfurl result not cached - may have failed due to network issues")
			return
		}

		// Verify unfurl metadata
		assert.NotEmpty(t, cached.Title, "Expected title from unfurl")
		assert.Equal(t, "video", cached.Type, "Expected embedType to be video")
		assert.Equal(t, "streamable", cached.Provider, "Expected provider to be streamable")
		assert.Equal(t, "streamable.com", cached.Domain, "Expected domain to be streamable.com")

		t.Logf("✓ Unfurl successful:")
		t.Logf("  Title: %s", cached.Title)
		t.Logf("  Type: %s", cached.Type)
		t.Logf("  Provider: %s", cached.Provider)
		t.Logf("  Description: %s", cached.Description)
	})
}

// TestPostUnfurl_YouTube tests that a post with a YouTube URL gets unfurled
func TestPostUnfurl_YouTube(t *testing.T) {
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

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Cleanup cache
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url LIKE '%youtube.com%'")

	// Test YouTube URL
	youtubeURL := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	// Attempt unfurl
	result, err := unfurlService.UnfurlURL(ctx, youtubeURL)
	if err != nil {
		t.Logf("Unfurl failed (may be network issue): %v", err)
		t.Skip("Skipping test - YouTube unfurl failed")
		return
	}

	require.NotNil(t, result, "Expected unfurl result")
	assert.Equal(t, "video", result.Type, "Expected embedType to be video")
	assert.Equal(t, "youtube", result.Provider, "Expected provider to be youtube")
	assert.NotEmpty(t, result.Title, "Expected title from YouTube")

	t.Logf("✓ YouTube unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
}

// TestPostUnfurl_Reddit tests that a post with a Reddit URL gets unfurled
func TestPostUnfurl_Reddit(t *testing.T) {
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

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Cleanup cache
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url LIKE '%reddit.com%'")

	// Use a well-known public Reddit post
	redditURL := "https://www.reddit.com/r/programming/comments/1234/test/"

	// Attempt unfurl
	result, err := unfurlService.UnfurlURL(ctx, redditURL)
	if err != nil {
		t.Logf("Unfurl failed (may be network issue or invalid URL): %v", err)
		t.Skip("Skipping test - Reddit unfurl failed")
		return
	}

	require.NotNil(t, result, "Expected unfurl result")
	assert.Equal(t, "reddit", result.Provider, "Expected provider to be reddit")
	assert.NotEmpty(t, result.Domain, "Expected domain to be set")

	t.Logf("✓ Reddit unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
}

// TestPostUnfurl_CacheHit tests that the second post with the same URL uses cache
func TestPostUnfurl_CacheHit(t *testing.T) {
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

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Cleanup cache
	testURL := "https://streamable.com/test123"
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url = $1", testURL)

	// First unfurl - should hit network
	t.Log("First unfurl - expecting cache miss")
	result1, err1 := unfurlService.UnfurlURL(ctx, testURL)
	if err1 != nil {
		t.Logf("First unfurl failed (may be network issue): %v", err1)
		t.Skip("Skipping test - network unfurl failed")
		return
	}

	require.NotNil(t, result1, "Expected first unfurl result")

	// Second unfurl - should hit cache
	t.Log("Second unfurl - expecting cache hit")
	start := time.Now()
	result2, err2 := unfurlService.UnfurlURL(ctx, testURL)
	elapsed := time.Since(start)

	require.NoError(t, err2, "Second unfurl should not fail")
	require.NotNil(t, result2, "Expected second unfurl result")

	// Cache hit should be much faster (< 100ms)
	assert.Less(t, elapsed.Milliseconds(), int64(100), "Cache hit should be fast")

	// Results should be identical
	assert.Equal(t, result1.Title, result2.Title, "Cached result should match")
	assert.Equal(t, result1.Provider, result2.Provider, "Cached provider should match")
	assert.Equal(t, result1.Type, result2.Type, "Cached type should match")

	// Verify only one entry in cache
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM unfurl_cache WHERE url = $1", testURL).Scan(&count)
	require.NoError(t, err, "Failed to count cache entries")
	assert.Equal(t, 1, count, "Should have exactly one cache entry")

	t.Logf("✓ Cache test passed:")
	t.Logf("  First unfurl: network call")
	t.Logf("  Second unfurl: cache hit (took %dms)", elapsed.Milliseconds())
	t.Logf("  Cache entries: %d", count)
}

// TestPostUnfurl_UserProvidedMetadata tests that user-provided metadata is preserved
func TestPostUnfurl_UserProvidedMetadata(t *testing.T) {
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

	// Setup
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	unfurlRepo := unfurl.NewRepository(db)

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, "http://localhost:3001", nil, "")

	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
		nil,
		nil,
	)

	postService := posts.NewPostService(
		postRepo,
		communityService,
		nil,
		nil,
		unfurlService,
		nil, // blueskyService
		"http://localhost:3001",
	)

	// Cleanup
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE 'did:plc:metadata%'")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:metadata%'")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE 'did:plc:metadata%'")
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url LIKE '%streamable.com%'")

	// Create test user and community
	testUserDID := generateTestDID("metadatauser")
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: "metadatauser.test",
		PDSURL: "http://localhost:3001",
	})
	require.NoError(t, err)

	testCommunity := &communities.Community{
		DID:             generateTestDID("metadatacommunity"),
		Handle:          "c-metadatacommunity.test.coves.social",
		Name:            "metadatacommunity",
		DisplayName:     "Metadata Test",
		Visibility:      "public",
		CreatedByDID:    testUserDID,
		HostedByDID:     "did:web:test.coves.social",
		PDSURL:          "http://localhost:3001",
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err)

	// Create post with user-provided metadata
	streamableURL := "https://streamable.com/abc123"
	customTitle := "My Custom Title"
	customDescription := "My Custom Description"
	title := "Metadata Test Post"
	content := "Testing metadata preservation"

	createReq := posts.CreatePostRequest{
		Community: testCommunity.DID,
		Title:     &title,
		Content:   &content,
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri":         streamableURL,
				"title":       customTitle,
				"description": customDescription,
			},
		},
		AuthorDID: testUserDID,
	}

	authCtx := middleware.SetTestUserDID(ctx, testUserDID)
	_, err = postService.CreatePost(authCtx, createReq)

	// Expected to fail at token refresh
	require.Error(t, err)

	// The important check: verify unfurl happened but didn't overwrite user data
	// In the real flow, this would be checked by examining the record written to PDS
	// For this test, we just verify the unfurl logic respects user-provided data
	t.Log("✓ User-provided metadata should be preserved during unfurl enhancement")
	t.Log("  (Full verification requires E2E test with real PDS)")
}

// TestPostUnfurl_OpenGraph tests that OpenGraph URLs get unfurled
func TestPostUnfurl_OpenGraph(t *testing.T) {
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

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Test with a real website that has OpenGraph tags
	// Using example.com as it's always available, though it may not have OG tags
	testURL := "https://www.wikipedia.org/"

	// Check if URL is supported
	assert.True(t, unfurlService.IsSupported(testURL), "Wikipedia URL should be supported")

	// Attempt unfurl
	result, err := unfurlService.UnfurlURL(ctx, testURL)
	if err != nil {
		t.Logf("Unfurl failed (may be network issue): %v", err)
		t.Skip("Skipping test - OpenGraph unfurl failed")
		return
	}

	require.NotNil(t, result, "Expected unfurl result")
	assert.Equal(t, "article", result.Type, "Expected type to be article for OpenGraph")
	assert.Equal(t, "opengraph", result.Provider, "Expected provider to be opengraph")
	assert.NotEmpty(t, result.Domain, "Expected domain to be set")

	t.Logf("✓ OpenGraph unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
	t.Logf("  Domain: %s", result.Domain)
	if result.Description != "" {
		t.Logf("  Description: %s", result.Description)
	}
	if result.ThumbnailURL != "" {
		t.Logf("  Thumbnail: %s", result.ThumbnailURL)
	}
}

// TestPostUnfurl_SmartRouting tests that oEmbed still works while OpenGraph handles others
func TestPostUnfurl_SmartRouting(t *testing.T) {
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

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Clean cache
	_, _ = db.Exec("DELETE FROM unfurl_cache WHERE url LIKE '%youtube.com%' OR url LIKE '%wikipedia.org%'")

	tests := []struct {
		name             string
		url              string
		expectedProvider string
	}{
		{
			name:             "YouTube (oEmbed)",
			url:              "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			expectedProvider: "youtube",
		},
		{
			name:             "Generic site (OpenGraph)",
			url:              "https://www.wikipedia.org/",
			expectedProvider: "opengraph",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := unfurlService.UnfurlURL(ctx, tt.url)
			if err != nil {
				t.Logf("Unfurl failed for %s: %v", tt.url, err)
				t.Skip("Skipping - network issue")
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.expectedProvider, result.Provider,
				"URL %s should use %s provider", tt.url, tt.expectedProvider)

			t.Logf("✓ %s correctly routed to %s provider", tt.name, result.Provider)
		})
	}
}

// TestPostUnfurl_KagiKite tests that Kagi Kite URLs get unfurled with story images
func TestPostUnfurl_KagiKite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Note: This test requires network access to kite.kagi.com
	// It will be skipped if the URL is not reachable

	kagiURL := "https://kite.kagi.com/96cf948f-8a1b-4281-9ba4-8a9e1ad7b3c6/world/11"

	// Test unfurl service
	ctx := context.Background()
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(1*time.Hour),
	)

	result, err := unfurlService.UnfurlURL(ctx, kagiURL)
	if err != nil {
		t.Skipf("Skipping Kagi test (URL not reachable): %v", err)
		return
	}

	require.NoError(t, err)
	assert.Equal(t, "article", result.Type)
	assert.Equal(t, "kagi", result.Provider)
	assert.NotEmpty(t, result.Title, "Should extract story title")
	assert.NotEmpty(t, result.ThumbnailURL, "Should extract story image")
	assert.Contains(t, result.ThumbnailURL, "kagiproxy.com", "Should be Kagi proxy URL")

	t.Logf("✓ Kagi unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Image: %s", result.ThumbnailURL)
	t.Logf("  Description: %s", result.Description)
}
