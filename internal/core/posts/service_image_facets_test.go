package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/unfurl"
)

// A social.coves.richtext.facet#image feature places embed.images.images[index]
// in the body. On create and on update, a post whose #image facets break a
// write rule is refused with ErrInvalidImageFacet before any side effect: no
// submission reservation, no unfurl fetch, no upload, no PDS write. The rule
// matrix itself is owned by the richtext package's unit tests; these pin the
// post-service wiring, on the record as it will be written (the merge, on an
// edit).

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// imageFacetContent is 53 bytes: "Before the storm." is [0,17), "After the
// storm." is [18,34), "The repaired pier." is [35,53).
const imageFacetContent = "Before the storm. After the storm. The repaired pier."

// Three images entries with distinct real CIDv1 (raw, sha2-256) blob refs.
const (
	imageFacetEntryHarbour    = `{"image":{"$type":"blob","ref":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"mimeType":"image/jpeg","size":48213},"alt":"the harbour before the storm"}`
	imageFacetEntryLighthouse = `{"image":{"$type":"blob","ref":{"$link":"bafkreibme22gw2h7y2h7tg2fhqotaqjucnbc24deqo72b6mkl2egezxhvy"},"mimeType":"image/jpeg","size":51877},"alt":"the lighthouse after the storm"}`
	imageFacetEntryPier       = `{"image":{"$type":"blob","ref":{"$link":"bafkreih6kvpkpwzi6vlk7cx6xoauvterrn3fqp75lwvj6saow4dbotcm2q"},"mimeType":"image/jpeg","size":39920},"alt":"the repaired pier"}`

	imageFacetTwoImages   = `{"$type":"social.coves.embed.images","images":[` + imageFacetEntryHarbour + `,` + imageFacetEntryLighthouse + `]}`
	imageFacetThreeImages = `{"$type":"social.coves.embed.images","images":[` + imageFacetEntryHarbour + `,` + imageFacetEntryLighthouse + `,` + imageFacetEntryPier + `]}`

	// imageFacetsZeroAndTwo places image 0 after the first sentence and image 2
	// after the third.
	imageFacetsZeroAndTwo = `[` +
		`{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},` +
		`{"index":{"byteStart":35,"byteEnd":53},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`
)

// imageFacetStored is a valid standing postv2 with three images, the body
// above and the given facets literal ("" for none).
func imageFacetStored(facets string) string {
	record := `{"$type":"social.coves.community.postv2","community":"` + lexiconCommunityDID + `",` +
		`"createdAt":"2026-10-01T09:00:00Z","title":"Storm repairs","content":"` + imageFacetContent + `",` +
		`"embed":` + imageFacetThreeImages
	if facets != "" {
		record += `,"facets":` + facets
	}
	return record + `}`
}

// ---------------------------------------------------------------------------
// B6 — a zero-length #image facet is an image-facet failure, not a generic one
// ---------------------------------------------------------------------------

func TestCreatePost_ZeroLengthImageFacetIsInvalidImageFacet(t *testing.T) {
	h := newLexiconHarness(t, nil)

	_, err := h.create(CreatePostRequest{
		Title:   lexiconStringPtr("Storm repairs"),
		Content: lexiconStringPtr(imageFacetContent),
		Facets: jsonArray(t, `[{"index":{"byteStart":17,"byteEnd":17},`+
			`"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`),
		Embed: jsonObject(t, imageFacetTwoImages),
	})

	assert.ErrorIs(t, err, ErrInvalidImageFacet)
	assert.Empty(t, h.repo.records, "no record may be written")
}

// ---------------------------------------------------------------------------
// B7+B9 — create enforces the #image rules before any side effect
// ---------------------------------------------------------------------------

