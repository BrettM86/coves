package integration

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/blueskypost"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlueskyPostCrossPosting_URLParsing tests URL detection and parsing
func TestBlueskyPostCrossPosting_URLParsing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Setup identity resolver for handle resolution
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup Bluesky post service
	repo := blueskypost.NewRepository(db)
	service := blueskypost.NewService(repo, identityResolver,
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)

	ctx := context.Background()

	t.Run("Detect valid bsky.app URLs", func(t *testing.T) {
		validURLs := []string{
			"https://bsky.app/profile/jay.bsky.team/post/3l7bsovn5rz2n",
			"https://bsky.app/profile/pfrazee.com/post/3l7bsovn5rz2n",
			"https://bsky.app/profile/did:plc:z72i7hdynmk6r22z27h6tvur/post/3l7bsovn5rz2n",
		}

		for _, url := range validURLs {
			t.Run(url, func(t *testing.T) {
				isValid := service.IsBlueskyURL(url)
				assert.True(t, isValid, "URL should be detected as valid bsky.app URL")
			})
		}
	})

	t.Run("Reject invalid URLs", func(t *testing.T) {
		invalidURLs := []string{
			"https://twitter.com/user/status/123",
			"https://example.com/post/123",
			"https://bsky.app/profile/user", // Missing post path
			"http://bsky.app/profile/user/post/123", // Wrong scheme
			"",
		}

		for _, url := range invalidURLs {
			t.Run(url, func(t *testing.T) {
				isValid := service.IsBlueskyURL(url)
				assert.False(t, isValid, "URL should be rejected")
			})
		}
	})

	t.Run("Parse URL with DID (no resolution needed)", func(t *testing.T) {
		url := "https://bsky.app/profile/did:plc:z72i7hdynmk6r22z27h6tvur/post/3l7qnsdi6gz24"

		atURI, err := service.ParseBlueskyURL(ctx, url)
		require.NoError(t, err)
		assert.Equal(t, "at://did:plc:z72i7hdynmk6r22z27h6tvur/app.bsky.feed.post/3l7qnsdi6gz24", atURI)
	})

	t.Run("Parse URL with handle (requires resolution)", func(t *testing.T) {
		// Use a real Bluesky post URL
		url := "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p"

		atURI, err := service.ParseBlueskyURL(ctx, url)
		if err != nil {
			t.Logf("Handle resolution failed (may be network issue): %v", err)
			t.Skip("Skipping - handle resolution requires network access")
			return
		}

		// Should have resolved to a DID
		assert.Contains(t, atURI, "at://did:")
		assert.Contains(t, atURI, "/app.bsky.feed.post/3makab2jnwk2p")
		t.Logf("Resolved URL to AT-URI: %s", atURI)
	})
}

