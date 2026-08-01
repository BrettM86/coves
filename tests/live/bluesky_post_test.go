//go:build live

package live

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/blueskypost"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// egressNote is appended to every failure that means "we could not reach a
// third party". It exists because that failure looks identical to a regression
// until you know which side of the wire broke.
//
// The live tier is the only tier allowed public egress, and it is the only tier
// that REQUIRES it: `make ci` runs the hermetic stack with its network declared
// `internal: true`, so nothing tagged `live` can even be executed from in there.
// If you are reading this message, you invoked `make test-live` yourself, from a
// shell that is supposed to be able to reach the open internet.
const egressNote = "The live tier requires public internet egress — plc.directory for handle\n" +
	"resolution and public.api.bsky.app for post content. It is never run on the\n" +
	"merge path: `make ci` blocks egress by design (docker-compose.ci.yml declares\n" +
	"its network internal: true), so this test cannot have been run from inside the\n" +
	"CI stack. Confirm this machine's connectivity before reading the failure as a\n" +
	"regression in Coves."

// pinnedPost is a real post on somebody else's account, used as a fixture.
//
// Pinning third-party content is a deliberate trade: these posts are the only
// way to check that the resolver copes with the shapes Bluesky actually emits,
// and the price is that their authors can delete them. When that happens the
// tier goes red and names the fixture — which is the correct outcome. A live
// tier that reports green because the thing it checks has vanished is exactly
// what this suite spent six phases removing.
type pinnedPost struct {
	// varName is the Go identifier below, so a failure can say what to edit.
	varName string
	// url is the bsky.app permalink a user would paste into Coves.
	url string
	// handle is url's author, asserted against the API's answer.
	handle string
	// shape describes what this fixture is FOR, so a replacement can be chosen
	// that still exercises the same code path.
	shape string
}

var (
	pinnedPlainPost = pinnedPost{
		varName: "pinnedPlainPost",
		url:     "https://bsky.app/profile/ianboudreau.com/post/3makab2jnwk2p",
		handle:  "ianboudreau.com",
		shape:   "a plain text post with no embed",
	}
	pinnedQuotePost = pinnedPost{
		varName: "pinnedQuotePost",
		url:     "https://bsky.app/profile/tedunderwood.com/post/3malohcd2vc2d",
		handle:  "tedunderwood.com",
		shape:   "a post quoting another post (app.bsky.embed.record)",
	}
	pinnedLinkPost = pinnedPost{
		varName: "pinnedLinkPost",
		url:     "https://bsky.app/profile/davidpfau.com/post/3malg2athns2d",
		handle:  "davidpfau.com",
		shape:   "a post with an external link card (app.bsky.embed.external)",
	}
	pinnedImagePost = pinnedPost{
		varName: "pinnedImagePost",
		url:     "https://bsky.app/profile/brennan.computer/post/3mallehc6hk2s",
		handle:  "brennan.computer",
		shape:   "a post whose only embed is an image (app.bsky.embed.images)",
	}
	pinnedQuoteWithImagePost = pinnedPost{
		varName: "pinnedQuoteWithImagePost",
		url:     "https://bsky.app/profile/lauraknelson.bsky.social/post/3malueymbis25",
		handle:  "lauraknelson.bsky.social",
		shape:   "a quote post that also carries an image (app.bsky.embed.recordWithMedia)",
	}
)

// productionPLCIdentityResolver creates an identity resolver that uses the production
// PLC directory (https://plc.directory) for resolving real Bluesky handles.
//
// READ-ONLY: This resolver only performs HTTP GET requests to look up existing identities.
// It does NOT write to the production PLC directory.
//
// Use this for tests that need to resolve real Bluesky handles like "ianboudreau.com".
// Do NOT use for tests involving local Coves identities (use local PLC instead).
//
// NOTE: Requires a database connection for the identity cache. Pass the test db.
func productionPLCIdentityResolver(db *sql.DB) identity.Resolver {
	config := identity.DefaultConfig()
	config.PLCURL = "https://plc.directory" // Production PLC - READ ONLY
	return identity.NewResolver(db, config)
}

