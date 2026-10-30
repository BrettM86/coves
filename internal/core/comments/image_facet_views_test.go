package comments

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
	"Coves/internal/core/users"
)

func TestBuildCommentViewFiltersImagePlacementsAgainstServedGallery(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
	const facets = `[
		{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]},
		{"index":{"byteStart":17,"byteEnd":22},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
		{"index":{"byteStart":12,"byteEnd":22},"features":[{"$type":"social.coves.richtext.facet#codeBlock"}]},
		{"index":{"byteStart":23,"byteEnd":27},"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://example.com"}]}]`
	const otherFacets = `{"index":{"byteStart":12,"byteEnd":22},"features":[{"$type":"social.coves.richtext.facet#codeBlock"}]},{"index":{"byteStart":23,"byteEnd":27},"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://example.com"}]}`
	const validFacets = `[{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]},` + otherFacets + `]`
	for _, test := range []struct {
		name, embed string
		served      bool
		nested      bool
	}{
		{name: "plain gallery", embed: lexiconTwoImageEmbed, served: true},
		{name: "gallery with quote", embed: commentCompositeEmbed(lexiconTwoImageEmbed, lexiconQuotedPostEmbed), served: true, nested: true},
		{name: "absent embed"},
		{name: "unprojected gallery", embed: `{"$type":"social.coves.embed.images","images":[{"alt":"1"},{"alt":"2"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			const root = "at://did:plc:root/social.coves.community.postv2/root"
			comment := createTestComment("at://did:plc:commenter/social.coves.community.comment/one", "did:plc:commenter", "commenter.test", root, root, 0)
			comment.Content = "First image code image link"
			storedFacets := facets
			comment.ContentFacets = &storedFacets
			storedEmbed := test.embed
			if test.embed != "" {
				comment.Embed = &storedEmbed
			}
			view := (&commentService{}).buildCommentView(comment, map[string]*users.User{})
			record, ok := view.Record.(*CommentRecord)
			require.True(t, ok)
			expected := `[` + otherFacets + `]`
			if test.served {
				expected = validFacets
			}
			assert.Equal(t, lexiconCommentFacets(t, expected), record.Facets, "placements must resolve in the served gallery and stay outside code")
			assert.Equal(t, "First image code image link", record.Content, "no fallback text or body rewrite")
			assert.Equal(t, "First image code image link", comment.Content)
			assert.Equal(t, facets, *comment.ContentFacets, "stored facets are unchanged")
			if test.embed == "" {
				assert.Nil(t, view.Embed)
				assert.Nil(t, comment.Embed)
				assert.Nil(t, record.Embed)
				return
			}
			assert.Equal(t, test.embed, *comment.Embed)
			assert.Equal(t, lexiconCommentObject(t, test.embed), record.Embed, "record embed stays as stored")
			if test.served {
				gallery := view.Embed.(map[string]interface{})
				if test.nested {
					attachment := gallery["attachment"].(map[string]interface{})
					assert.Equal(t, lexiconCommentObject(t, lexiconQuotedPostEmbed)["post"], attachment["post"])
					gallery = gallery["images"].(map[string]interface{})
				}
				assert.Equal(t, "social.coves.embed.images#view", gallery["$type"])
				images := gallery["images"].([]interface{})
				require.Len(t, images, 2, "the image whose placement was removed remains in the gallery")
				assert.Equal(t, "1", images[0].(map[string]interface{})["alt"])
				assert.Equal(t, "2", images[1].(map[string]interface{})["alt"])
			}
		})
	}
}