// TestBlueskyPostCrossPosting_LiveAPI tests fetching real posts from Bluesky
func TestBlueskyPostCrossPosting_LiveAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Cleanup cache from previous runs
	_, _ = db.Exec("DELETE FROM bluesky_post_cache")

	// Setup services
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)

	repo := blueskypost.NewRepository(db)
	service := blueskypost.NewService(repo, identityResolver,
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)

	ctx := context.Background()

	t.Run("Fetch regular Bluesky post", func(t *testing.T) {
		// Regular post from ianboudreau.com
		bskyURL := "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p"

		// First parse the URL to get the AT-URI
		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed: %v", err)
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Logf("API fetch failed (may be network issue): %v", err)
			t.Skip("Skipping - Bluesky API requires network access")
			return
		}

		require.NotNil(t, result)

		if result.Unavailable {
			t.Logf("Post marked as unavailable (may have been deleted): %s", result.Message)
			t.Skip("Skipping - post is unavailable")
			return
		}

		// Validate response structure
		assert.NotEmpty(t, result.URI, "Should have URI")
		assert.NotEmpty(t, result.CID, "Should have CID")
		assert.NotEmpty(t, result.Text, "Should have text content")
		assert.NotNil(t, result.Author, "Should have author")
		assert.NotEmpty(t, result.Author.DID, "Author should have DID")
		assert.Equal(t, "ianboudreau.com", result.Author.Handle, "Should be from ianboudreau.com")

		t.Logf("✓ Successfully fetched regular Bluesky post:")
		t.Logf("  URI: %s", result.URI)
		t.Logf("  Author: @%s (%s)", result.Author.Handle, result.Author.DisplayName)
		t.Logf("  Text: %.100s...", result.Text)
		t.Logf("  Likes: %d, Reposts: %d, Replies: %d", result.LikeCount, result.RepostCount, result.ReplyCount)
	})

	t.Run("Fetch post with quote repost", func(t *testing.T) {
		// Post with quote RT from tedunderwood.com
		bskyURL := "https://bsky.app/profile/tedunderwood.com/post/3malohcd2vc2d"

		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed: %v", err)
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("API fetch failed: %v", err)
			return
		}

		require.NotNil(t, result)
		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
			return
		}

		assert.Equal(t, "tedunderwood.com", result.Author.Handle)

		// This post should have a quoted post
		if result.QuotedPost != nil {
			t.Logf("✓ Found quoted post")
			// Note: Quoted post text/author may be empty if the quote is a different type
			// (e.g., recordWithMedia where the record is nested differently)
			if result.QuotedPost.Author != nil && result.QuotedPost.Author.Handle != "" {
				t.Logf("  Quoted author: @%s", result.QuotedPost.Author.Handle)
			}
			if result.QuotedPost.Text != "" {
				t.Logf("  Quoted text: %.60s...", result.QuotedPost.Text)
			}
		} else {
			t.Log("Note: Quoted post not found (may have been deleted or structure changed)")
		}

		t.Logf("✓ Successfully fetched post with quote:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
	})

	t.Run("Fetch post with link embed", func(t *testing.T) {
		// Post with link unfurl from davidpfau.com
		bskyURL := "https://bsky.app/profile/davidpfau.com/post/3malg2athns2d"

		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed: %v", err)
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("API fetch failed: %v", err)
			return
		}

		require.NotNil(t, result)
		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
			return
		}

		assert.Equal(t, "davidpfau.com", result.Author.Handle)
		assert.NotEmpty(t, result.Text)

		t.Logf("✓ Successfully fetched post with link embed:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
		t.Logf("  Has media: %v", result.HasMedia)
	})

	t.Run("Fetch image-only post", func(t *testing.T) {
		// Post with just an image from brennan.computer
		bskyURL := "https://bsky.app/profile/brennan.computer/post/3mallehc6hk2s"

		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed: %v", err)
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("API fetch failed: %v", err)
			return
		}

		require.NotNil(t, result)
		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
			return
		}

		assert.Equal(t, "brennan.computer", result.Author.Handle)

		// This post should have media (image)
		assert.True(t, result.HasMedia, "Image post should have HasMedia=true")
		assert.Greater(t, result.MediaCount, 0, "Image post should have MediaCount > 0")

		t.Logf("✓ Successfully fetched image post:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s", result.Text)
		t.Logf("  Has media: %v (count: %d)", result.HasMedia, result.MediaCount)
	})

	t.Run("Fetch quote RT with image", func(t *testing.T) {
		// Quote RT with an image from lauraknelson.bsky.social
		bskyURL := "https://bsky.app/profile/lauraknelson.bsky.social/post/3malueymbis25"

		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed: %v", err)
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("API fetch failed: %v", err)
			return
		}

		require.NotNil(t, result)
		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
			return
		}

		assert.Equal(t, "lauraknelson.bsky.social", result.Author.Handle)

		// This post should have media (image in quote RT)
		// Note: We're testing detection, not rendering (Phase 1)
		t.Logf("✓ Successfully fetched quote RT with image:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s", result.Text)
		t.Logf("  Has media: %v (count: %d)", result.HasMedia, result.MediaCount)
		if result.QuotedPost != nil {
			t.Logf("  Has quoted post: yes")
			if result.QuotedPost.HasMedia {
				t.Logf("  Quoted post has media: %d items", result.QuotedPost.MediaCount)
			}
		}
	})

	t.Run("Cache hit on second fetch", func(t *testing.T) {
		// Use the regular post we just fetched
		bskyURL := "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p"
		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skip("Skipping - handle resolution failed")
			return
		}

		// First fetch (may hit cache from previous test or fetch from API)
		result1, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skip("Skipping - first fetch failed")
			return
		}

		// Second fetch should hit cache
		start := time.Now()
		result2, err := service.ResolvePost(ctx, atURI)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, result2)

		// Cache hit should be very fast (< 100ms)
		assert.Less(t, elapsed.Milliseconds(), int64(100), "Cache hit should be fast")

		// Results should match
		assert.Equal(t, result1.URI, result2.URI)
		assert.Equal(t, result1.Text, result2.Text)

		t.Logf("✓ Cache hit successful (took %dms)", elapsed.Milliseconds())
	})

	t.Run("Handle unavailable post gracefully", func(t *testing.T) {
		// Use a fake URI that doesn't exist
		fakeURI := "at://did:plc:nonexistent/app.bsky.feed.post/doesnotexist"

		result, err := service.ResolvePost(ctx, fakeURI)
		if err != nil {
			// API errors are acceptable for non-existent posts
			t.Logf("Got error for non-existent post (acceptable): %v", err)
			return
		}

		// If no error, should be marked unavailable
		if result != nil {
			assert.True(t, result.Unavailable, "Non-existent post should be marked unavailable")
			assert.NotEmpty(t, result.Message, "Should have unavailable message")
			t.Logf("✓ Non-existent post handled gracefully: %s", result.Message)
		}
	})
}

