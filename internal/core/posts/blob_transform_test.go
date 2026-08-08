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

	embedAuthorDID = "did:plc:testauthor"
	embedAuthorPDS = "http://localhost:3002" // coves:allow-host-literal: expected-output fixture for a pure string transform; never dialled

	// The two record URIs the transform has to tell apart. Their COLLECTION is
	// the whole signal: it says which repository the record lives in, and
	// therefore which repository holds its blobs.
	embedLegacyURI = "at://" + embedCommunityDID + "/social.coves.community.post/3lrc77gmww4nc"
	embedPostV2URI = "at://" + embedAuthorDID + "/" + PostV2Collection + "/3lrc77gmww4nc"
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

// externalEmbedPost is a DEPRECATED community-repo post: its record lives in
// the community's repository, so the community owns its blobs.
//
// The URI is set explicitly rather than left empty, which is how these fixtures
// used to read. An owner chosen per record is chosen FROM the URI, so a fixture
// without one would be asserting the transform's fallback while claiming to
// asserting the community case — and would keep passing if the selection broke
// in exactly the direction that matters.
func externalEmbedPost() *PostView {
	return &PostView{
		URI:       embedLegacyURI,
		Community: &CommunityRef{DID: embedCommunityDID, PDSURL: embedCommunityPDS},
		Author:    &AuthorView{DID: embedAuthorDID, PDSURL: embedAuthorPDS},
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri":   "https://example.com",
				"thumb": testBlobRef(),
			},
		},
	}
}

// authorOwnedEmbedPost is the same post as a postv2 record: it lives in the
// AUTHOR's repository, so the author owns its blobs.
func authorOwnedEmbedPost() *PostView {
	post := externalEmbedPost()
	post.URI = embedPostV2URI
	return post
}

