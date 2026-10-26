package posts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post create and update refuse an images embed carrying more than 8 images
// as ErrTooManyImages (never ErrInvalidEmbed), whether or not a lexicon
// validator is wired, and refuse any embed.images[].image that is not a
// well-formed image blob ref as ErrInvalidEmbed on their own, without the
// lexicon backstop. A refusal writes nothing.

// blobCheckCID is a real CIDv1 (raw, sha2-256), the one in lexiconBlobJPEG.
const blobCheckCID = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"

// blobCheckImage is one images entry whose image is the given blob JSON.
func blobCheckImage(blob string) string { return `{"image":` + blob + `}` }

// blobCheckEmbed is an images embed of the given entries.
func blobCheckEmbed(entries ...string) string {
	return `{"$type":"social.coves.embed.images","images":[` + strings.Join(entries, ",") + `]}`
}

// validImageEntries is n entries that each carry lexiconBlobJPEG.
func validImageEntries(n int) []string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = blobCheckImage(lexiconBlobJPEG)
	}
	return entries
}

// blobCheckStored is a standing postv2 carrying embed.
func blobCheckStored(embed string) string {
	return `{"$type":"social.coves.community.postv2","community":"` + lexiconCommunityDID + `",` +
		`"createdAt":"2026-10-01T09:00:00Z","title":"Harbour photos","embed":` + embed + `}`
}

// ---------------------------------------------------------------------------
// The 8-image cap
// ---------------------------------------------------------------------------

func TestCreatePost_MoreThanEightImagesIsTooManyImages(t *testing.T) {
	tests := []struct {
		name          string
		withValidator bool
		images        int
		wantAccepted  bool
	}{
		{name: "validator wired, 9 images", withValidator: true, images: 9},
		{name: "no validator, 9 images", withValidator: false, images: 9},
		// GUARD: exactly 8 is within the cap.
		{name: "validator wired, exactly 8 images", withValidator: true, images: 8, wantAccepted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h *lexiconHarness
			if tc.withValidator {
				h = newLexiconHarness(t, nil)
			} else {
				h = newUnvalidatedLexiconHarness(t)
			}

			_, err := h.create(CreatePostRequest{
				Title: lexiconStringPtr("Harbour photos"),
				Embed: jsonObject(t, blobCheckEmbed(validImageEntries(tc.images)...)),
			})

			if tc.wantAccepted {
				require.NoError(t, err)
				assert.Len(t, h.repo.records, 1, "the post should be stored")
				return
			}
			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Equal(t, 0, h.ledger.liveRows(), "the submission reservation must be released")
		})
	}
}

func TestUpdatePost_MoreThanEightImagesIsTooManyImages(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		req    func(t *testing.T) UpdatePostRequest
	}{
		{
			name:   "replacing a 1-image embed with 9 images",
			stored: blobCheckStored(blobCheckEmbed(validImageEntries(1)...)),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Embed: jsonObject(t, blobCheckEmbed(validImageEntries(9)...))}
			},
		},
		{
			name:   "title-only edit of a record already carrying 9 images",
			stored: blobCheckStored(blobCheckEmbed(validImageEntries(9)...)),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Title: lexiconStringPtr("Harbour photos, corrected")}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			h.repo.seed(t, jsonObject(t, tc.stored))
			before := h.repo.snapshot(t, lexiconEditRkey)

			_, err := h.update(tc.req(t))

			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
		})
	}
}

// malformedNinthImageEmbeds are images embeds of 9 entries where an entry is
// malformed in shape; the count is checked before any entry's shape.
var malformedNinthImageEmbeds = []struct {
	name  string
	embed string
}{
	{name: "8 valid entries and a 9th with no image object", embed: blobCheckEmbed(append(validImageEntries(8), `{"alt":"no image"}`)...)},
	{name: "9 bare string entries", embed: blobCheckEmbed(strings.Split(strings.Repeat(`"img",`, 8)+`"img"`, ",")...)},
}

func TestCreatePost_NineImagesWithMalformedEntryIsTooManyImages(t *testing.T) {
	for _, tc := range malformedNinthImageEmbeds {
		t.Run(tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)

			_, err := h.create(CreatePostRequest{
				Title: lexiconStringPtr("Harbour photos"),
				Embed: jsonObject(t, tc.embed),
			})

			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Equal(t, 0, h.ledger.liveRows(), "the submission reservation must be released")
		})
	}
}