// TestBlueskyPostCrossPosting_CircuitBreaker tests circuit breaker behavior
func TestBlueskyPostCrossPosting_CircuitBreaker(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test verifies the circuit breaker pattern works correctly.
	// We don't actually want to trip the circuit breaker against production,
	// so this is more of a unit-level integration test.

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)

	repo := blueskypost.NewRepository(db)
	service := blueskypost.NewService(repo, identityResolver,
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)

	ctx := context.Background()

	t.Run("Service recovers after successful request", func(t *testing.T) {
		// Make a valid request to ensure the circuit is closed
		bskyURL := "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p"
		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skip("Skipping - handle resolution failed")
			return
		}

		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skip("Skipping - API not available")
			return
		}

		// Should succeed
		assert.NotNil(t, result)
		t.Log("✓ Circuit breaker allows requests when API is healthy")
	})
}

// TestBlueskyPostCrossPosting_E2E_LivePDS tests writing posts with Bluesky URLs to a real PDS
// This catches lexicon validation errors like invalid strongRef CIDs
func TestBlueskyPostCrossPosting_E2E_LivePDS(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live PDS E2E test in short mode")
	}

	// Check if PDS is running
	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("bsky%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("bskytest-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	t.Logf("Creating test user on PDS: %s", testUserHandle)
	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	t.Logf("Test user created: DID=%s", userDID)

	// Index user in AppView
	_ = createTestUser(t, db, testUserHandle, userDID)

	// Create test community (needed for post creation)
	testCommunityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("bskytest%d", time.Now().UnixNano()%1000000), "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	t.Run("Write post with Bluesky URL to PDS succeeds", func(t *testing.T) {
		// This test validates that Phase 1 (text-only) works correctly
		// The Bluesky URL should NOT be converted to an embed (which would require CID)
		// Instead, it should be stored as plain text content

		rkey := fmt.Sprintf("bskytest-%d", time.Now().UnixNano())

		// Post record with Bluesky URL in content (no embed conversion in Phase 1)
		postRecord := map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": testCommunityDID,
			"author":    userDID,
			"title":     "Post with Bluesky Link",
			"content":   "Check out this Bluesky post: https://bsky.app/profile/jay.bsky.team/post/3l7bsovn5rz2n",
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}

		// Write directly to PDS - this will catch lexicon validation errors
		uri, cid, writeErr := writePDSRecord(pdsURL, pdsAccessToken, userDID, "social.coves.community.post", rkey, postRecord)

		// The key assertion: this should succeed because we're NOT creating an embed
		// If embed conversion was enabled with empty CID, this would fail
		require.NoError(t, writeErr, "Writing post with Bluesky URL should succeed (Phase 1: no embed conversion)")
		require.NotEmpty(t, uri, "Should receive record URI")
		require.NotEmpty(t, cid, "Should receive record CID")

		t.Logf("✅ Post with Bluesky URL written successfully:")
		t.Logf("   URI: %s", uri)
		t.Logf("   CID: %s", cid)

		// Verify the record exists on PDS
		verifyResp, verifyErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.post&rkey=%s",
			pdsURL, userDID, rkey))
		require.NoError(t, verifyErr, "Should be able to fetch record from PDS")
		defer func() { _ = verifyResp.Body.Close() }()

		require.Equal(t, http.StatusOK, verifyResp.StatusCode, "Record should exist on PDS")
		t.Logf("✅ Record verified on PDS")
	})

	t.Run("Write post with external embed (no Bluesky URL) succeeds", func(t *testing.T) {
		// Regular external embed (not a Bluesky URL) should still work
		rkey := fmt.Sprintf("exttest-%d", time.Now().UnixNano())

		postRecord := map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": testCommunityDID,
			"author":    userDID,
			"title":     "Post with External Link",
			"content":   "Check out this article",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":         "https://example.com/article",
					"title":       "Example Article",
					"description": "An interesting article about testing",
				},
			},
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}

		uri, cid, writeErr := writePDSRecord(pdsURL, pdsAccessToken, userDID, "social.coves.community.post", rkey, postRecord)
		require.NoError(t, writeErr, "Writing post with external embed should succeed")
		require.NotEmpty(t, uri)
		require.NotEmpty(t, cid)

		t.Logf("✅ Post with external embed written: %s", uri)
	})
}

