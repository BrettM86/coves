package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Index 2 is legal in the lexicon, but cannot resolve against two main
// images. These tests own entry-point rejection; richtext owns the rules.
const postCompositeThirdImageFacet = `[{"index":{"byteStart":35,"byteEnd":53},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`

func TestCreatePost_ImagesWithAttachmentUnresolvedImageFacet(t *testing.T) {
	h := newLexiconHarness(t, nil)

	_, err := h.create(CreatePostRequest{
		Title:   lexiconStringPtr("Harbour repairs"),
		Content: lexiconStringPtr(imageFacetContent),
		Facets:  jsonArray(t, postCompositeThirdImageFacet),
		Embed:   jsonObject(t, postWriteQuoteComposite),
	})

	assert.ErrorIs(t, err, ErrInvalidImageFacet)
	assert.Zero(t, h.ledger.nextID, "no submission reservation may ever be taken")
	assert.Empty(t, h.ledger.rows, "no admission state may be created")
	assert.Zero(t, h.repo.puts, "an unresolved placement must not reach PUT")
	assert.Empty(t, h.repo.records, "no record may be written")
}

func TestUpdatePost_ImagesWithAttachmentUnresolvedMergedImageFacet(t *testing.T) {
	for _, test := range []struct {
		name      string
		embedOnly bool
	}{
		{name: "embed-only shrink under standing facets", embedOnly: true},
		{name: "facets-only edit against standing main images"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			standingEmbed := jsonObject(t, postWriteQuoteComposite)
			standingFacets := `[{"index":{"byteStart":18,"byteEnd":34},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
			request := UpdatePostRequest{Facets: jsonArray(t, postCompositeThirdImageFacet)}
			if test.embedOnly {
				standingEmbed["images"] = jsonObject(t, imageFacetThreeImages)
				standingFacets = postCompositeThirdImageFacet
				request = UpdatePostRequest{Embed: jsonObject(t, postWriteQuoteComposite)}
			}
			standing := postWriteCompositeStored(t, standingEmbed)
			standing["content"] = imageFacetContent
			standing["facets"] = jsonArray(t, standingFacets)
			h.repo.seed(t, standing)
			before := h.repo.snapshot(t, lexiconEditRkey)

			_, err := h.update(request)

			assert.ErrorIs(t, err, ErrInvalidImageFacet)
			assert.Zero(t, h.repo.puts, "an invalid effective merged record must not reach PUT")
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the full standing record and CID remain unchanged")
		})
	}
}
