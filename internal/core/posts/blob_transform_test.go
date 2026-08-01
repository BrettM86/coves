package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
)

const (
	embedCommunityDID = "did:plc:testcommunity"
	embedCommunityPDS = "http://localhost:3001" // coves:allow-host-literal: expected-output fixture for a pure string transform; never dialled
	embedBlobCID      = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
)

// withImageProxy enables the process-wide image proxy for one test. The
// projection itself is covered in internal/core/embeds; these cases are about
// what the posts layer contributes — that the community owns the blobs, and
// that a post with nothing to project survives untouched.
func withImageProxy(t *testing.T) {
	t.Helper()
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://img.coves.social",
	})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
}

func testBlobRef() map[string]interface{} {
	return map[string]interface{}{
		"$type":    "blob",
		"ref":      map[string]interface{}{"$link": embedBlobCID},
		"mimeType": "image/jpeg",
		"size":     52813,
	}
}

func externalEmbedPost() *PostView {
	return &PostView{
		Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri":   "https://example.com",
				"thumb": testBlobRef(),
			},
		},
	}
}

func TestTransformBlobRefsToURLs(t *testing.T) {
	t.Run("an external thumb is served from the image proxy under the community DID", func(t *testing.T) {
		withImageProxy(t)

		post := externalEmbedPost()
		TransformBlobRefsToURLs(post)

		embedMap := post.Embed.(map[string]interface{})
		assert.Equal(t, "social.coves.embed.external#view", embedMap["$type"])

		external := embedMap["external"].(map[string]interface{})
		// The AppView signs community posts into the community's repo and
		// uploads their blobs there, so the community DID owns the blob no
		// matter who authored the post.
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedCommunityDID+"/"+embedBlobCID,
			external["thumb"])
	})

	t.Run("image embeds are hydrated, not skipped", func(t *testing.T) {
		withImageProxy(t)

		post := &PostView{
			Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.images",
				"images": []interface{}{
					map[string]interface{}{"image": testBlobRef(), "alt": "a cat"},
				},
			},
		}

		TransformBlobRefsToURLs(post)

		embedMap := post.Embed.(map[string]interface{})
		assert.Equal(t, "social.coves.embed.images#view", embedMap["$type"])

		image := embedMap["images"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/content_preview/plain/"+embedCommunityDID+"/"+embedBlobCID,
			image["thumb"])
		assert.Equal(t,
			"https://img.coves.social/img/content_full/plain/"+embedCommunityDID+"/"+embedBlobCID,
			image["fullsize"])
		assert.Equal(t, "a cat", image["alt"])
	})

	t.Run("no image URL the AppView emits addresses a PDS blob endpoint", func(t *testing.T) {
		withImageProxy(t)

		post := externalEmbedPost()
		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		// The whole point of the proxy: a getBlob URL here would be media
		// served around the CDN that scans it.
		assert.NotContains(t, external["thumb"], "com.atproto.sync.getBlob")
	})

	t.Run("an already-hydrated thumb is left alone", func(t *testing.T) {
		withImageProxy(t)

		hydrated := "https://img.coves.social/img/embed_thumbnail/plain/did:plc:test/bafytest"
		post := &PostView{
			Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com",
					"thumb": hydrated,
				},
			},
		}

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t, hydrated, external["thumb"])
	})

	t.Run("a post with no community is left untouched", func(t *testing.T) {
		withImageProxy(t)

		post := externalEmbedPost()
		post.Community = nil

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok, "with no owning repo there is no URL to build, so the blob must survive")
		assert.Equal(t, "blob", thumb["$type"])
	})

	t.Run("the proxy resolves the DID itself, so a missing PDS URL is not fatal", func(t *testing.T) {
		withImageProxy(t)

		post := externalEmbedPost()
		post.Community.PDSURL = ""

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedCommunityDID+"/"+embedBlobCID,
			external["thumb"],
			"the PDS URL is only needed for the proxy-disabled fallback")
	})

	t.Run("nil inputs do not panic", func(t *testing.T) {
		withImageProxy(t)

		assert.NotPanics(t, func() { TransformBlobRefsToURLs(nil) })

		post := &PostView{
			Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
			Embed:     nil,
		}
		TransformBlobRefsToURLs(post)
		assert.Nil(t, post.Embed)
	})

	t.Run("an embed that is not an object is left untouched", func(t *testing.T) {
		withImageProxy(t)

		post := &PostView{
			Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
			Embed:     "not-an-object",
		}

		TransformBlobRefsToURLs(post)

		assert.Equal(t, "not-an-object", post.Embed)
	})
}

func TestTransformBlobRefsToURLs_ProxyDisabled(t *testing.T) {
	// The self-hosted opt-out (ALLOW_UNPROXIED_MEDIA): URLs address the
	// community's PDS directly.
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: false})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)

	post := externalEmbedPost()
	TransformBlobRefsToURLs(post)

	external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
	assert.Equal(t,
		blobs.HydrateBlobURL(embedCommunityPDS, embedCommunityDID, embedBlobCID),
		external["thumb"])
}