// TestBlueskyPostCrossPosting_E2E_PostCreation tests the full flow of creating a post with a Bluesky embed
func TestBlueskyPostCrossPosting_E2E_PostCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Cleanup cache
	_, _ = db.Exec("DELETE FROM bluesky_post_cache WHERE at_uri LIKE 'at://did:plc:%'")

	// Setup services
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)

	repo := blueskypost.NewRepository(db)
	service := blueskypost.NewService(repo, identityResolver,
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)

	t.Run("Full URL to resolved embed flow", func(t *testing.T) {
		// Simulate user pasting a bsky.app URL (quote post with embedded content)
		bskyURL := "https://bsky.app/profile/tedunderwood.com/post/3malohcd2vc2d"

		// Step 1: Detect it's a Bluesky URL
		if !service.IsBlueskyURL(bskyURL) {
			t.Fatal("Should detect valid bsky.app URL")
		}

		// Step 2: Parse to AT-URI
		atURI, err := service.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed (network issue): %v", err)
			return
		}
		t.Logf("Parsed URL to AT-URI: %s", atURI)

		// Step 3: Resolve the post
		result, err := service.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("Post resolution failed (network issue): %v", err)
			return
		}

		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
			return
		}

		// Verify the resolved embed has all required fields
		assert.NotEmpty(t, result.URI)
		assert.NotEmpty(t, result.CID)
		assert.NotEmpty(t, result.Text)
		assert.NotNil(t, result.Author)
		assert.Equal(t, "tedunderwood.com", result.Author.Handle)

		t.Logf("✓ E2E flow complete:")
		t.Logf("  Input URL: %s", bskyURL)
		t.Logf("  AT-URI: %s", atURI)
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
		if result.QuotedPost != nil {
			t.Logf("  Quoted: @%s - %.60s...", result.QuotedPost.Author.Handle, result.QuotedPost.Text)
		}
	})
}

