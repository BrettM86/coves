package comments

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentImagesWithAttachmentTooManyImagesRejected(t *testing.T) {
	cases := []imageCheckWrite{
		{lexiconCommentWrite: lexiconCommentWrite{name: "create nine images", embed: commentCompositeEmbed(lexiconNineImageEmbed, lexiconQuotedPostEmbed)}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create count precedes malformed blob", embed: commentCompositeEmbed(lexiconNineImageEmbedOneMalformed, lexiconQuotedPostEmbed)}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update nine images before stored record fetch", update: true, stored: lexiconStoredComment, embed: commentCompositeEmbed(lexiconNineImageEmbed, lexiconQuotedPostEmbed)}, getRecordFails: true},
	}
	for _, write := range cases {
		t.Run(write.name, func(t *testing.T) {
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrTooManyImages)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			write.assertNothingWritten(t, client)
		})
	}
	t.Run("eight images fit without validator", func(t *testing.T) {
		embed := commentCompositeEmbed(lexiconEightImageEmbed, lexiconQuotedPostEmbed)
		write := imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{embed: embed}}
		client, err := write.run(t)
		require.NoError(t, err)
		assert.Equal(t, lexiconCommentObject(t, embed), writtenComment(t, client).Embed)
	})
}

func TestCommentImagesWithAttachmentCreateAccepted(t *testing.T) {
	const facets = `[{"index":{"byteStart":2,"byteEnd":6},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
	embed := commentCompositeEmbed(lexiconTwoImageEmbed, lexiconQuotedPostEmbed)
	write := imageCheckWrite{
		lexiconCommentWrite: lexiconCommentWrite{content: "é📷", facets: facets, embed: embed},
		withValidator:       true,
	}
	client, err := write.run(t)
	require.NoError(t, err)
	written := writtenComment(t, client)
	assert.Equal(t, "é📷", written.Content)
	assert.Equal(t, lexiconCommentObject(t, embed), written.Embed)
	assert.Equal(t, lexiconCommentFacets(t, facets), written.Facets)
}

func TestCommentImagesWithAttachmentInvalidImageFacetRejected(t *testing.T) {
	const facets = `[{"index":{"byteStart":0,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#image","index":2}]}]`
	embed := commentCompositeEmbed(lexiconTwoImageEmbed, lexiconQuotedPostEmbed)
	cases := []imageCheckWrite{
		{lexiconCommentWrite: lexiconCommentWrite{name: "create without validator", content: lexiconImageFacetContent, facets: facets, embed: embed}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update without validator before stored record fetch", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: facets, embed: embed}, getRecordFails: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create with validator", content: lexiconImageFacetContent, facets: facets, embed: embed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update with validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: facets, embed: embed}, withValidator: true},
	}
	for _, write := range cases {
		t.Run(write.name, func(t *testing.T) {
			client, err := write.run(t)
			assert.ErrorIs(t, err, ErrInvalidImageFacet)
			assert.NotErrorIs(t, err, ErrInvalidEmbed)
			assert.NotErrorIs(t, err, ErrInvalidFacets)
			write.assertNothingWritten(t, client)
		})
	}
}

func TestCommentImagesWithAttachmentUpdateAccepted(t *testing.T) {
	const facets = `[{"index":{"byteStart":4,"byteEnd":8},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
	embed := commentCompositeEmbed(lexiconTwoImageEmbed, lexiconQuotedPostEmbed)
	write := imageCheckWrite{
		lexiconCommentWrite: lexiconCommentWrite{update: true, stored: lexiconStoredComment, content: "New 📷", facets: facets, embed: embed},
		withValidator:       true,
	}
	client, err := write.run(t)
	require.NoError(t, err)
	written := writtenComment(t, client)
	assert.Equal(t, "New 📷", written.Content)
	assert.Equal(t, lexiconCommentObject(t, embed), written.Embed)
	assert.Equal(t, lexiconCommentFacets(t, facets), written.Facets)
	assert.Equal(t, lexiconCommentReply(), written.Reply)
	assert.Equal(t, "2026-10-01T12:00:00Z", written.CreatedAt)
}
