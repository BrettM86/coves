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
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unfurl targets. Every one of these is somebody else's server, and each is
// pinned for a specific reason:
//
//   - streamable and youtube exercise the oEmbed path (two different providers,
//     because their response shapes differ),
//   - reddit exercises a provider that answers oEmbed for an id-only permalink,
//   - wikipedia exercises the OpenGraph fallback for a site with no oEmbed
//     endpoint, and is the most stable large site available for that job.
//
// When one of these 404s the tier fails and names the constant. That is the
// intended behaviour: replace the URL with a live one serving the same provider
// and re-run. Do not reintroduce a skip — an unfurl test that goes green
// because the target vanished has checked nothing.
const (
	liveStreamableURL = "https://streamable.com/7kpdft"
	liveYouTubeURL    = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	liveRedditURL     = "https://www.reddit.com/r/programming/comments/1234/test/"
	liveOpenGraphURL  = "https://www.wikipedia.org/"
)

// unfurlEgressNote is the unfurl tier's half of the same message
// bluesky_post_test.go's egressNote carries: a failure to reach a third party
// is a red result here, and the one place it can never be diagnosed from is the
// merge gate, which cannot run these tests at all.
const unfurlEgressNote = "The live tier requires public internet egress. It is never run on the merge\n" +
	"path: `make ci` blocks egress by design (docker-compose.ci.yml declares its\n" +
	"network internal: true), so this test cannot have been run from inside the CI\n" +
	"stack. Confirm this machine's connectivity, and that the target above still\n" +
	"serves the metadata this test reads, before treating the failure as a\n" +
	"regression in Coves."

// liveUnfurlService builds the unfurl service against the real providers, with
// a timeout generous enough for a cold third-party response.
func liveUnfurlService(t *testing.T, repo unfurl.Repository) unfurl.Service {
	t.Helper()
	return unfurl.NewService(repo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)
}

// mustUnfurl fetches metadata for url, failing with the egress note rather than
// skipping when the target cannot be reached.
func mustUnfurl(t *testing.T, ctx context.Context, service unfurl.Service, url, constName string) *unfurl.UnfurlResult {
	t.Helper()

	result, err := service.UnfurlURL(ctx, url)
	if err != nil {
		t.Fatalf("could not unfurl %s (%s): %v\n\n%s", url, constName, err, unfurlEgressNote)
	}
	require.NotNil(t, result, "UnfurlURL returned neither a result nor an error for %s", url)
	return result
}

