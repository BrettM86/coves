package embeds

import (
	"encoding/json"
	"os"
	"testing"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/validation"
)

const imagesWithAttachmentType = "social.coves.embed.imagesWithAttachment"

func imagesWithAttachment(attachment map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"$type":      imagesWithAttachmentType,
		"images":     imagesEmbed(imageEntry(blobRef(testCID))),
		"attachment": attachment,
	}
}

func attachmentQuote() map[string]interface{} {
	return map[string]interface{}{
		"$type": TypePost,
		"post": map[string]interface{}{
			"uri": "at://did:plc:quoted/social.coves.community.postv2/3kquoted",
			"cid": secondTestCID,
		},
	}
}

func attachmentExternal() map[string]interface{} {
	return map[string]interface{}{
		"$type": TypeExternal,
		"external": map[string]interface{}{
			"uri": "https://example.com/article", "title": "An article",
			"thumb":  blobRef(secondTestCID),
			"images": []interface{}{imageEntry(blobRef("QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"))},
		},
	}
}

func TestImagesWithAttachmentBlobCIDs(t *testing.T) {
	t.Run("post deduplicates across gallery and attachment", func(t *testing.T) {
		embed := imagesWithAttachment(attachmentExternal())
		gallery := embed["images"].(map[string]interface{})
		gallery["images"] = []interface{}{imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID))}
		require.Equal(t, []string{
			"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
			"QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG",
		}, PostBlobCIDs(embed))
	})
	t.Run("comment includes gallery only", func(t *testing.T) {
		embed := imagesWithAttachment(attachmentQuote())
		gallery := embed["images"].(map[string]interface{})
		gallery["images"] = []interface{}{imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID)), imageEntry(blobRef(testCID))}
		require.Equal(t, []string{
			"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
		}, CommentImageCIDs(embed))
	})
}

func projectedAttachmentGallery() map[string]interface{} {
	return map[string]interface{}{
		"$type": "social.coves.embed.images#view",
		"images": []interface{}{map[string]interface{}{
			"alt":      "an image",
			"thumb":    "https://media.example/img/content_preview/plain/did:plc:testcommunity/bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			"fullsize": "https://media.example/img/content_full/plain/did:plc:testcommunity/bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
		}},
	}
}

func projectedAttachmentQuote() map[string]interface{} {
	return map[string]interface{}{
		"$type": "social.coves.embed.post#view",
		"post": map[string]interface{}{
			"uri": "at://did:plc:quoted/social.coves.community.postv2/3kquoted",
			"cid": "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
		},
	}
}

func projectedAttachmentExternal() map[string]interface{} {
	return map[string]interface{}{
		"$type": "social.coves.embed.external#viewExternal",
		"uri":   "https://example.com/article", "title": "An article",
		"thumb": "https://media.example/img/embed_thumbnail/plain/did:plc:testcommunity/bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
		"images": []interface{}{map[string]interface{}{
			"alt":      "an image",
			"thumb":    "https://media.example/img/content_preview/plain/did:plc:testcommunity/QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG",
			"fullsize": "https://media.example/img/content_full/plain/did:plc:testcommunity/QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG",
		}},
	}
}

func TestImagesWithAttachmentPostExternalView(t *testing.T) {
	withProxy(t, "https://media.example")
	embed := imagesWithAttachment(attachmentExternal())
	HydrateView(embed, testDID, "")
	assert.Equal(t, "social.coves.embed.imagesWithAttachment#view", embed["$type"])
	assert.Equal(t, projectedAttachmentGallery(), embed["images"])
	assert.Equal(t, projectedAttachmentExternal(), embed["attachment"])
}

func TestImagesWithAttachmentPostQuoteView(t *testing.T) {
	withProxy(t, "https://media.example")
	embed := imagesWithAttachment(attachmentQuote())
	HydrateView(embed, testDID, "")
	assert.Equal(t, "social.coves.embed.imagesWithAttachment#view", embed["$type"])
	assert.Equal(t, projectedAttachmentGallery(), embed["images"])
	assert.Equal(t, projectedAttachmentQuote(), embed["attachment"])
}