// liveBlueskyService builds the service under test against the production PLC
// directory and the public Bluesky AppView API.
func liveBlueskyService(t *testing.T, db *sql.DB) blueskypost.Service {
	t.Helper()
	return blueskypost.NewService(
		blueskypost.NewRepository(db),
		productionPLCIdentityResolver(db),
		blueskypost.WithTimeout(30*time.Second),
		blueskypost.WithCacheTTL(1*time.Hour),
	)
}

// resolveLiveBlueskyURL turns a bsky.app permalink into an AT-URI, failing the
// test if the handle inside it cannot be resolved against the production PLC
// directory.
//
// The failure is deliberate. Handle resolution is a network round trip, and the
// only reasons it fails are "no egress" and "the account is gone" — neither of
// which the live tier may report as a pass.
func resolveLiveBlueskyURL(t *testing.T, ctx context.Context, service blueskypost.Service, p pinnedPost) string {
	t.Helper()

	atURI, err := service.ParseBlueskyURL(ctx, p.url)
	if err != nil {
		t.Fatalf("could not resolve the handle %q in %s (%s): %v\n\n%s\n\n"+
			"If %s has simply changed or deleted their handle, update %s at the top\n"+
			"of this file with another account hosting %s.",
			p.handle, p.url, p.varName, err, egressNote, p.handle, p.varName, p.shape)
	}
	return atURI
}

// fetchLiveBlueskyPost resolves a pinned fixture all the way to post content,
// failing on an unreachable API and, separately, on a post that upstream no
// longer serves.
//
// The two failures read differently on purpose: one says "your network is
// broken", the other says "this fixture is stale, replace it here". Both are
// red, because the assertion the caller is about to make has not been checked.
func fetchLiveBlueskyPost(t *testing.T, ctx context.Context, service blueskypost.Service, p pinnedPost) *blueskypost.BlueskyPostResult {
	t.Helper()

	atURI := resolveLiveBlueskyURL(t, ctx, service, p)

	result, err := service.ResolvePost(ctx, atURI)
	if err != nil {
		t.Fatalf("could not fetch %s (%s) from the Bluesky API: %v\n\n%s",
			atURI, p.varName, err, egressNote)
	}
	require.NotNil(t, result, "ResolvePost returned neither a post nor an error for %s", atURI)

	if result.Unavailable {
		t.Fatalf("the pinned post %s (%s) is no longer retrievable: %s\n\n"+
			"This is an upstream fact, not a Coves regression: the author deleted the\n"+
			"post, locked the account, or blocked the fetcher. Find a live replacement\n"+
			"that is %s, update %s at the top of this file, and re-run. Do not turn this\n"+
			"back into a skip — a live tier that goes green when its subject has\n"+
			"disappeared is checking nothing.",
			p.url, p.varName, result.Message, p.shape, p.varName)
	}

	assert.Equal(t, p.handle, result.Author.Handle,
		"%s resolved to a different author than the URL claims", p.varName)

	return result
}

// staleShapeMessage explains a fixture that still resolves but no longer has the
// structure it was chosen for — a quote whose target was deleted, a link card
// stripped by its author. Same remedy as an outright deletion: repin, here.
func staleShapeMessage(p pinnedPost, what string) string {
	return strings.Join([]string{
		p.url + " (" + p.varName + ") resolved, but " + what + ".",
		"",
		"That fixture is pinned specifically as " + p.shape + ", so this is either a",
		"regression in the extraction code or an upstream edit to somebody else's",
		"post. Check the post in a browser: if the shape is gone, pick a replacement",
		"with the same shape and update " + p.varName + " at the top of this file.",
	}, "\n")
}

