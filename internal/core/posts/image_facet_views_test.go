package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/embeds"
)

func TestTransformBlobRefsToURLsFiltersImagePlacementsWithoutChangingStoredRecord(t *testing.T) {
	withImageProxy(t)
	const facets = `[
		{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
		{"index":{"byteStart":35,"byteEnd":53},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]},
		{"index":{"byteStart":18,"byteEnd":23},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
		{"index":{"byteStart":24,"byteEnd":34},"features":[{"$type":"social.coves.richtext.facet#image","index":5}]}]`
	stored := jsonObject(t, imageFacetStored(facets))
	// Both views start with the actual shared record map. Only their served
	// embeds are deserialized separately, as on the post read path.
	for range 2 {
		view := &PostView{
			URI: embedPostV2URI, Author: &AuthorView{DID: embedAuthorDID},
			Record: stored, Embed: jsonObject(t, imageFacetThreeImages),
		}
		TransformBlobRefsToURLs(view)
		record, ok := view.Record.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, jsonArray(t, imageFacetsZeroAndTwo), record["facets"], "only first index 0 and valid index 2 placements survive")
		assert.Equal(t, imageFacetContent, record["content"], "covered text stays in the body")
		assert.Equal(t, jsonObject(t, imageFacetThreeImages), record["embed"], "returned record embed stays as stored")
		gallery := view.Embed.(map[string]interface{})
		assert.Equal(t, "social.coves.embed.images#view", gallery["$type"])
		images := gallery["images"].([]interface{})
		require.Len(t, images, 3, "dropped placements must not remove gallery images")
		assert.Equal(t, "the harbour before the storm", images[0].(map[string]interface{})["alt"])
		assert.Equal(t, "the lighthouse after the storm", images[1].(map[string]interface{})["alt"])
		assert.Equal(t, "the repaired pier", images[2].(map[string]interface{})["alt"])
		assert.Equal(t, jsonObject(t, imageFacetStored(facets)), stored, "filtering must not mutate a shared source record or its facets")
	}
}

func TestTransformBlobRefsToURLsRemovesPlacementsWithoutServedImages(t *testing.T) {
	withImageProxy(t)
	const facets = `[
		{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
		{"index":{"byteStart":6,"byteEnd":10},"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://example.com"}]},
		{"index":{"byteStart":11,"byteEnd":16},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
	const otherFacets = `[{"index":{"byteStart":6,"byteEnd":10},"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://example.com"}]}]`
	for _, test := range []struct {
		name, embed string
		noOwner     bool
	}{
		{name: "missing embed"},
		{name: "unservable forged view", embed: `{"$type":"social.coves.embed.images#view","images":[{"alt":"forged"}]}`},
		{name: "unprojected gallery", embed: `{"$type":"social.coves.embed.images","images":[{"alt":"missing blob"},{"alt":"also missing"}]}`},
		{name: "no author owner", embed: imageFacetTwoImages, noOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storedLiteral := `{"$type":"social.coves.community.postv2","content":"` + imageFacetContent + `","facets":` + facets
			if test.embed != "" {
				storedLiteral += `,"embed":` + test.embed
			}
			storedLiteral += `}`
			stored := jsonObject(t, storedLiteral)
			view := &PostView{URI: embedPostV2URI, Author: &AuthorView{DID: embedAuthorDID}, Record: stored}
			if test.embed != "" {
				view.Embed = embeds.ServableEmbed(jsonObject(t, test.embed))
			}
			if test.noOwner {
				view.Author = nil
			}
			TransformBlobRefsToURLs(view)
			record, ok := view.Record.(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, jsonArray(t, otherFacets), record["facets"], "every image placement must be removed when no image view was served")
			assert.Equal(t, imageFacetContent, record["content"])
			assert.Equal(t, jsonObject(t, storedLiteral), stored, "shared storage stays unchanged on early-return paths too")
		})
	}
}

func TestTransformBlobRefsToURLsImagePlacementsUseOnlyNestedMainGallery(t *testing.T) {
	withImageProxy(t)
	const facets = `[
		{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]},
		{"index":{"byteStart":18,"byteEnd":34},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`
	const expected = `[{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
	const composite = `{"$type":"social.coves.embed.imagesWithAttachment","images":` + imageFacetTwoImages + `,"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story","title":"Harbour repairs","images":[` + imageFacetEntryPier + `,` + imageFacetEntryHarbour + `,` + imageFacetEntryLighthouse + `]}}}`
	stored := jsonObject(t, imageFacetStored(facets))
	stored["embed"] = jsonObject(t, composite)
	view := &PostView{URI: embedPostV2URI, Author: &AuthorView{DID: embedAuthorDID}, Record: stored, Embed: jsonObject(t, composite)}
	TransformBlobRefsToURLs(view)
	record := view.Record.(map[string]interface{})
	assert.Equal(t, jsonArray(t, expected), record["facets"], "attachment gallery images cannot satisfy a main-gallery index")
	assert.Equal(t, imageFacetContent, record["content"])
	served := view.Embed.(map[string]interface{})
	assert.Equal(t, "social.coves.embed.imagesWithAttachment#view", served["$type"])
	main := served["images"].(map[string]interface{})["images"].([]interface{})
	require.Len(t, main, 2)
	assert.Equal(t, "the harbour before the storm", main[0].(map[string]interface{})["alt"])
	assert.Equal(t, "the lighthouse after the storm", main[1].(map[string]interface{})["alt"])
	attachment := served["attachment"].(map[string]interface{})
	assert.Equal(t, "https://news.example.com/story", attachment["uri"])
	assert.Equal(t, "Harbour repairs", attachment["title"])
	external := attachment["images"].([]interface{})
	require.Len(t, external, 3)
	assert.Equal(t, "the repaired pier", external[0].(map[string]interface{})["alt"])
	assert.Equal(t, "the harbour before the storm", external[1].(map[string]interface{})["alt"])
	assert.Equal(t, "the lighthouse after the storm", external[2].(map[string]interface{})["alt"])
	assert.Equal(t, jsonArray(t, facets), stored["facets"])
	assert.Equal(t, jsonObject(t, composite), stored["embed"])
}
