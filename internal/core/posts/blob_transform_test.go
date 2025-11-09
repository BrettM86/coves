package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransformBlobRefsToURLs(t *testing.T) {
	t.Run("transforms external embed thumb from blob to URL", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com",
					"thumb": map[string]interface{}{
						"$type": "blob",
						"ref": map[string]interface{}{
							"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
						},
						"mimeType": "image/jpeg",
						"size":     52813,
					},
				},
			},
		}

		TransformBlobRefsToURLs(post)

		// Verify embed is still a map
		embedMap, ok := post.Embed.(map[string]interface{})
		require.True(t, ok, "embed should still be a map")

		// Verify external is still a map
		external, ok := embedMap["external"].(map[string]interface{})
		require.True(t, ok, "external should be a map")

		// Verify thumb is now a URL string
		thumbURL, ok := external["thumb"].(string)
		require.True(t, ok, "thumb should be a string URL")
		assert.Equal(t,
			"http://localhost:3001/xrpc/com.atproto.sync.getBlob?did=did:plc:testcommunity&cid=bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			thumbURL)
	})

	t.Run("handles missing thumb gracefully", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com",
					// No thumb field
				},
			},
		}

		// Should not panic
		TransformBlobRefsToURLs(post)

		// Verify external is unchanged
		embedMap := post.Embed.(map[string]interface{})
		external := embedMap["external"].(map[string]interface{})
		_, hasThumb := external["thumb"]
		assert.False(t, hasThumb, "thumb should not be added")
	})

	t.Run("handles already-transformed URL thumb", func(t *testing.T) {
		expectedURL := "http://localhost:3001/xrpc/com.atproto.sync.getBlob?did=did:plc:test&cid=bafytest"
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com",
					"thumb": expectedURL, // Already a URL string
				},
			},
		}

		// Should not error or change the URL
		TransformBlobRefsToURLs(post)

		// Verify thumb is unchanged
		embedMap := post.Embed.(map[string]interface{})
		external := embedMap["external"].(map[string]interface{})
		thumbURL, ok := external["thumb"].(string)
		require.True(t, ok, "thumb should still be a string")
		assert.Equal(t, expectedURL, thumbURL, "thumb URL should be unchanged")
	})

	t.Run("handles missing embed", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: nil,
		}

		// Should not panic
		TransformBlobRefsToURLs(post)

		// Verify embed is still nil
		assert.Nil(t, post.Embed, "embed should remain nil")
	})

	t.Run("handles nil post", func(t *testing.T) {
		// Should not panic
		TransformBlobRefsToURLs(nil)
	})

	t.Run("handles missing community", func(t *testing.T) {
		post := &PostView{
			Community: nil,
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com",
					"thumb": map[string]interface{}{
						"$type": "blob",
						"ref": map[string]interface{}{
							"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
						},
					},
				},
			},
		}

		// Should not panic or transform
		TransformBlobRefsToURLs(post)

		// Verify thumb is unchanged (still a blob)
		embedMap := post.Embed.(map[string]interface{})
		external := embedMap["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok, "thumb should still be a map (blob ref)")
		assert.Equal(t, "blob", thumb["$type"], "blob type should be unchanged")
	})

	t.Run("handles missing PDS URL", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "", // Empty PDS URL
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com",
					"thumb": map[string]interface{}{
						"$type": "blob",
						"ref": map[string]interface{}{
							"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
						},
					},
				},
			},
		}

		// Should not panic or transform
		TransformBlobRefsToURLs(post)

		// Verify thumb is unchanged (still a blob)
		embedMap := post.Embed.(map[string]interface{})
		external := embedMap["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok, "thumb should still be a map (blob ref)")
		assert.Equal(t, "blob", thumb["$type"], "blob type should be unchanged")
	})

	t.Run("handles malformed blob ref gracefully", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://example.com",
					"thumb": map[string]interface{}{
						"$type": "blob",
						"ref":   "invalid-ref-format", // Should be a map with $link
					},
				},
			},
		}

		// Should not panic
		TransformBlobRefsToURLs(post)

		// Verify thumb is unchanged (malformed blob)
		embedMap := post.Embed.(map[string]interface{})
		external := embedMap["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok, "thumb should still be a map")
		assert.Equal(t, "invalid-ref-format", thumb["ref"], "malformed ref should be unchanged")
	})

	t.Run("ignores non-external embed types", func(t *testing.T) {
		post := &PostView{
			Community: &CommunityRef{
				DID:    "did:plc:testcommunity",
				PDSURL: "http://localhost:3001",
			},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.images",
				"images": []interface{}{
					map[string]interface{}{
						"image": map[string]interface{}{
							"$type": "blob",
							"ref": map[string]interface{}{
								"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
							},
						},
					},
				},
			},
		}

		// Should not transform non-external embeds
		TransformBlobRefsToURLs(post)

		// Verify images embed is unchanged
		embedMap := post.Embed.(map[string]interface{})
		images := embedMap["images"].([]interface{})
		imageObj := images[0].(map[string]interface{})
		imageBlob := imageObj["image"].(map[string]interface{})
		assert.Equal(t, "blob", imageBlob["$type"], "image blob should be unchanged")
	})
}

func TestTransformThumbToURL(t *testing.T) {
	t.Run("transforms valid blob ref to URL", func(t *testing.T) {
		external := map[string]interface{}{
			"uri": "https://example.com",
			"thumb": map[string]interface{}{
				"$type": "blob",
				"ref": map[string]interface{}{
					"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
				},
				"mimeType": "image/jpeg",
				"size":     52813,
			},
		}

		transformThumbToURL(external, "did:plc:test", "http://localhost:3001")

		thumbURL, ok := external["thumb"].(string)
		require.True(t, ok, "thumb should be a string URL")
		assert.Equal(t,
			"http://localhost:3001/xrpc/com.atproto.sync.getBlob?did=did:plc:test&cid=bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			thumbURL)
	})

	t.Run("does not transform if thumb is already string", func(t *testing.T) {
		expectedURL := "http://localhost:3001/xrpc/com.atproto.sync.getBlob?did=did:plc:test&cid=bafytest"
		external := map[string]interface{}{
			"uri":   "https://example.com",
			"thumb": expectedURL,
		}

		transformThumbToURL(external, "did:plc:test", "http://localhost:3001")

		thumbURL, ok := external["thumb"].(string)
		require.True(t, ok, "thumb should still be a string")
		assert.Equal(t, expectedURL, thumbURL, "thumb should be unchanged")
	})

	t.Run("does not transform if thumb is missing", func(t *testing.T) {
		external := map[string]interface{}{
			"uri": "https://example.com",
		}

		transformThumbToURL(external, "did:plc:test", "http://localhost:3001")

		_, hasThumb := external["thumb"]
		assert.False(t, hasThumb, "thumb should not be added")
	})

	t.Run("does not transform if CID is empty", func(t *testing.T) {
		external := map[string]interface{}{
			"uri": "https://example.com",
			"thumb": map[string]interface{}{
				"$type": "blob",
				"ref": map[string]interface{}{
					"$link": "", // Empty CID
				},
			},
		}

		transformThumbToURL(external, "did:plc:test", "http://localhost:3001")

		// Verify thumb is unchanged
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok, "thumb should still be a map")
		ref := thumb["ref"].(map[string]interface{})
		assert.Equal(t, "", ref["$link"], "empty CID should be unchanged")
	})
}
