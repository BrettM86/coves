package comments

import (
	"strings"
	"testing"

	"Coves/internal/atproto/embedimages"

	"github.com/stretchr/testify/assert"
)

func commentCompositeEmbed(images, attachment string) string {
	return `{"$type":"social.coves.embed.imagesWithAttachment","images":` + images + `,"attachment":` + attachment + `}`
}

const commentCompositeExternalAttachment = `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/harbour","title":"Harbour news","description":"Repairs are complete"}}`

func TestCommentImagesWithAttachmentCreateExternalRejected(t *testing.T) {
	for _, withValidator := range []bool{true, false} {
		name := "without validator"
		if withValidator {
			name = "with validator"
		}
		t.Run(name, func(t *testing.T) {
			write := imageCheckWrite{
				lexiconCommentWrite: lexiconCommentWrite{embed: commentCompositeEmbed(lexiconOneImageEmbed, commentCompositeExternalAttachment)},
				withValidator:       withValidator,
			}
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			write.assertNothingWritten(t, client)
		})
	}
}

func TestCommentImagesWithAttachmentUpdateExternalRejected(t *testing.T) {
	for _, withValidator := range []bool{true, false} {
		name := "without validator"
		if withValidator {
			name = "with validator"
		}
		t.Run(name, func(t *testing.T) {
			write := imageCheckWrite{
				lexiconCommentWrite: lexiconCommentWrite{update: true, stored: lexiconStoredComment, embed: commentCompositeEmbed(lexiconOneImageEmbed, commentCompositeExternalAttachment)},
				withValidator:       withValidator,
			}
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			write.assertNothingWritten(t, client)
		})
	}
}

func TestCommentImagesWithAttachmentInvalidGalleryRejected(t *testing.T) {
	missingType := strings.Replace(lexiconOneImageEmbed, `"$type":"social.coves.embed.images",`, "", 1)
	missingTypeIdentityCID := strings.Replace(missingType, "bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi", "bafkqaaa", 1)
	wrongTypeIdentityCID := strings.Replace(lexiconOneImageEmbed, "social.coves.embed.images", "social.coves.embed.post", 1)
	wrongTypeIdentityCID = strings.Replace(wrongTypeIdentityCID, "bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi", "bafkqaaa", 1)
	cases := []struct {
		name, images          string
		update, withValidator bool
	}{
		{name: "create missing type with valid blob", images: missingType, withValidator: true},
		{name: "update missing type with valid blob", images: missingType, update: true, withValidator: true},
		{name: "create missing type with identity CID", images: missingTypeIdentityCID, withValidator: true},
		{name: "update missing type with identity CID", images: missingTypeIdentityCID, update: true, withValidator: true},
		{name: "create wrong type with identity CID", images: wrongTypeIdentityCID, withValidator: true},
		{name: "update wrong type with identity CID", images: wrongTypeIdentityCID, update: true, withValidator: true},
		{name: "create non-object gallery without validator", images: `[]`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			write := imageCheckWrite{
				lexiconCommentWrite: lexiconCommentWrite{update: test.update, stored: lexiconStoredComment, embed: commentCompositeEmbed(test.images, lexiconQuotedPostEmbed)},
				withValidator:       test.withValidator,
			}
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			write.assertNothingWritten(t, client)
		})
	}
}

func TestCommentImagesWithAttachmentInvalidBlobRejected(t *testing.T) {
	cases := []struct {
		name                  string
		old, replacement      string
		update, withValidator bool
	}{
		{name: "create missing blob type", old: `"$type":"blob",`, replacement: ""},
		{name: "create malformed CID", old: "bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi", replacement: "not-a-cid"},
		{name: "create non sha256 CID with validator", old: "bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi", replacement: "bafkqaaa", withValidator: true},
		{name: "create unacceptable MIME", old: "image/jpeg", replacement: "image/svg+xml"},
		{name: "create oversized blob", old: "48213", replacement: "10000001"},
		{name: "create fractional size", old: "48213", replacement: "1.5"},
		{name: "update malformed CID with validator", old: "bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi", replacement: "not-a-cid", update: true, withValidator: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			images := strings.Replace(lexiconOneImageEmbed, test.old, test.replacement, 1)
			write := imageCheckWrite{
				lexiconCommentWrite: lexiconCommentWrite{update: test.update, stored: lexiconStoredComment, embed: commentCompositeEmbed(images, lexiconQuotedPostEmbed)},
				withValidator:       test.withValidator,
			}
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrInvalidEmbed)
			// A blanket composite allow-list refusal must not hide a missing blob check.
			assert.ErrorIs(t, err, embedimages.ErrInvalidImageBlob)
			write.assertNothingWritten(t, client)
		})
	}
}