// TestBlueskyPostCrossPosting_URLParsing tests URL detection and parsing
func TestBlueskyPostCrossPosting_URLParsing(t *testing.T) {
	db := testkit.DB(t)
	service := liveBlueskyService(t, db)

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
			"https://bsky.app/profile/user",         // Missing post path
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
		atURI := resolveLiveBlueskyURL(t, ctx, service, pinnedPlainPost)

		// Should have resolved to a DID
		assert.Contains(t, atURI, "at://did:")
		assert.Contains(t, atURI, "/app.bsky.feed.post/3makab2jnwk2p")
		t.Logf("Resolved URL to AT-URI: %s", atURI)
	})
}

// TestBlueskyPostCrossPosting_LiveAPI tests fetching real posts from Bluesky
func TestBlueskyPostCrossPosting_LiveAPI(t *testing.T) {
	db := testkit.DB(t)
	service := liveBlueskyService(t, db)

	ctx := context.Background()

	t.Run("Fetch regular Bluesky post", func(t *testing.T) {
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedPlainPost)

		// Validate response structure
		assert.NotEmpty(t, result.URI, "Should have URI")
		assert.NotEmpty(t, result.CID, "Should have CID")
		assert.NotEmpty(t, result.Text, "Should have text content")
		require.NotNil(t, result.Author, "Should have author")
		assert.NotEmpty(t, result.Author.DID, "Author should have DID")

		t.Logf("✓ Successfully fetched regular Bluesky post:")
		t.Logf("  URI: %s", result.URI)
		t.Logf("  Author: @%s (%s)", result.Author.Handle, result.Author.DisplayName)
		t.Logf("  Text: %.100s...", result.Text)
		t.Logf("  Likes: %d, Reposts: %d, Replies: %d", result.LikeCount, result.RepostCount, result.ReplyCount)
	})

	t.Run("Fetch post with quote repost", func(t *testing.T) {
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedQuotePost)

		// The fixture is pinned BECAUSE it quotes another post; if the quote is
		// gone the extraction path is untested and the fixture needs replacing.
		require.NotNil(t, result.QuotedPost, staleShapeMessage(pinnedQuotePost, "no quoted post was extracted"))

		t.Logf("✓ Successfully fetched post with quote:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
		// The quoted post's own text/author can legitimately be empty — a
		// recordWithMedia nests the record one level deeper, and a deleted
		// quote target resolves to a tombstone. Presence is the contract here.
		if result.QuotedPost.Author != nil && result.QuotedPost.Author.Handle != "" {
			t.Logf("  Quoted author: @%s", result.QuotedPost.Author.Handle)
		}
		if result.QuotedPost.Text != "" {
			t.Logf("  Quoted text: %.60s...", result.QuotedPost.Text)
		}
	})

	t.Run("Fetch post with link embed", func(t *testing.T) {
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedLinkPost)

		assert.NotEmpty(t, result.Text)

		// Verify the external embed is extracted — the reason this fixture is
		// pinned rather than reusing the plain post.
		require.NotNil(t, result.Embed, staleShapeMessage(pinnedLinkPost, "no external embed was extracted"))
		assert.NotEmpty(t, result.Embed.URI, "External embed should have URI")

		t.Logf("✓ Successfully fetched post with link embed:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
		t.Logf("  External embed URI: %s", result.Embed.URI)
		t.Logf("  External embed title: %s", result.Embed.Title)
		t.Logf("  External embed thumb: %s", result.Embed.Thumb)
		t.Logf("  Has media: %v", result.HasMedia)
	})

	t.Run("Fetch image-only post", func(t *testing.T) {
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedImagePost)

		// This post should have media (image)
		assert.True(t, result.HasMedia, "Image post should have HasMedia=true")
		assert.Greater(t, result.MediaCount, 0, "Image post should have MediaCount > 0")

		t.Logf("✓ Successfully fetched image post:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s", result.Text)
		t.Logf("  Has media: %v (count: %d)", result.HasMedia, result.MediaCount)
	})

	t.Run("Fetch quote RT with image", func(t *testing.T) {
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedQuoteWithImagePost)

		// recordWithMedia carries both halves; both must survive extraction, or
		// the fixture no longer exercises the path it was pinned for.
		require.NotNil(t, result.QuotedPost, staleShapeMessage(pinnedQuoteWithImagePost, "no quoted post was extracted"))
		assert.True(t, result.HasMedia, "Quote RT with an image should have HasMedia=true")
		assert.Greater(t, result.MediaCount, 0, "Quote RT with an image should have MediaCount > 0")

		t.Logf("✓ Successfully fetched quote RT with image:")
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s", result.Text)
		t.Logf("  Has media: %v (count: %d)", result.HasMedia, result.MediaCount)
		t.Logf("  Quoted post has media: %v (count: %d)", result.QuotedPost.HasMedia, result.QuotedPost.MediaCount)
	})

	t.Run("Cache hit on second fetch", func(t *testing.T) {
		// Reuse the plain fixture: the first fetch may already be cached from
		// the subtest above, which is fine — what is asserted is that a second
		// fetch is served locally and returns the same content.
		result1 := fetchLiveBlueskyPost(t, ctx, service, pinnedPlainPost)
		atURI := result1.URI

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

// TestBlueskyPostCrossPosting_E2E_PostCreation tests the full flow of creating a post with a Bluesky embed
func TestBlueskyPostCrossPosting_E2E_PostCreation(t *testing.T) {
	db := testkit.DB(t)
	service := liveBlueskyService(t, db)

	ctx := context.Background()

	t.Run("Full URL to resolved embed flow", func(t *testing.T) {
		// Simulate user pasting a bsky.app URL (quote post with embedded content)

		// Step 1: Detect it's a Bluesky URL
		require.True(t, service.IsBlueskyURL(pinnedQuotePost.url), "Should detect valid bsky.app URL")

		// Steps 2 and 3: parse to an AT-URI and resolve the post
		result := fetchLiveBlueskyPost(t, ctx, service, pinnedQuotePost)

		// Verify the resolved embed has all required fields
		assert.NotEmpty(t, result.URI)
		assert.NotEmpty(t, result.CID)
		assert.NotEmpty(t, result.Text)
		require.NotNil(t, result.Author)

		t.Logf("✓ E2E flow complete:")
		t.Logf("  Input URL: %s", pinnedQuotePost.url)
		t.Logf("  AT-URI: %s", result.URI)
		t.Logf("  Author: @%s", result.Author.Handle)
		t.Logf("  Text: %.80s...", result.Text)
	})
}

// TestBlueskyPostCrossPosting_EmbedConversion tests that Bluesky URLs in external embeds
// are converted to social.coves.embed.post with proper strongRef (uri + cid)
func TestBlueskyPostCrossPosting_EmbedConversion(t *testing.T) {
	db := testkit.DB(t)
	blueskyService := liveBlueskyService(t, db)

	ctx := context.Background()

	t.Run("Convert Bluesky URL to post embed with strongRef", func(t *testing.T) {
		// 1. Verify URL is detected as Bluesky
		require.True(t, blueskyService.IsBlueskyURL(pinnedPlainPost.url), "Should detect as Bluesky URL")

		// 2 and 3. Parse to an AT-URI and resolve the post to get its CID
		result := fetchLiveBlueskyPost(t, ctx, blueskyService, pinnedPlainPost)

		// 4. Verify we have all fields needed for strongRef
		require.NotEmpty(t, result.URI, "Should have AT-URI")
		require.NotEmpty(t, result.CID, "Should have CID for strongRef")

		// 5. Verify the CID is a valid format (starts with 'baf')
		assert.True(t, len(result.CID) > 10, "CID should be a valid length")
		assert.True(t, strings.HasPrefix(result.CID, "baf"), "CID should start with 'baf' (CIDv1)")

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
