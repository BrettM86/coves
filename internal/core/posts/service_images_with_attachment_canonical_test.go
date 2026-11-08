package posts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imagesWithAttachment.images is a plain (non-union) ref to
// social.coves.embed.images, so its $type is optional on the wire. The post
// service accepts a gallery without it and writes the canonical shape (gallery
// $type filled) to the author's PDS; a $type that names the wrong schema at
// any non-union ref position is an invalid embed.

const (
	// postCanonicalTypedGallery is the gallery $type as it appears in the
	// typed composite literals; stripping it gives the untyped submission.
	postCanonicalTypedGallery = `"images":{"$type":"social.coves.embed.images",`

	postCanonicalSourcesComposite = `{"$type":"social.coves.embed.imagesWithAttachment","images":` + imageFacetTwoImages + `,"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://news.example.com/story","title":"Harbour reopens","sources":[{"uri":"https://news.example.com/original","title":"Original report","sourcePost":{"$type":"com.atproto.repo.strongRef","uri":"at://did:plc:lexvalquoted2345abcdefgh/social.coves.community.postv2/3l7bsovn5rz2n","cid":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}}]}}}`
)

// untypedGallery removes the gallery $type from a typed composite literal.
func untypedGallery(t *testing.T, typed string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(typed, postCanonicalTypedGallery), "fixture must carry exactly one typed gallery")
	return strings.Replace(typed, postCanonicalTypedGallery, `"images":{`, 1)
}

func TestCreatePost_ImagesWithAttachmentUntypedGalleryWritesCanonical(t *testing.T) {
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		for _, test := range []struct{ name, typed string }{
			{name: "quote attachment", typed: postWriteQuoteComposite},
			{name: "external attachment", typed: postWriteExternalComposite},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				h := newUnvalidatedLexiconHarness(t)
				if validator {
					h = newLexiconHarness(t, nil)
				}
				expected := jsonObject(t, test.typed)
				_, err := h.create(CreatePostRequest{
					Title:   lexiconStringPtr("Harbour repairs"),
					Content: lexiconStringPtr("The pier is open."),
					Embed:   jsonObject(t, untypedGallery(t, test.typed)),
				})
				require.NoError(t, err, "an untyped gallery at the non-union images ref is lexicon-valid")
				stored := h.repo.onlyRecord(t).Value
				assert.Equal(t, "social.coves.embed.images", stored["embed"].(map[string]any)["images"].(map[string]any)["$type"])
				assert.Equal(t, expected, stored["embed"], "the PDS record carries the canonical (typed) embed")
			})
		}
	}
}

// R5: the admission fingerprint is computed over the canonical embed, so an
// untyped resubmission of a typed post is the same submission.
func TestCreatePost_ImagesWithAttachmentUntypedGallerySameFingerprintAsTyped(t *testing.T) {
	h := newLexiconHarness(t, nil)
	request := func(embed string) CreatePostRequest {
		return CreatePostRequest{
			Title:   lexiconStringPtr("Harbour repairs"),
			Content: lexiconStringPtr("The pier is open."),
			Embed:   jsonObject(t, embed),
		}
	}
	_, err := h.create(request(postWriteQuoteComposite))
	require.NoError(t, err)

	_, err = h.create(request(untypedGallery(t, postWriteQuoteComposite)))
	assert.ErrorIs(t, err, ErrDuplicateSubmission, "typed and untyped galleries are one submission")
	assert.Len(t, h.repo.records, 1, "the duplicate is not written")
}

