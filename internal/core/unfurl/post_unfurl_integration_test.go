//go:build integration

package unfurl_test

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/unfurl"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostUnfurl_UnsupportedURL tests that posts with unsupported URLs still succeed
func TestPostUnfurl_UnsupportedURL(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()

	// Setup services
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	postRepo := postgres.NewPostRepository(db)

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, testkit.Endpoints().PDS.BaseURL, nil, "")

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		testkit.Endpoints().PDS.BaseURL,
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
		nil,
		nil,
		communities.PrivateHostOptions(true)...,
	)

	// Create post service WITHOUT unfurl service
	postService := posts.NewPostService(
		postRepo,
		communityService,
		nil, // aggregatorService
		nil, // blobService
		nil, // unfurlService - intentionally nil to test graceful handling
		nil, // blueskyService
		testkit.Endpoints().PDS.BaseURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
	)

	// Create test user
	testUserDID := fixtures.DID("unsupporteduser")
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: "unsupporteduser.test",
		PDSURL: testkit.Endpoints().PDS.BaseURL,
	})
	require.NoError(t, err)

	// Create test community
	testCommunity := &communities.Community{
		DID:             fixtures.DID("unsupportedcommunity"),
		Handle:          "c-unsupportedcommunity.test.coves.social",
		Name:            "unsupportedcommunity",
		DisplayName:     "Unsupported URL Test",
		Visibility:      "public",
		CreatedByDID:    testUserDID,
		HostedByDID:     "did:web:test.coves.social",
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err)

	// Create post with unsupported URL
	unsupportedURL := "https://example.com/article/123"
	title := "Unsupported URL Test"
	content := "Testing unsupported domain"

	createReq := posts.CreatePostRequest{
		Community: testCommunity.DID,
		Title:     &title,
		Content:   &content,
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri": unsupportedURL,
			},
		},
		AuthorDID: testUserDID,
	}

	authCtx := middleware.SetTestUserDID(ctx, testUserDID)
	_, err = postService.CreatePost(authCtx, nil, createReq)

	// Should still fail at the author-repo write (expected): the service is
	// wired with no author-repo factory, so it has nothing to sign the record
	// with. Before task 6 the same role was played by the community's unusable
	// token, which the write path no longer touches.
	require.Error(t, err, "Expected error opening the author's repository")
	assert.ErrorIs(t, err, posts.ErrNoAuthorCredentials)

	// The point is that it didn't fail earlier due to unsupported URL
	t.Log("✓ Post creation with unsupported URL proceeded to the author-repo write stage")
}

// TestPostUnfurl_MissingEmbedType tests posts without external embed type don't trigger unfurling
func TestPostUnfurl_MissingEmbedType(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()

	// Setup
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	postRepo := postgres.NewPostRepository(db)
	unfurlRepo := unfurl.NewRepository(db)

	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, testkit.Endpoints().PDS.BaseURL, nil, "")

	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
	)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		testkit.Endpoints().PDS.BaseURL,
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
		nil,
		nil,
		communities.PrivateHostOptions(true)...,
	)

	postService := posts.NewPostService(
		postRepo,
		communityService,
		nil,
		nil,
		unfurlService,
		nil, // blueskyService
		testkit.Endpoints().PDS.BaseURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
	)

	// Create test user and community
	testUserDID := fixtures.DID("noembeduser")
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testUserDID,
		Handle: "noembeduser.test",
		PDSURL: testkit.Endpoints().PDS.BaseURL,
	})
	require.NoError(t, err)

	testCommunity := &communities.Community{
		DID:             fixtures.DID("noembedcommunity"),
		Handle:          "c-noembedcommunity.test.coves.social",
		Name:            "noembedcommunity",
		DisplayName:     "No Embed Test",
		Visibility:      "public",
		CreatedByDID:    testUserDID,
		HostedByDID:     "did:web:test.coves.social",
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err = communityRepo.Create(ctx, testCommunity)
	require.NoError(t, err)

	// Test 1: Post with no embed
	t.Run("Post with no embed", func(t *testing.T) {
		title := "No Embed Post"
		content := "Just text content"

		createReq := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, nil, createReq)

		// Should fail at the author-repo write (expected); see above.
		require.Error(t, err)
		assert.ErrorIs(t, err, posts.ErrNoAuthorCredentials)

		t.Log("✓ Post without embed succeeded (no unfurl attempted)")
	})

	// Test 2: Post with images embed (different type)
	t.Run("Post with images embed", func(t *testing.T) {
		title := "Images Post"
		content := "Post with images"

		createReq := posts.CreatePostRequest{
			Community: testCommunity.DID,
			Title:     &title,
			Content:   &content,
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.images",
				"images": []interface{}{
					map[string]interface{}{
						"image": map[string]interface{}{
							"ref": "bafytest123",
						},
						"alt": "Test image",
					},
				},
			},
			AuthorDID: testUserDID,
		}

		authCtx := middleware.SetTestUserDID(ctx, testUserDID)
		_, err := postService.CreatePost(authCtx, nil, createReq)

		// Should fail at the author-repo write (expected); see above.
		require.Error(t, err)
		assert.ErrorIs(t, err, posts.ErrNoAuthorCredentials)

		t.Log("✓ Post with images embed succeeded (no unfurl attempted)")
	})
}