func TestUpdatePost_NineImagesWithMalformedEntryIsTooManyImages(t *testing.T) {
	for _, tc := range malformedNinthImageEmbeds {
		t.Run(tc.name, func(t *testing.T) {
			h := newLexiconHarness(t, nil)
			h.repo.seed(t, jsonObject(t, blobCheckStored(blobCheckEmbed(validImageEntries(1)...))))
			before := h.repo.snapshot(t, lexiconEditRkey)

			_, err := h.update(UpdatePostRequest{Embed: jsonObject(t, tc.embed)})

			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
		})
	}
}

// ---------------------------------------------------------------------------
// Blob-ref checks on embed.images[].image, with no lexicon validator wired
// ---------------------------------------------------------------------------

func TestCreatePost_ImageBlobChecks(t *testing.T) {
	tests := []struct {
		name         string
		embed        string
		wantAccepted bool
	}{
		{
			name:  "image without $type",
			embed: blobCheckEmbed(blobCheckImage(`{"ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg","size":48213}`)),
		},
		{
			name:  "$type image",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"image","ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg","size":48213}`)),
		},
		{
			name:  "ref $link that is not a CID",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","ref":{"$link":"not-a-cid"},"mimeType":"image/jpeg","size":48213}`)),
		},
		{
			name:  "ref missing",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","mimeType":"image/jpeg","size":48213}`)),
		},
		{
			name:  "ref $link a number",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","ref":{"$link":42},"mimeType":"image/jpeg","size":48213}`)),
		},
		{
			name: "mimeType image/svg+xml on the third of three images",
			embed: blobCheckEmbed(
				blobCheckImage(lexiconBlobJPEG),
				blobCheckImage(lexiconBlobJPEG),
				blobCheckImage(`{"$type":"blob","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/svg+xml","size":48213}`),
			),
		},
		{
			name:  "size 10000001",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg","size":10000001}`)),
		},
		{
			name:  "size missing",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg"}`)),
		},
		{
			name:  "size 1234.5",
			embed: blobCheckEmbed(blobCheckImage(`{"$type":"blob","ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg","size":1234.5}`)),
		},
		{
			// GUARD: every accepted MIME type, and size at the 10000000 cap.
			name: "png, jpeg, webp and gif, the last at size 10000000",
			embed: blobCheckEmbed(
				blobCheckImage(`{"$type":"blob","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/png","size":48213}`),
				blobCheckImage(`{"$type":"blob","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/jpeg","size":48213}`),
				blobCheckImage(`{"$type":"blob","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/webp","size":48213}`),
				blobCheckImage(`{"$type":"blob","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/gif","size":10000000}`),
			),
			wantAccepted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnvalidatedLexiconHarness(t)

			_, err := h.create(CreatePostRequest{
				Title: lexiconStringPtr("Harbour photos"),
				Embed: jsonObject(t, tc.embed),
			})

			if tc.wantAccepted {
				require.NoError(t, err)
				assert.Len(t, h.repo.records, 1, "the post should be stored")
				return
			}
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			assert.Empty(t, h.repo.records, "no record may be written")
			assert.Equal(t, 0, h.ledger.liveRows(), "the submission reservation must be released")
		})
	}
}

func TestUpdatePost_NonBlobImageIsInvalidEmbed(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		req    func(t *testing.T) UpdatePostRequest
	}{
		{
			name:   "replacing the embed with an image of $type image",
			stored: blobCheckStored(blobCheckEmbed(validImageEntries(1)...)),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Embed: jsonObject(t, blobCheckEmbed(blobCheckImage(
					`{"$type":"image","ref":{"$link":"`+blobCheckCID+`"},"mimeType":"image/jpeg","size":48213}`)))}
			},
		},
		{
			name: "title-only edit of a record whose standing image has no $type",
			stored: blobCheckStored(blobCheckEmbed(blobCheckImage(
				`{"ref":{"$link":"` + blobCheckCID + `"},"mimeType":"image/jpeg","size":48213}`))),
			req: func(t *testing.T) UpdatePostRequest {
				return UpdatePostRequest{Title: lexiconStringPtr("Harbour photos, corrected")}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnvalidatedLexiconHarness(t)
			h.repo.seed(t, jsonObject(t, tc.stored))
			before := h.repo.snapshot(t, lexiconEditRkey)

			_, err := h.update(tc.req(t))

			assert.ErrorIs(t, err, ErrInvalidEmbed)
			assert.Equal(t, before, h.repo.snapshot(t, lexiconEditRkey), "the standing record must be untouched")
		})
	}
}
