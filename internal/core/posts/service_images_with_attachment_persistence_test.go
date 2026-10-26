package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Literal persisted expectations are decoded separately from each request so
// in-place changes to the requested embed cannot change the expected value.
const (
	postWriteQuoteComposite    = `{"$type":"social.coves.embed.imagesWithAttachment","images":` + imageFacetTwoImages + `,"attachment":{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:lexvalquoted2345abcdefgh/social.coves.community.postv2/3l7bsovn5rz2n","cid":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}}}`
	postWriteFourImages        = `[` + imageFacetEntryHarbour + `,` + imageFacetEntryLighthouse + `,` + imageFacetEntryPier + `,{"image":` + lexiconBlobJPEG + `,"alt":"boats returning","aspectRatio":{"width":4,"height":3}}]`
	postWriteExternalComposite = `{"$type":"social.coves.embed.imagesWithAttachment","images":{"$type":"social.coves.embed.images","images":` + postWriteFourImages + `},"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story","title":"Harbour reopens","description":"Repairs finished.","thumb":{"$type":"blob","ref":{"$link":"bafkreih6kvpkpwzi6vlk7cx6xoauvterrn3fqp75lwvj6saow4dbotcm2q"},"mimeType":"image/jpeg","size":39920},"images":` + postWriteFourImages + `}}}`
)

// B6 owns the raw total-cap matrix. In particular, invalid entries still
// count, and the ninth entry wins over a nested-main blob validation error.
func TestCreatePost_ImagesWithAttachmentTooManyImages(t *testing.T) {
	tests := []struct {
		name                    string
		main, external          int
		invalidBlob, rawEntries bool
	}{
		{name: "main nine", main: 9},
		{name: "external nine", main: 1, external: 9},
		{name: "combined five plus four", main: 5, external: 4},
		{name: "cap precedes invalid blob", main: 5, external: 4, invalidBlob: true},
		{name: "raw malformed entries count", main: 5, external: 4, rawEntries: true},
	}
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		for _, test := range tests {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				embed := postWriteComposite(t)
				main := make([]any, test.main)
				for index := range main {
					main[index] = jsonObject(t, imageFacetEntryHarbour)
				}
				external := make([]any, test.external)
				for index := range external {
					external[index] = jsonObject(t, imageFacetEntryLighthouse)
				}
				if test.invalidBlob {
					main[0].(map[string]any)["image"].(map[string]any)["ref"] = map[string]any{"$link": "not-a-cid"}
				}
				if test.rawEntries {
					main[0], external[0] = "not-an-image", nil
				}
				embed["images"].(map[string]any)["images"] = main
				embed["attachment"].(map[string]any)["external"].(map[string]any)["images"] = external
				h := newUnvalidatedLexiconHarness(t)
				if validator {
					h = newLexiconHarness(t, nil)
				}
				_, err := h.create(CreatePostRequest{Title: lexiconStringPtr("Harbour repairs"), Embed: embed})
				assert.ErrorIs(t, err, ErrTooManyImages)
				assert.NotErrorIs(t, err, ErrInvalidEmbed)
				assert.Empty(t, h.repo.records, "no record may be written")
				assert.Empty(t, h.ledger.rows, "no admission state may be created")
				assert.Zero(t, h.ledger.nextID, "no reservation may ever be taken")
			})
		}
	}
}

// B7 covers the merged-record update gate, rather than repeating B6's rules.
func TestUpdatePost_ImagesWithAttachmentTooManyMergedImages(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "replacement"
		if retained {
			name = "retained-standing-composite"
		}
		t.Run(name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			overCap := jsonObject(t, postWriteExternalComposite)
			main := overCap["images"].(map[string]any)
			main["images"] = append(main["images"].([]any), jsonObject(t, imageFacetEntryHarbour))
			standing := postWriteComposite(t)
			req := UpdatePostRequest{Embed: overCap}
			if retained {
				standing, req = overCap, UpdatePostRequest{Content: lexiconStringPtr("The pier is open again.")}
			}
			h.repo.seed(t, postWriteCompositeStored(t, standing))
			before := h.repo.snapshot(t, lexiconEditRkey)
			_, err := h.update(req)
			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.Zero(t, h.repo.puts, "an over-cap merged record must not reach PUT")
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record and CID remain unchanged")
		})
	}
}

func TestCreatePost_ImagesWithAttachmentPersistsComposite(t *testing.T) {
	for _, test := range []struct{ name, embed string }{
		{name: "quote strongRef only", embed: postWriteQuoteComposite},
		{name: "external supplied thumb at four plus four", embed: postWriteExternalComposite},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			expected := jsonObject(t, test.embed)
			_, err := h.create(CreatePostRequest{
				Title:   lexiconStringPtr("Harbour repairs"),
				Content: lexiconStringPtr("The pier is open."),
				Embed:   jsonObject(t, test.embed),
			})
			require.NoError(t, err, "valid composite galleries must be accepted")
			stored := h.repo.onlyRecord(t).Value
			assert.Equal(t, expected, stored["embed"], "persist the requested gallery, supplied thumb and strongRef unchanged")
			assert.NotContains(t, stored, "facets")
		})
	}
}

func TestUpdatePost_ImagesWithAttachmentPersistsMergedComposite(t *testing.T) {
	for _, test := range []struct{ name, embed string }{
		{name: "quote strongRef only", embed: postWriteQuoteComposite},
		{name: "external supplied thumb", embed: postWriteExternalComposite},
	} {
		for _, retained := range []bool{false, true} {
			mode := "replacement"
			if retained {
				mode = "retained-standing-composite"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				h := newLexiconHarness(t, nil)
				expected := jsonObject(t, test.embed)
				standing := postWriteComposite(t)
				req := UpdatePostRequest{Embed: jsonObject(t, test.embed)}
				wantContent := "The pier is open."
				if retained {
					standing = jsonObject(t, test.embed)
					wantContent = "The pier is open again."
					req = UpdatePostRequest{Content: lexiconStringPtr("The pier is open again.")}
				}
				h.repo.seed(t, postWriteCompositeStored(t, standing))
				_, err := h.update(req)
				require.NoError(t, err, "valid effective composite galleries must be accepted")
				stored := h.repo.snapshot(t, lexiconEditRkey)
				require.NotNil(t, stored)
				assert.NotEqual(t, "bafyreiseededrecord", stored.CID, "the effective merged record must be written")
				assert.Equal(t, expected, stored.Value["embed"], "persist the effective gallery, supplied thumb and strongRef unchanged")
				assert.Equal(t, wantContent, stored.Value["content"])
				assert.NotContains(t, stored.Value, "facets")
			})
		}
	}
}