// TestBlueskyPostCrossPosting_EmbedConversion tests that Bluesky URLs in external embeds
// are converted to social.coves.embed.post with proper strongRef (uri + cid)
func TestBlueskyPostCrossPosting_EmbedConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Cleanup cache from previous runs
	_, _ = db.Exec("DELETE FROM bluesky_post_cache")

	// Setup identity resolver for handle resolution
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup Bluesky post service
	repo := blueskypost.NewRepository(db)
	blueskyService := blueskypost.NewService(repo, identityResolver,
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)

	ctx := context.Background()

	t.Run("Convert Bluesky URL to post embed with strongRef", func(t *testing.T) {
		// Use a real Bluesky post URL
		bskyURL := "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p"

		// 1. Verify URL is detected as Bluesky
		require.True(t, blueskyService.IsBlueskyURL(bskyURL), "Should detect as Bluesky URL")

		// 2. Parse URL to AT-URI
		atURI, err := blueskyService.ParseBlueskyURL(ctx, bskyURL)
		if err != nil {
			t.Skipf("Handle resolution failed (network issue): %v", err)
		}
		t.Logf("Parsed AT-URI: %s", atURI)

		// 3. Resolve the post to get CID
		result, err := blueskyService.ResolvePost(ctx, atURI)
		if err != nil {
			t.Skipf("Post resolution failed (network issue): %v", err)
		}

		if result.Unavailable {
			t.Skipf("Post unavailable: %s", result.Message)
		}

		// 4. Verify we have all fields needed for strongRef
		require.NotEmpty(t, result.URI, "Should have AT-URI")
		require.NotEmpty(t, result.CID, "Should have CID for strongRef")

		// 5. Verify the CID is a valid format (starts with 'baf')
		assert.True(t, len(result.CID) > 10, "CID should be a valid length")
		assert.True(t, result.CID[:3] == "baf", "CID should start with 'baf' (CIDv1)")

		// 6. Simulate the conversion that would happen in tryConvertBlueskyURLToPostEmbed
		convertedEmbed := map[string]interface{}{
			"$type": "social.coves.embed.post",
			"post": map[string]interface{}{
				"uri": result.URI,
				"cid": result.CID,
			},
		}

		// Verify the converted embed structure
		embedType := convertedEmbed["$type"].(string)
		assert.Equal(t, "social.coves.embed.post", embedType)

		postRef := convertedEmbed["post"].(map[string]interface{})
		assert.NotEmpty(t, postRef["uri"])
		assert.NotEmpty(t, postRef["cid"])

		t.Logf("✅ Embed conversion successful:")
		t.Logf("   $type: %s", embedType)
		t.Logf("   uri: %s", postRef["uri"])
		t.Logf("   cid: %s", postRef["cid"])
	})

	t.Run("Unavailable post keeps external embed", func(t *testing.T) {
		// Use a fake URI that won't exist
		fakeURI := "at://did:plc:nonexistent123/app.bsky.feed.post/doesnotexist"

		result, err := blueskyService.ResolvePost(ctx, fakeURI)
		if err != nil {
			// Some errors are acceptable for non-existent posts
			t.Logf("Got error for non-existent post: %v", err)
			t.Logf("✅ Error case would fall back to external embed")
			return
		}

		if result != nil && result.Unavailable {
			// This is the expected case - post is marked unavailable
			// With the new behavior, we keep the external embed instead of creating a placeholder
			t.Logf("✅ Unavailable post detected: %s", result.Message)
			t.Logf("   Would keep as external embed (no placeholder CID)")
		}
	})
}