func TestCreatePost_ImagesWithAttachmentWrongRefTypeRejected(t *testing.T) {
	tests := []struct {
		name, embed string
	}{
		// Passes on base: the gallery $type check already refuses this.
		{name: "gallery typed as external", embed: strings.Replace(postWriteQuoteComposite, postCanonicalTypedGallery, `"images":{"$type":"social.coves.embed.external",`, 1)},
		{name: "source sourcePost typed as bsky post", embed: strings.Replace(postCanonicalSourcesComposite, `"sourcePost":{"$type":"com.atproto.repo.strongRef"`, `"sourcePost":{"$type":"app.bsky.feed.post"`, 1)},
		{name: "quote post strongRef typed as bsky post", embed: strings.Replace(postWriteQuoteComposite, `"post":{"uri"`, `"post":{"$type":"app.bsky.feed.post","uri"`, 1)},
		{name: "gallery image entry typed as view", embed: strings.Replace(postWriteQuoteComposite, `{"image":{"$type":"blob"`, `{"$type":"social.coves.embed.images#view","image":{"$type":"blob"`, 1)},
	}
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		for _, test := range tests {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				h := newUnvalidatedLexiconHarness(t)
				if validator {
					h = newLexiconHarness(t, nil)
				}
				_, err := h.create(CreatePostRequest{Title: lexiconStringPtr("Harbour repairs"), Embed: jsonObject(t, test.embed)})
				assert.ErrorIs(t, err, ErrInvalidEmbed)
				assert.Empty(t, h.repo.records, "no record may be written")
				assert.Empty(t, h.ledger.rows, "no admission state may be created")
			})
		}
	}
}

// Control: the correctly typed sources composite is accepted, so the
// wrong-sourcePost row above is refused for its $type alone.
func TestCreatePost_ImagesWithAttachmentTypedSourcesAccepted(t *testing.T) {
	h := newLexiconHarness(t, nil)
	_, err := h.create(CreatePostRequest{Title: lexiconStringPtr("Harbour repairs"), Embed: jsonObject(t, postCanonicalSourcesComposite)})
	require.NoError(t, err)
}

func TestUpdatePost_TitleOnlyEditCanonicalizesStandingUntypedGallery(t *testing.T) {
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		t.Run(mode, func(t *testing.T) {
			h := newUnvalidatedLexiconHarness(t)
			if validator {
				h = newLexiconHarness(t, nil)
			}
			h.repo.seed(t, postWriteCompositeStored(t, jsonObject(t, untypedGallery(t, postWriteQuoteComposite))))
			_, err := h.update(UpdatePostRequest{Title: lexiconStringPtr("Harbour repairs finished")})
			require.NoError(t, err, "a standing untyped gallery is lexicon-valid and must not block an edit")
			stored := h.repo.snapshot(t, lexiconEditRkey)
			require.NotNil(t, stored)
			assert.NotEqual(t, "bafyreiseededrecord", stored.CID, "the edit must be written")
			assert.Equal(t, "Harbour repairs finished", stored.Value["title"])
			assert.Equal(t, jsonObject(t, postWriteQuoteComposite), stored.Value["embed"], "the PUT record carries the canonical gallery $type")
		})
	}
}

func TestUpdatePost_WrongDescendantRefTypeRejected(t *testing.T) {
	tests := []struct{ name, embed string }{
		{name: "source sourcePost typed as bsky post", embed: strings.Replace(postCanonicalSourcesComposite, `"sourcePost":{"$type":"com.atproto.repo.strongRef"`, `"sourcePost":{"$type":"app.bsky.feed.post"`, 1)},
		{name: "gallery image entry typed as view", embed: strings.Replace(postWriteQuoteComposite, `{"image":{"$type":"blob"`, `{"$type":"social.coves.embed.images#view","image":{"$type":"blob"`, 1)},
	}
	for _, validator := range []bool{true, false} {
		mode := "validator"
		if !validator {
			mode = "nil-validator"
		}
		for _, test := range tests {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				h := newUnvalidatedLexiconHarness(t)
				if validator {
					h = newLexiconHarness(t, nil)
				}
				h.repo.seed(t, postWriteCompositeStored(t, postWriteComposite(t)))
				before := h.repo.snapshot(t, lexiconEditRkey)
				_, err := h.update(UpdatePostRequest{Embed: jsonObject(t, test.embed)})
				assert.ErrorIs(t, err, ErrInvalidEmbed)
				assert.Zero(t, h.repo.puts, "an invalid merged record must not reach PUT")
				assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record and CID remain unchanged")
			})
		}
	}
}