func TestCreatePost_ImageFacetRules(t *testing.T) {
	rejectTests := []struct {
		name   string
		embed  string
		facets string
	}{
		{
			// The unfurl fetch is the side effect an external embed would
			// otherwise trigger.
			name:  "#image facet on an external embed",
			embed: `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/storm-repairs"}}`,
			facets: `[{"index":{"byteStart":0,"byteEnd":17},` +
				`"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
		},
		{
			// Index 8 is also outside the lexicon's 0..7 once the lexicon
			// declares #image, so this pins the rule check ahead of the
			// lexicon check.
			name:  "#image index 8 on a 2-image embed",
			embed: imageFacetTwoImages,
			facets: `[{"index":{"byteStart":0,"byteEnd":17},` +
				`"features":[{"$type":"social.coves.richtext.facet#image","index":8}]}]`,
		},
		{
			// The generic facet check would refuse this range too, as a
			// sentinel-less InvalidRequest; the #image rule must claim it.
			name:  "#image range past the end of the content",
			embed: imageFacetTwoImages,
			facets: `[{"index":{"byteStart":35,"byteEnd":60},` +
				`"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`,
		},
	}

	for _, tc := range rejectTests {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			unfurlService := &fixedUnfurl{result: unfurl.UnfurlResult{
				Type:   "article",
				URI:    "https://news.example.com/storm-repairs",
				Title:  "Storm repairs finished",
				Domain: "news.example.com",
			}}
			h := newLexiconHarness(t, unfurlService)

			_, err := h.create(CreatePostRequest{
				Title:   lexiconStringPtr("Storm repairs"),
				Content: lexiconStringPtr(imageFacetContent),
				Facets:  jsonArray(t, tc.facets),
				Embed:   jsonObject(t, tc.embed),
			})

			assert.ErrorIs(t, err, ErrInvalidImageFacet)
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Zero(t, h.repo.uploads, "no blob may be uploaded")
			assert.Zero(t, h.ledger.nextID, "no submission reservation may ever be taken")
			assert.Equal(t, 0, h.ledger.liveRows(), "no submission reservation may be held")
			assert.Zero(t, unfurlService.calls, "no unfurl fetch may happen")
		})
	}

	t.Run("accept/#image index 0 and 2 on a 3-image embed", func(t *testing.T) {
		h := newLexiconHarness(t, nil)

		_, err := h.create(CreatePostRequest{
			Title:   lexiconStringPtr("Storm repairs"),
			Content: lexiconStringPtr(imageFacetContent),
			Facets:  jsonArray(t, imageFacetsZeroAndTwo),
			Embed:   jsonObject(t, imageFacetThreeImages),
		})
		require.NoError(t, err)

		assert.Equal(t, jsonArray(t, `[`+
			`{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},`+
			`{"index":{"byteStart":35,"byteEnd":53},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`),
			h.repo.onlyRecord(t).Value["facets"])
	})
}

// A broken images embed is the embed's fault: an #image facet pointing into
// it must not turn InvalidEmbed into InvalidImageFacet.
func TestCreatePost_BrokenImagesEmbedWithImageFacetIsInvalidEmbed(t *testing.T) {
	h := newLexiconHarness(t, nil)

	_, err := h.create(CreatePostRequest{
		Title:   lexiconStringPtr("Storm repairs"),
		Content: lexiconStringPtr(imageFacetContent),
		Facets: jsonArray(t, `[{"index":{"byteStart":0,"byteEnd":17},`+
			`"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`),
		Embed: jsonObject(t, `{"$type":"social.coves.embed.images","images":{}}`),
	})

	assert.ErrorIs(t, err, ErrInvalidEmbed)
	assert.NotErrorIs(t, err, ErrInvalidImageFacet)
	assert.Empty(t, h.repo.records, "no record may be written")
}

// ---------------------------------------------------------------------------
// B8+B9 — update enforces the #image rules on the merged record
// ---------------------------------------------------------------------------

func TestUpdatePost_ImageFacetRules(t *testing.T) {
	rejectTests := []struct {
		name   string
		stored string
		req    func(t *testing.T) UpdatePostRequest
	}{
		{
			// The request carries no embed: the rule must judge index 3
			// against the standing 3-image embed.
			name:   "facets-only edit placing index 3 against the standing 3 images",
			stored: imageFacetStored(""),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{
					Facets: jsonArray(t, `[{"index":{"byteStart":18,"byteEnd":34},`+
						`"features":[{"$type":"social.coves.richtext.facet#image","index":3}]}]`),
				}
			},
		},
		{
			// The request carries no facets: the standing index 2 now points
			// past the new 2-image embed.
			name:   "embed-only edit down to 2 images under standing facets placing index 2",
			stored: imageFacetStored(imageFacetsZeroAndTwo),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Embed: jsonObject(t, imageFacetTwoImages)}
			},
		},
	}

	for _, tc := range rejectTests {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			h.repo.seed(t, jsonObject(t, tc.stored))
			before := h.repo.snapshot(t, lexiconEditRkey)

			_, err := h.update(tc.req(t))

			assert.ErrorIs(t, err, ErrInvalidImageFacet)
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
		})
	}

	t.Run("accept/facets-only edit placing index 0 and 2 against the standing 3 images", func(t *testing.T) {
		h := newLexiconHarness(t, nil)
		h.repo.seed(t, jsonObject(t, imageFacetStored("")))

		_, err := h.update(UpdatePostRequest{Facets: jsonArray(t, imageFacetsZeroAndTwo)})
		require.NoError(t, err)

		stored := h.repo.snapshot(t, lexiconEditRkey)
		require.NotNil(t, stored)
		assert.NotEqual(t, "bafyreiseededrecord", stored.CID, "the edit must have been written")
		assert.Equal(t, jsonArray(t, `[`+
			`{"index":{"byteStart":0,"byteEnd":17},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},`+
			`{"index":{"byteStart":35,"byteEnd":53},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`),
			stored.Value["facets"])
	})
}