func TestTransformBlobRefsToURLs(t *testing.T) {
	t.Run("an external thumb is served from the image proxy under the community DID", func(t *testing.T) {
		withImageProxy(t)

		post := externalEmbedPost()
		TransformBlobRefsToURLs(post)

		embedMap := post.Embed.(map[string]interface{})
		assert.Equal(t, "social.coves.embed.external#view", embedMap["$type"])

		external := embedMap["external"].(map[string]interface{})
		// A DEPRECATED community.post record: the AppView signed it into the
		// community's repo and uploaded its blobs there, so the community DID
		// owns the blob no matter who authored the post. Every such record
		// standing in production today still resolves this way, and will until
		// task 8 re-materializes them — which is why this case survives the
		// flip rather than being replaced by it.
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedCommunityDID+"/"+embedBlobCID,
			external["thumb"])
	})

	t.Run("image embeds are hydrated, not skipped", func(t *testing.T) {
		withImageProxy(t)

		post := &PostView{
			URI:       embedLegacyURI,
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

// Which repository a post's blobs live in, now that the answer depends on the
// record (docs/PRD_AUTHOR_OWNED_POSTS.md §3.1, §4.2).
//
// Before the write-path flip there was one answer for every post: the AppView
// signed the record into the COMMUNITY's repo and uploaded its blobs there, so
// the community DID owned the media whoever had written it. A postv2 record
// lives in its AUTHOR's repo and its blobs are uploaded under the author's own
// session, so the author DID owns them — and the two kinds of record are indexed
// into ONE table and served by ONE read path.
//
// So the owner has to be chosen PER RECORD, and the only thing that says which
// is the URI's collection. Getting it wrong is not a crash: it builds a
// perfectly well-formed image URL naming a repository that does not hold the
// blob, which renders as a broken image for every reader and looks from the
// server side like everything worked.
//
// The transform is pure, so this is where the choice belongs. What a pure test
// CANNOT prove is that AuthorView.PDSURL is populated at all on the real read
// path — the repository has always selected the column and dropped it — and that
// is pinned at T1 in service_blob_test.go.
func TestTransformBlobRefsToURLs_ChoosesTheOwningRepoPerRecord(t *testing.T) {
	t.Run("a postv2 record's blobs are served under the AUTHOR's DID", func(t *testing.T) {
		withImageProxy(t)

		post := authorOwnedEmbedPost()
		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedAuthorDID+"/"+embedBlobCID,
			external["thumb"],
			"a postv2 post's media was uploaded to the author's PDS under the author's session, so a "+
				"URL built under the community DID names a repo that has never held this blob")
	})

	t.Run("the two collections resolve to different repos for the same blob", func(t *testing.T) {
		withImageProxy(t)

		legacy := externalEmbedPost()
		authored := authorOwnedEmbedPost()
		TransformBlobRefsToURLs(legacy)
		TransformBlobRefsToURLs(authored)

		legacyThumb := legacy.Embed.(map[string]interface{})["external"].(map[string]interface{})["thumb"]
		authoredThumb := authored.Embed.(map[string]interface{})["external"].(map[string]interface{})["thumb"]

		// Asserted as a PAIR because the failure this catches is a transform
		// that reads the URI but answers the same owner either way — which each
		// case above would still catch, but only if both are kept. Stating the
		// difference directly means the day someone "simplifies" the selection
		// back to a constant, this fails with a message that says what was lost.
		assert.NotEqual(t, legacyThumb, authoredThumb,
			"the same blob CID under the two collections must resolve to different repositories: "+
				"one is in the community's repo and the other is in the author's")
	})

	t.Run("proxy disabled, a postv2 record addresses the AUTHOR's PDS", func(t *testing.T) {
		// The self-hosted opt-out builds a direct getBlob URL, which needs the
		// HOST as well as the DID — so this is the case where a wrong owner
		// stops being a wrong path segment and becomes a request to the wrong
		// server entirely.
		blobs.ResetImageURLConfigForTesting()
		blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: false})
		t.Cleanup(blobs.ResetImageURLConfigForTesting)

		post := authorOwnedEmbedPost()
		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t,
			blobs.HydrateBlobURL(embedAuthorPDS, embedAuthorDID, embedBlobCID),
			external["thumb"])
	})

	t.Run("a postv2 record with no author is left unprojected", func(t *testing.T) {
		withImageProxy(t)

		// FAIL CLOSED. Falling back to the community here would be the most
		// tempting repair and the worst one: it produces a confident URL under
		// a DID that has never held the blob, so the reader gets a broken image
		// and the server logs nothing. Leaving the blob ref in place is visibly
		// wrong to the client and matches what the transform already does for a
		// post with no community (the case above).
		post := authorOwnedEmbedPost()
		post.Author = nil

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(map[string]interface{})
		require.True(t, ok,
			"with no author there is no owning repo for a postv2 record's blobs, so none may be invented")
		assert.Equal(t, "blob", thumb["$type"])
	})

	t.Run("a postv2 record whose author has no PDS URL still proxies by DID", func(t *testing.T) {
		withImageProxy(t)

		// The mirror of the community case beside it: the proxy resolves the DID
		// itself, so the host is only needed for the proxy-disabled fallback. An
		// author whose pds_url column is empty must not lose their images.
		post := authorOwnedEmbedPost()
		post.Author.PDSURL = ""

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedAuthorDID+"/"+embedBlobCID,
			external["thumb"])
	})

	t.Run("a view with no URI falls back to the community", func(t *testing.T) {
		withImageProxy(t)

		// Not a production shape — every indexed post has a URI — but the
		// selection has to answer SOMETHING, and the community is the safe
		// answer: it is what every record predating the flip resolves to, so an
		// unattributable view degrades to the old behaviour rather than to a
		// confident claim about an author's repo.
		post := externalEmbedPost()
		post.URI = ""

		TransformBlobRefsToURLs(post)

		external := post.Embed.(map[string]interface{})["external"].(map[string]interface{})
		assert.Equal(t,
			"https://img.coves.social/img/embed_thumbnail/plain/"+embedCommunityDID+"/"+embedBlobCID,
			external["thumb"])
	})
}