// TestPostUnfurl_KagiKiteExcluded verifies that kite.kagi.com is deliberately
// NOT unfurled. Its server-rendered <title>/og:* tags are an identical per-path
// default for every URL (see commit f8efe46 and internal/core/unfurl/providers.go),
// so unfurling would attach the same misleading metadata to every Kite story.
// The kagi-news trusted aggregator already supplies authoritative metadata from
// the Kagi JSON feed, so the unfurl path for Kite URLs is intentionally disabled.
func TestPostUnfurl_KagiKiteExcluded(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()

	// Setup unfurl repository and service
	unfurlRepo := unfurl.NewRepository(db)
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithCacheTTL(24*time.Hour),
	)

	// Various kite.kagi.com URL shapes must all be reported as unsupported.
	kiteURLs := []string{
		"https://kite.kagi.com/",
		"https://kite.kagi.com/abc/science/9",
		"https://kite.kagi.com/search?q=test",
	}

	for _, kiteURL := range kiteURLs {
		assert.False(t, unfurlService.IsSupported(kiteURL),
			"kite.kagi.com is deliberately excluded and must not be supported: %s", kiteURL)

		// UnfurlURL must refuse the URL rather than fetch misleading metadata.
		result, err := unfurlService.UnfurlURL(ctx, kiteURL)
		require.Error(t, err, "Expected an error unfurling excluded URL %s", kiteURL)
		assert.Nil(t, result, "Expected no unfurl result for excluded URL %s", kiteURL)
		assert.Contains(t, err.Error(), "unsupported URL",
			"Expected an 'unsupported URL' error for %s", kiteURL)
	}

	t.Logf("✓ kite.kagi.com correctly excluded from unfurling")
}

