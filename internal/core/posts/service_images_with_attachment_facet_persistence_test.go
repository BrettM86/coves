package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const postCompositeSecondImageFacet = `[{"index":{"byteStart":18,"byteEnd":34},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`

// The external fixture also has an attachment gallery. Placements in both
// variants below address exactly two main images, independent of that gallery.
func postPlacementComposite(t *testing.T, literal string) map[string]any {
	t.Helper()
	embed := jsonObject(t, literal)
	embed["images"] = jsonObject(t, imageFacetTwoImages)
	return embed
}

func TestCreatePost_ImagesWithAttachmentPersistsImageFacets(t *testing.T) {
	for _, test := range []struct{ name, embed string }{
		{name: "quote strongRef only", embed: postWriteQuoteComposite},
		{name: "external supplied thumb", embed: postWriteExternalComposite},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			expectedEmbed := postPlacementComposite(t, test.embed)
			expectedFacets := jsonArray(t, postCompositeSecondImageFacet)
			_, err := h.create(CreatePostRequest{
				Title:   lexiconStringPtr("Harbour repairs"),
				Content: lexiconStringPtr(imageFacetContent),
				Embed:   postPlacementComposite(t, test.embed),
				Facets:  jsonArray(t, postCompositeSecondImageFacet),
			})
			require.NoError(t, err, "index 1 must resolve against the two nested main images")
			stored := h.repo.onlyRecord(t).Value
			assert.Equal(t, expectedEmbed, stored["embed"], "preserve both galleries, supplied thumb and quote strongRef")
			assert.Equal(t, expectedFacets, stored["facets"], "persist the requested placement unchanged")
			assert.Equal(t, imageFacetContent, stored["content"])
		})
	}
}

func TestUpdatePost_ImagesWithAttachmentPersistsMergedImageFacets(t *testing.T) {
	for _, test := range []struct{ name, embed string }{
		{name: "quote strongRef only", embed: postWriteQuoteComposite},
		{name: "external supplied thumb", embed: postWriteExternalComposite},
	} {
		for _, retained := range []bool{false, true} {
			mode := "replacement embed and facets"
			if retained {
				mode = "content-only retains standing composite and facets"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				h := newLexiconHarness(t, nil)
				expectedEmbed := postPlacementComposite(t, test.embed)
				expectedFacets := jsonArray(t, postCompositeSecondImageFacet)
				standing := postWriteCompositeStored(t, postWriteComposite(t))
				standing["content"] = imageFacetContent
				request := UpdatePostRequest{
					Embed:  postPlacementComposite(t, test.embed),
					Facets: jsonArray(t, postCompositeSecondImageFacet),
				}
				expectedContent := imageFacetContent
				if retained {
					standing["embed"] = postPlacementComposite(t, test.embed)
					standing["facets"] = jsonArray(t, postCompositeSecondImageFacet)
					expectedContent = imageFacetContent + " The harbour is open again."
					request = UpdatePostRequest{Content: lexiconStringPtr(expectedContent)}
				}
				h.repo.seed(t, standing)
				_, err := h.update(request)
				require.NoError(t, err, "the valid index-1 placement must survive the effective merged record")
				stored := h.repo.snapshot(t, lexiconEditRkey)
				require.NotNil(t, stored)
				assert.NotEqual(t, "bafyreiseededrecord", stored.CID, "the effective merged record must be written")
				assert.Equal(t, expectedEmbed, stored.Value["embed"], "persist the effective composite unchanged")
				assert.Equal(t, expectedFacets, stored.Value["facets"], "persist the effective placement unchanged")
				assert.Equal(t, expectedContent, stored.Value["content"])
			})
		}
	}
}
