package embeds

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImagesWithAttachmentForgedMediaSafety(t *testing.T) {
	withProxy(t, "https://media.example")
	for _, test := range []struct {
		name       string
		stored     map[string]interface{}
		attachment map[string]interface{}
	}{
		{
			name: "gallery view-only URLs with a valid blob",
			stored: func() map[string]interface{} {
				embed := imagesWithAttachment(attachmentQuote())
				image := embed["images"].(map[string]interface{})["images"].([]interface{})[0].(map[string]interface{})
				image["thumb"], image["fullsize"] = authorURL("content_preview"), authorURL("content_full")
				return embed
			}(),
			attachment: projectedAttachmentQuote(),
		},
		{
			name: "external string thumb with a valid sibling gallery",
			stored: imagesWithAttachment(map[string]interface{}{
				"$type": TypeExternal,
				"external": map[string]interface{}{
					"uri": "https://example.com/article", "title": "An article",
					"thumb": authorURL("embed_thumbnail"),
				},
			}),
			attachment: map[string]interface{}{
				"$type": "social.coves.embed.external#viewExternal",
				"uri":   "https://example.com/article", "title": "An article",
			},
		},
		{
			name: "quote view with a forged resolved preview",
			stored: func() map[string]interface{} {
				quote := attachmentQuote()
				quote["$type"] = "social.coves.embed.post#view"
				quote["resolved"] = map[string]interface{}{
					"images": []interface{}{map[string]interface{}{
						"thumb": authorURL("content_preview"), "fullsize": authorURL("content_full"),
					}},
				}
				return imagesWithAttachment(quote)
			}(),
			attachment: projectedAttachmentQuote(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			served := serve(t, test.stored)
			assert.NotContains(t, servedProxyURLs(served), authorURL("content_preview"))
			assert.NotContains(t, servedProxyURLs(served), authorURL("content_full"))
			assert.NotContains(t, servedProxyURLs(served), authorURL("embed_thumbnail"))
			assert.Equal(t, map[string]interface{}{
				"$type":  "social.coves.embed.imagesWithAttachment#view",
				"images": projectedAttachmentGallery(), "attachment": test.attachment,
			}, served, "sanitization must preserve and project the valid sibling gallery")
		})
	}
}

// A malformed leaf must never leak authored media, even when projection fails.
// The whole composite may be withheld; if retained, neither part may be torn
// into a partially hydrated view.
func TestImagesWithAttachmentMalformedMediaFailsClosed(t *testing.T) {
	withProxy(t, "https://media.example")
	for _, test := range []struct {
		name     string
		external map[string]interface{}
	}{
		{
			name: "malformed media",
			external: map[string]interface{}{
				"uri": "https://example.com/article", "title": "An article",
				"thumb": map[string]interface{}{"ref": map[string]interface{}{}},
				"images": []interface{}{map[string]interface{}{
					"image": authorURL("content_full"), "thumb": authorURL("content_preview"),
					"fullsize": authorURL("content_full"), "alt": "malformed image",
				}},
			},
		},
		{name: "missing external URI", external: map[string]interface{}{}},
		{name: "non-string external URI", external: map[string]interface{}{"uri": 42}},
		{name: "empty external URI", external: map[string]interface{}{"uri": ""}},
		{name: "relative external URI", external: map[string]interface{}{"uri": "/article"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := imagesWithAttachment(map[string]interface{}{
				"$type": TypeExternal, "external": test.external,
			})
			served := serve(t, stored)
			assert.Empty(t, servedProxyURLs(served), "failed nested projection must not expose author-written media URLs")
			if embed, ok := served.(map[string]interface{}); ok {
				assert.Equal(t, "social.coves.embed.imagesWithAttachment", embed["$type"], "malformed attachment must not receive a composite view type")
				gallery, err := json.Marshal(embed["images"])
				assert.NoError(t, err)
				assert.JSONEq(t, `{"$type":"social.coves.embed.images","images":[{"alt":"an image","image":{"$type":"blob","mimeType":"image/jpeg","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"size":52813}}]}`, string(gallery), "failed projection leaves the sibling gallery in record shape")
			}
		})
	}
}

func TestImagesWithAttachmentRemovedQuoteRedactsCopiedContent(t *testing.T) {
	withProxy(t, "https://media.example")
	quote := attachmentQuote()
	quote["$type"] = "social.coves.embed.post#view"
	quote["post"].(map[string]interface{})["text"] = "removed copied body"
	quote["post"].(map[string]interface{})["embed"] = imagesEmbed(imageEntry(blobRef(secondTestCID)))
	quote["resolved"] = map[string]interface{}{
		"text": "removed copied body", "title": "removed copied title",
		"images":        []interface{}{map[string]interface{}{"fullsize": authorURL("content_full")}},
		"contentLabels": []interface{}{map[string]interface{}{"val": "porn"}},
	}
	for _, test := range []struct {
		name  string
		serve func(*testing.T, interface{}) interface{}
	}{{"post", serve}, {"comment", serveComment}} {
		t.Run(test.name, func(t *testing.T) {
			served := test.serve(t, imagesWithAttachment(quote))
			assert.Equal(t, map[string]interface{}{
				"$type":  "social.coves.embed.imagesWithAttachment#view",
				"images": projectedAttachmentGallery(), "attachment": projectedAttachmentQuote(),
			}, served, "D-QUOTES retains only the strongRef while the quoter's gallery survives")
		})
	}
}