func TestImagesWithAttachmentCommentView(t *testing.T) {
	withProxy(t, "https://media.example")
	t.Run("quote projects alongside images", func(t *testing.T) {
		embed := imagesWithAttachment(attachmentQuote())
		HydrateCommentView(embed, testDID, "")
		assert.Equal(t, "social.coves.embed.imagesWithAttachment#view", embed["$type"])
		assert.Equal(t, projectedAttachmentGallery(), embed["images"])
		assert.Equal(t, projectedAttachmentQuote(), embed["attachment"])
	})
	t.Run("external attachment never becomes a comment view", func(t *testing.T) {
		embed := imagesWithAttachment(attachmentExternal())
		HydrateCommentView(embed, testDID, "")
		assert.Equal(t, "social.coves.embed.imagesWithAttachment", embed["$type"])
		assert.Equal(t, "social.coves.embed.external", embed["attachment"].(map[string]interface{})["$type"])
		assert.Empty(t, servedProxyURLs(embed))
	})
}

// Use the existing active-comment fixture as the containing view. Record is
// unknown in view schemas; these contracts exercise only the served embed union.
func attachmentContainingView(t *testing.T, comment bool, embed map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile("../../../tests/lexicon-test-data/moderation/comment-view-active-valid.json")
	require.NoError(t, err)
	var view map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &view))
	if !comment {
		view["$type"] = "social.coves.community.post.defs#postView"
		view["uri"] = "at://did:plc:abc123/social.coves.community.postv2/3k7a3dmb5bk2c"
		delete(view, "post")
		delete(view, "stats")
		view["community"] = map[string]interface{}{"did": "did:plc:community", "name": "photos"}
	}
	view["embed"] = embed
	return view
}

// Indigo validates a catalog-known open-union variant even if its ref was
// omitted from the union. Assert this one additive declaration separately.
func assertAttachmentViewDeclared(t *testing.T, validator *validation.LexiconValidator, ref string) {
	t.Helper()
	schema, err := validator.GetCatalog().Resolve(ref)
	require.NoError(t, err)
	object, ok := schema.Def.(lexicon.SchemaObject)
	require.True(t, ok)
	union, ok := object.Properties["embed"].Inner.(lexicon.SchemaUnion)
	require.True(t, ok)
	assert.Contains(t, union.Refs, "social.coves.embed.imagesWithAttachment#view", "the containing view must declare the new served union member")
}

func TestImagesWithAttachmentPostViewSchema(t *testing.T) {
	validator, err := validation.NewEmbeddedLexiconValidator(true)
	require.NoError(t, err)
	ref := "social.coves.community.post.defs#postView"
	assertAttachmentViewDeclared(t, validator, ref)
	// Verify the containing fixture before introducing the new union member.
	require.NoError(t, validator.ValidateObject(attachmentContainingView(t, false, imagesEmbed(imageEntry(blobRef(testCID)))), ref))
	embed := map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment#view",
		"images": projectedAttachmentGallery(), "attachment": projectedAttachmentExternal(),
	}
	assert.NoError(t, validator.ValidateObject(attachmentContainingView(t, false, embed), ref), "strict containing-post validation must recognize the composite and its nested views")
	invalid := copyEmbed(t, embed).(map[string]interface{})
	delete(invalid["attachment"].(map[string]interface{}), "uri")
	assert.Error(t, validator.ValidateObject(attachmentContainingView(t, false, invalid), ref), "the nested external payload requires its URI")
}

func TestImagesWithAttachmentCommentViewSchema(t *testing.T) {
	validator, err := validation.NewEmbeddedLexiconValidator(true)
	require.NoError(t, err)
	ref := "social.coves.community.comment.defs#commentView"
	assertAttachmentViewDeclared(t, validator, ref)
	require.NoError(t, validator.ValidateObject(attachmentContainingView(t, true, imagesEmbed(imageEntry(blobRef(testCID)))), ref))
	embed := map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment#view",
		"images": projectedAttachmentGallery(), "attachment": projectedAttachmentQuote(),
	}
	assert.NoError(t, validator.ValidateObject(attachmentContainingView(t, true, embed), ref), "strict containing-comment validation must recognize the composite and its nested views")
	invalid := copyEmbed(t, embed).(map[string]interface{})
	delete(invalid["attachment"].(map[string]interface{})["post"].(map[string]interface{}), "cid")
	assert.Error(t, validator.ValidateObject(attachmentContainingView(t, true, invalid), ref), "the nested quote requires a complete strongRef")
}
