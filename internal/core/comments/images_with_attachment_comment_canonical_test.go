package comments

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imagesWithAttachment.images is a plain (non-union) ref, so a gallery without
// $type is lexicon-valid. The comment service accepts it and writes the
// canonical shape (gallery $type filled); a $type naming the wrong schema at a
// non-union ref position is ErrInvalidEmbed.

func commentUntypedGallery(t *testing.T, typed string) string {
	t.Helper()
	const galleryType = `"$type":"social.coves.embed.images",`
	require.Equal(t, 1, strings.Count(typed, galleryType), "fixture must carry exactly one gallery $type")
	return strings.Replace(typed, galleryType, "", 1)
}

func TestCommentImagesWithAttachmentUntypedGalleryEmptyContentCreateWritesCanonical(t *testing.T) {
	for _, withValidator := range []bool{true, false} {
		name := "without validator"
		if withValidator {
			name = "with validator"
		}
		t.Run(name, func(t *testing.T) {
			service, client := newUnvalidatedCommentService(t)
			if withValidator {
				service, client = newLexiconCommentService(t)
			}
			embed := commentCompositeEmbed(commentUntypedGallery(t, lexiconOneImageEmbed), lexiconQuotedPostEmbed)
			_, err := service.CreateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), CreateCommentRequest{
				Reply:   lexiconCommentReply(),
				Content: "",
				Embed:   lexiconCommentObject(t, embed),
			})
			require.NoError(t, err, "an image-only comment with an untyped gallery is valid")
			written := writtenComment(t, client)
			assert.Equal(t, "", written.Content)
			assert.Equal(t, "social.coves.embed.images", written.Embed.(map[string]any)["images"].(map[string]any)["$type"])
			assert.Equal(t, lexiconCommentObject(t, commentCompositeEmbed(lexiconOneImageEmbed, lexiconQuotedPostEmbed)), written.Embed,
				"the created record carries the canonical (typed) embed")
		})
	}
}

func TestCommentImagesWithAttachmentUntypedGalleryUpdateWritesCanonical(t *testing.T) {
	for _, withValidator := range []bool{true, false} {
		name := "without validator"
		if withValidator {
			name = "with validator"
		}
		t.Run(name, func(t *testing.T) {
			write := imageCheckWrite{
				lexiconCommentWrite: lexiconCommentWrite{
					update: true, stored: lexiconStoredComment,
					embed: commentCompositeEmbed(commentUntypedGallery(t, lexiconOneImageEmbed), lexiconQuotedPostEmbed),
				},
				withValidator: withValidator,
			}
			client, err := write.run(t)
			require.NoError(t, err, "an untyped gallery is valid on update")
			written := writtenComment(t, client)
			assert.Equal(t, "Edited comment", written.Content)
			assert.Equal(t, lexiconCommentObject(t, commentCompositeEmbed(lexiconOneImageEmbed, lexiconQuotedPostEmbed)), written.Embed,
				"the PUT record carries the canonical (typed) embed")
		})
	}
}

func TestCommentImagesWithAttachmentWrongRefTypeRejected(t *testing.T) {
	wrongGallery := strings.Replace(lexiconOneImageEmbed, `"$type":"social.coves.embed.images"`, `"$type":"social.coves.embed.external"`, 1)
	viewEntry := strings.Replace(lexiconOneImageEmbed, `{"image":{"$type":"blob"`, `{"$type":"social.coves.embed.images#view","image":{"$type":"blob"`, 1)
	wrongStrongRef := strings.Replace(lexiconQuotedPostEmbed, `"post":{"uri"`, `"post":{"$type":"app.bsky.feed.post","uri"`, 1)
	cases := []lexiconCommentWrite{
		// Passes on base: the gallery $type check already refuses this.
		{name: "create gallery typed as external", embed: commentCompositeEmbed(wrongGallery, lexiconQuotedPostEmbed)},
		{name: "create gallery image entry typed as view", embed: commentCompositeEmbed(viewEntry, lexiconQuotedPostEmbed)},
		{name: "create quote strongRef typed as bsky post", embed: commentCompositeEmbed(lexiconOneImageEmbed, wrongStrongRef)},
		{name: "update gallery typed as external", update: true, stored: lexiconStoredComment, embed: commentCompositeEmbed(wrongGallery, lexiconQuotedPostEmbed)},
		{name: "update gallery image entry typed as view", update: true, stored: lexiconStoredComment, embed: commentCompositeEmbed(viewEntry, lexiconQuotedPostEmbed)},
	}
	for _, withValidator := range []bool{true, false} {
		mode := "without validator"
		if withValidator {
			mode = "with validator"
		}
		for _, test := range cases {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				write := imageCheckWrite{lexiconCommentWrite: test, withValidator: withValidator}
				client, err := write.run(t)
				assert.ErrorIs(t, err, ErrInvalidEmbed)
				write.assertNothingWritten(t, client)
			})
		}
	}
	// R6: a non-map embed is passed through to existing validation unchanged,
	// so the lexicon validator still refuses it exactly as before. Passes on
	// base. (Without a validator base accepts it; canonicalization does not
	// change that.)
	t.Run("with validator/create embed is an array", func(t *testing.T) {
		write := imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{embed: `[]`}, withValidator: true}
		client, err := write.run(t)
		assert.ErrorIs(t, err, ErrInvalidEmbed)
		write.assertNothingWritten(t, client)
	})
}