// TestPostUnfurl_E2E_WithJetstream tests the full unfurl flow with Jetstream consumer
// This simulates: Create post → unfurl → write to PDS → Jetstream event → index in AppView
func TestPostUnfurl_E2E_WithJetstream(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	postRepo := postgres.NewPostRepository(db)
	unfurlRepo := unfurl.NewRepository(db)

	// Setup services
	identityConfig := identity.DefaultConfig()
	identityResolver := identity.NewResolver(db, identityConfig)
	userService := users.NewUserService(userRepo, identityResolver, testkit.Endpoints().PDS.BaseURL, nil, "")

	// THE HATCH IS OPEN BECAUSE THE FIXTURE IS ON LOOPBACK, and for no other
	// reason. unfurlTarget below is an httptest server, so its address is
	// exactly the class the SSRF guard refuses — the same reason
	// blobs/fetch_guard_test.go builds its service with WithPrivateHostsAllowed.
	//
	// This is the honest repair rather than the convenient one. The alternative —
	// handing this test a client that skips the guarded transport — would keep it
	// green while removing the thing it exercises: the request below would no
	// longer travel the path production travels, and a regression in that path
	// would leave this test passing. The hatch changes ONE decision, the address
	// classification, and leaves the vetted dial, the byte cap and the timeout
	// exactly where production has them.
	unfurlService := unfurl.NewService(unfurlRepo,
		unfurl.WithTimeout(30*time.Second),
		unfurl.WithPrivateHostsAllowed(),
	)

	// The URL this test unfurls is served by an httptest server rather than by a
	// real site. The unfurl path it exercises is the genuine one — fetch, parse
	// OpenGraph tags, cache — but the metadata is ours, so the assertions below
	// hold on every run. Previously this fetched streamable.com and fell back to
	// hand-built mock data whenever that failed, which under the CI stack's
	// egress block was every run: the test verified its own fixture.
	unfurlTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head>
<meta property="og:title" content="E2E Test Video">
<meta property="og:description" content="Test video for E2E unfurl">
<meta property="og:image" content="http://localhost:9/thumb.jpg">
</head><body></body></html>`)
	}))
	defer unfurlTarget.Close()
	targetURL := unfurlTarget.URL + "/e2etest"

	// Create test data
	testUserDID := fixtures.DID("e2eunfurluser")
	author := fixtures.User(t, db, "e2eunfurluser.test", testUserDID)

	testCommunityDID := fixtures.DID("e2eunfurlcommunity")
	community := &communities.Community{
		DID:             testCommunityDID,
		Handle:          "c-e2eunfurlcommunity.test.coves.social",
		Name:            "e2eunfurlcommunity",
		DisplayName:     "E2E Unfurl Test",
		OwnerDID:        testCommunityDID,
		CreatedByDID:    author.DID,
		HostedByDID:     "did:web:coves.test",
		Visibility:      "public",
		ModerationType:  "moderator",
		RecordURI:       fmt.Sprintf("at://%s/social.coves.community.profile/self", testCommunityDID),
		RecordCID:       "fakecid123",
		PDSAccessToken:  "fake_token",
		PDSRefreshToken: "fake_refresh",
	}
	_, err := communityRepo.Create(ctx, community)
	require.NoError(t, err)

	rkey := testkit.TID()

	// Trigger the unfurl, as the post service would. No fallback: if this fails
	// the test fails, because everything below asserts on what it produced.
	unfurlResult, err := unfurlService.UnfurlURL(ctx, targetURL)
	require.NoError(t, err, "Unfurl of the local fixture server must succeed")
	require.NotNil(t, unfurlResult)
	require.Equal(t, "E2E Test Video", unfurlResult.Title, "Title should come from the served og:title")
	require.Equal(t, "opengraph", unfurlResult.Provider)

	// The unfurl is cached, so the post service would not refetch it.
	cached, err := unfurlRepo.Get(ctx, targetURL)
	require.NoError(t, err, "Unfurl result should have been cached")
	require.NotNil(t, cached, "Unfurl result should have been cached")

	// Build the embed that would be written to PDS (with unfurl enhancement)
	enhancedEmbed := map[string]interface{}{
		"$type": "social.coves.embed.external",
		"external": map[string]interface{}{
			"uri":          targetURL,
			"title":        unfurlResult.Title,
			"description":  unfurlResult.Description,
			"embedType":    unfurlResult.Type,
			"provider":     unfurlResult.Provider,
			"domain":       unfurlResult.Domain,
			"thumbnailUrl": unfurlResult.ThumbnailURL,
		},
	}

	// Simulate Jetstream event with enhanced embed
	jetstreamEvent := jetstream.JetstreamEvent{
		Did:  author.DID,
		Kind: "commit",
		Commit: &jetstream.CommitEvent{
			Operation:  "create",
			Collection: posts.PostV2Collection,
			RKey:       rkey,
			CID:        "bafy2bzaceunfurle2e",
			Record: map[string]interface{}{
				"$type":     posts.PostV2Collection,
				"community": community.DID,
				"title":     "E2E Unfurl Test Post",
				"content":   "Testing unfurl E2E flow",
				"embed":     enhancedEmbed,
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	// Process through Jetstream consumer
	consumer := jetstream.NewPostEventConsumer(
		postRepo, communityRepo, userService, db,
		jetstream.WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
	err = consumer.HandleEvent(ctx, &jetstreamEvent)
	require.NoError(t, err, "Failed to process Jetstream event")

	// Verify post was indexed with unfurl metadata
	uri := fmt.Sprintf("at://%s/%s/%s", author.DID, posts.PostV2Collection, rkey)
	indexedPost, err := postRepo.GetRawIndexedRow(ctx, uri)
	require.NoError(t, err, "Post should be indexed")

	// Verify embed was stored
	require.NotNil(t, indexedPost.Embed, "Post should have embed")

	// Parse embed JSON
	var embedData map[string]interface{}
	err = json.Unmarshal([]byte(*indexedPost.Embed), &embedData)
	require.NoError(t, err, "Embed should be valid JSON")

	// Verify unfurl enhancement fields are present
	external, ok := embedData["external"].(map[string]interface{})
	require.True(t, ok, "Embed should have external field")

	assert.Equal(t, targetURL, external["uri"], "URI should match")
	assert.Equal(t, unfurlResult.Title, external["title"], "Title should match unfurl")
	assert.Equal(t, unfurlResult.Type, external["embedType"], "EmbedType should be set")
	assert.Equal(t, unfurlResult.Provider, external["provider"], "Provider should be set")
	assert.Equal(t, unfurlResult.Domain, external["domain"], "Domain should be set")

	t.Logf("✓ E2E unfurl test complete:")
	t.Logf("  Post URI: %s", uri)
	t.Logf("  Unfurl Title: %s", unfurlResult.Title)
	t.Logf("  Unfurl Type: %s", unfurlResult.Type)
	t.Logf("  Unfurl Provider: %s", unfurlResult.Provider)
}