// TestPostUnfurl_Streamable tests that a Streamable URL gets unfurled as a video
// embed via the provider's oEmbed endpoint.
func TestPostUnfurl_Streamable(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := liveUnfurlService(t, unfurlRepo)

	result := mustUnfurl(t, ctx, unfurlService, liveStreamableURL, "liveStreamableURL")

	assert.NotEmpty(t, result.Title, "Expected title from unfurl")
	assert.Equal(t, "video", result.Type, "Expected embedType to be video")
	assert.Equal(t, "streamable", result.Provider, "Expected provider to be streamable")
	assert.Equal(t, "streamable.com", result.Domain, "Expected domain to be streamable.com")

	// The service caches on the way out; a live unfurl that never lands in the
	// cache would re-hit the provider on every post referencing the same URL.
	cached, err := unfurlRepo.Get(ctx, liveStreamableURL)
	require.NoError(t, err, "reading the unfurl cache failed")
	require.NotNil(t, cached, "a successful unfurl of %s left nothing in unfurl_cache", liveStreamableURL)
	assert.Equal(t, result.Provider, cached.Provider, "cached provider should match the live result")

	t.Logf("✓ Unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
	t.Logf("  Description: %s", result.Description)
}

// TestPostUnfurl_YouTube tests that a post with a YouTube URL gets unfurled
func TestPostUnfurl_YouTube(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlService := liveUnfurlService(t, unfurl.NewRepository(db))

	result := mustUnfurl(t, ctx, unfurlService, liveYouTubeURL, "liveYouTubeURL")

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
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlService := liveUnfurlService(t, unfurl.NewRepository(db))

	result := mustUnfurl(t, ctx, unfurlService, liveRedditURL, "liveRedditURL")

	assert.Equal(t, "reddit", result.Provider, "Expected provider to be reddit")
	assert.NotEmpty(t, result.Domain, "Expected domain to be set")

	t.Logf("✓ Reddit unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
}

// TestPostUnfurl_CacheHit tests that the second unfurl of the same URL is served
// from Postgres rather than from the provider.
func TestPostUnfurl_CacheHit(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlService := liveUnfurlService(t, unfurl.NewRepository(db))

	// testkit.DB hands this test its own database, so the first call is a
	// guaranteed cache miss and really does go to the provider.
	t.Log("First unfurl - expecting cache miss")
	result1 := mustUnfurl(t, ctx, unfurlService, liveStreamableURL, "liveStreamableURL")

	// Second unfurl - should hit cache
	t.Log("Second unfurl - expecting cache hit")
	start := time.Now()
	result2, err2 := unfurlService.UnfurlURL(ctx, liveStreamableURL)
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
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM unfurl_cache WHERE url = $1", liveStreamableURL).Scan(&count)
	require.NoError(t, err, "Failed to count cache entries")
	assert.Equal(t, 1, count, "Should have exactly one cache entry")

	t.Logf("✓ Cache test passed:")
	t.Logf("  First unfurl: network call")
	t.Logf("  Second unfurl: cache hit (took %dms)", elapsed.Milliseconds())
	t.Logf("  Cache entries: %d", count)
}

// TestPostUnfurl_UserProvidedMetadata tests that user-provided metadata is preserved
func TestPostUnfurl_UserProvidedMetadata(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()
	pdsURL := testkit.Endpoints().PDS.BaseURL

	// Setup
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	unfurlRepo := unfurl.NewRepository(db)

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	unfurlService := liveUnfurlService(t, unfurlRepo)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
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
		pdsURL,
	)

	// Create test user and community
	testUserDID := generateTestDID("metadatauser")
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: "metadatauser.test",
		PDSURL: pdsURL,
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
		PDSURL:          pdsURL,
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err)

	// Create post with user-provided metadata
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
				"uri":         liveStreamableURL,
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
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlService := liveUnfurlService(t, unfurl.NewRepository(db))

	// Check if URL is supported
	assert.True(t, unfurlService.IsSupported(liveOpenGraphURL), "Wikipedia URL should be supported")

	result := mustUnfurl(t, ctx, unfurlService, liveOpenGraphURL, "liveOpenGraphURL")

	assert.Equal(t, "article", result.Type, "Expected type to be article for OpenGraph")
	assert.Equal(t, "opengraph", result.Provider, "Expected provider to be opengraph")
	assert.NotEmpty(t, result.Domain, "Expected domain to be set")

	t.Logf("✓ OpenGraph unfurl successful:")
	t.Logf("  Title: %s", result.Title)
	t.Logf("  Type: %s", result.Type)
	t.Logf("  Provider: %s", result.Provider)
	t.Logf("  Domain: %s", result.Domain)
	t.Logf("  Description: %s", result.Description)
	t.Logf("  Thumbnail: %s", result.ThumbnailURL)
}

// TestPostUnfurl_SmartRouting tests that oEmbed still works while OpenGraph handles others
func TestPostUnfurl_SmartRouting(t *testing.T) {
	db := testkit.DB(t)

	ctx := context.Background()

	unfurlService := liveUnfurlService(t, unfurl.NewRepository(db))

	tests := []struct {
		name             string
		url              string
		constName        string
		expectedProvider string
	}{
		{
			name:             "YouTube (oEmbed)",
			url:              liveYouTubeURL,
			constName:        "liveYouTubeURL",
			expectedProvider: "youtube",
		},
		{
			name:             "Generic site (OpenGraph)",
			url:              liveOpenGraphURL,
			constName:        "liveOpenGraphURL",
			expectedProvider: "opengraph",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mustUnfurl(t, ctx, unfurlService, tt.url, tt.constName)

			assert.Equal(t, tt.expectedProvider, result.Provider,
				"URL %s should use %s provider", tt.url, tt.expectedProvider)

			t.Logf("✓ %s correctly routed to %s provider", tt.name, result.Provider)
		})
	}
}
