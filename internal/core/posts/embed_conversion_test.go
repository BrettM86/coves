package posts

import (
	"context"
	"errors"
	"testing"

	"Coves/internal/core/blueskypost"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBlueskyService implements blueskypost.Service for testing
type mockBlueskyService struct {
	isBlueskyURLResult bool
	parseURLResult     string
	parseURLError      error
	resolvePostResult  *blueskypost.BlueskyPostResult
	resolvePostError   error
}

func (m *mockBlueskyService) IsBlueskyURL(url string) bool {
	return m.isBlueskyURLResult
}

func (m *mockBlueskyService) ParseBlueskyURL(_ context.Context, _ string) (string, error) {
	return m.parseURLResult, m.parseURLError
}

func (m *mockBlueskyService) ResolvePost(_ context.Context, _ string) (*blueskypost.BlueskyPostResult, error) {
	return m.resolvePostResult, m.resolvePostError
}

func TestTryConvertBlueskyURLToPostEmbed(t *testing.T) {
	ctx := context.Background()

	t.Run("returns false when blueskyService is nil", func(t *testing.T) {
		svc := &postService{
			blueskyService: nil, // nil service
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when blueskyService is nil")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when URL is empty", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "", // empty URL
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when URL is empty")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when URI field is missing", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			// no "uri" field
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when uri field is missing")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when URI is not a string type", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": 12345, // int instead of string
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when uri is not a string")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when URL is not Bluesky", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: false, // not a Bluesky URL
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://twitter.com/user/status/123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when URL is not Bluesky")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when URL parsing fails", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLError:      errors.New("handle resolution failed"),
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/nonexistent.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when URL parsing fails")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when post resolution fails with error", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:test/app.bsky.feed.post/abc123",
			resolvePostError:   errors.New("API timeout"),
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when post resolution fails")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when post is unavailable", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:deleted/app.bsky.feed.post/deleted123",
			resolvePostResult: &blueskypost.BlueskyPostResult{
				Unavailable: true,
				Message:     "This post has been deleted",
			},
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/deleted.bsky.social/post/deleted123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false for unavailable posts - keep as external embed")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when ResolvePost returns nil result", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:test/app.bsky.feed.post/abc123",
			resolvePostResult:  nil, // nil result
			resolvePostError:   nil, // no error
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when result is nil")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false with circuit breaker error", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:test/app.bsky.feed.post/abc123",
			resolvePostError:   blueskypost.ErrCircuitOpen,
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when circuit breaker is open")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when resolved post has empty URI", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:test/app.bsky.feed.post/abc123",
			resolvePostResult: &blueskypost.BlueskyPostResult{
				URI: "", // empty URI
				CID: "bafytest123",
			},
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when URI is empty")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("returns false when resolved post has empty CID", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:test/app.bsky.feed.post/abc123",
			resolvePostResult: &blueskypost.BlueskyPostResult{
				URI: "at://did:plc:test/app.bsky.feed.post/abc123",
				CID: "", // empty CID
			},
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/abc123",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.False(t, result, "Should return false when CID is empty")
		assert.Nil(t, postRecord.Embed, "Should not modify embed")
	})

	t.Run("successfully converts valid Bluesky URL to post embed", func(t *testing.T) {
		mockSvc := &mockBlueskyService{
			isBlueskyURLResult: true,
			parseURLResult:     "at://did:plc:abcdef/app.bsky.feed.post/xyz789",
			resolvePostResult: &blueskypost.BlueskyPostResult{
				URI:  "at://did:plc:abcdef/app.bsky.feed.post/xyz789",
				CID:  "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
				Text: "Hello from Bluesky!",
				Author: &blueskypost.Author{
					DID:    "did:plc:abcdef",
					Handle: "test.bsky.social",
				},
			},
		}
		svc := &postService{
			blueskyService: mockSvc,
		}

		external := map[string]interface{}{
			"uri": "https://bsky.app/profile/test.bsky.social/post/xyz789",
		}
		postRecord := &PostRecord{}

		result := svc.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord)

		assert.True(t, result, "Should return true for successful conversion")
		require.NotNil(t, postRecord.Embed, "Should set embed")

		// Verify embed structure (Embed is already map[string]interface{})
		assert.Equal(t, "social.coves.embed.post", postRecord.Embed["$type"])

		postRef, ok := postRecord.Embed["post"].(map[string]interface{})
		require.True(t, ok, "post should be a map")

		assert.Equal(t, "at://did:plc:abcdef/app.bsky.feed.post/xyz789", postRef["uri"])
		assert.Equal(t, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", postRef["cid"])
	})
}
