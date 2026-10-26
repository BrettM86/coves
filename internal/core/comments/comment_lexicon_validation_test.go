package comments

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/validation"
)

// With the embedded lexicon validator wired in, the comment service checks the
// FINAL social.coves.community.comment record before it reaches the author's
// PDS, on both create and update. An embed that is not a lexicon-valid
// social.coves.embed.images or social.coves.embed.post is refused as
// ErrInvalidEmbed; any other lexicon failure of the final record (for an
// update, the record merged with the stored reply and createdAt) is a client
// validation error. A refused create writes nothing and a refused update leaves
// the stored record untouched. Lexicon-valid records, including every shape
// current clients send, are written unchanged.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	lexiconCommentAuthorDID = "did:plc:lexcommentauthor2345abc"
	lexiconCommentEditRkey  = "3m2lexcommentedt"
	lexiconCommentEditURI   = "at://" + lexiconCommentAuthorDID + "/" + commentCollection + "/" + lexiconCommentEditRkey

	lexiconCommentRootURI = "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot"
	lexiconCommentRootCID = "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"

	// lexiconStoredComment is a lexicon-valid comment as the author's PDS holds
	// it, so an update that is refused can only have been refused for what the
	// update itself carries.
	lexiconStoredComment = `{
		"$type": "social.coves.community.comment",
		"content": "Original comment",
		"reply": {
			"root": {"uri": "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", "cid": "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"},
			"parent": {"uri": "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", "cid": "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"}
		},
		"createdAt": "2026-10-01T12:00:00Z"
	}`

	// lexiconStoredCommentStaleCreatedAt is lexiconStoredComment with a
	// preserved createdAt that is not a datetime: only the MERGED update record
	// carries it.
	lexiconStoredCommentStaleCreatedAt = `{
		"$type": "social.coves.community.comment",
		"content": "Original comment",
		"reply": {
			"root": {"uri": "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", "cid": "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"},
			"parent": {"uri": "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", "cid": "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"}
		},
		"createdAt": "yesterday"
	}`

	// lexiconOneImageEmbed is a lexicon-valid single-image embed.
	lexiconOneImageEmbed = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"A harbour at dusk"}
	]}`

	// lexiconEightImageEmbed is a lexicon-valid embed at the eight-image
	// maximum, with an omitted alt and aspect ratios as clients send them.
	lexiconEightImageEmbed = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"First","aspectRatio":{"width":1200,"height":800}},
		{"image":{"$type":"blob","ref":{"$link":"bafkreib5dipcpf4rdg2pvav7orgswyp63cyxhe4arsnzwpudvjavyzw4fm"},"mimeType":"image/png","size":120044},"alt":"Second"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreihgbhkwamntj75ssfrilr3agdhzqta3qu7ytt5ycrtyjmjeo77twe"},"mimeType":"image/webp","size":9001}},
		{"image":{"$type":"blob","ref":{"$link":"bafkreifsqyectzpd7jlbf45k7hlzgop6ncmtj7ius5kcgq6cnpqi43adki"},"mimeType":"image/gif","size":250000},"alt":"Fourth"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreiezwcsghegxciwyhts3btg2aesmghg6rvxlcxve2snveyzyewqqly"},"mimeType":"image/jpeg","size":1},"alt":"Fifth"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreifh73aqwbulq5vrgkzualeqf3rupx6eb2bsuxio3ftmaiwvffrrne"},"mimeType":"image/jpeg","size":10000000},"alt":"Sixth","aspectRatio":{"width":1,"height":1}},
		{"image":{"$type":"blob","ref":{"$link":"bafkreievvwh6osab7xfx6bnrlrmeuankaljgcx4yesexsbb6wlyjdlb7nu"},"mimeType":"image/png","size":77777},"alt":"Seventh"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreiddo4hn7eykg3ts7wquhof4ctskwzwk6nt52ta5lfsvfllmtgf6q4"},"mimeType":"image/jpeg","size":31337},"alt":"Eighth"}
	]}`

	// lexiconQuotedPostEmbed is a lexicon-valid quoted-post embed.
	lexiconQuotedPostEmbed = `{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:lexcommentquotedauthor/social.coves.community.post/3m2lexcommentquote","cid":"bafyreibzohnz4y676vq5u2ycuplmu766phrlhkja37fbg3z2m7xli4wbxy"}}`

	// lexiconLinkContent and lexiconLinkFacets are the content and link facet
	// shape current web and mobile clients send ("https://example.com/guide"
	// spans bytes 5-30).
	lexiconLinkContent = "Read https://example.com/guide now"
	lexiconLinkFacets  = `[{"index":{"byteStart":5,"byteEnd":30},"features":[{"$type":"social.coves.richtext.facet#link","uri":"https://example.com/guide"}]}]`
)

// lexiconCommentJSON decodes a JSON literal into the generic shape the comment
// handler hands the service (maps, []interface{}, float64).
func lexiconCommentJSON(t *testing.T, literal string) interface{} {
	t.Helper()
	var v interface{}
	require.NoErrorf(t, json.Unmarshal([]byte(literal), &v), "test literal is not JSON: %s", literal)
	return v
}

// lexiconCommentObject is lexiconCommentJSON for an object literal.
func lexiconCommentObject(t *testing.T, literal string) map[string]interface{} {
	t.Helper()
	obj, ok := lexiconCommentJSON(t, literal).(map[string]interface{})
	require.Truef(t, ok, "test literal is not a JSON object: %s", literal)
	return obj
}

// lexiconCommentFacets is lexiconCommentJSON for an array literal.
func lexiconCommentFacets(t *testing.T, literal string) []interface{} {
	t.Helper()
	arr, ok := lexiconCommentJSON(t, literal).([]interface{})
	require.Truef(t, ok, "test literal is not a JSON array: %s", literal)
	return arr
}

// lexiconCommentReply is a lexicon-valid reply to the root post.
func lexiconCommentReply() ReplyRef {
	return ReplyRef{
		Root:   StrongRef{URI: lexiconCommentRootURI, CID: lexiconCommentRootCID},
		Parent: StrongRef{URI: lexiconCommentRootURI, CID: lexiconCommentRootCID},
	}
}

// newLexiconCommentService builds a comment service with the embedded lexicon
// validator over an in-memory author PDS.
func newLexiconCommentService(t *testing.T) (Service, *mockPDSClient) {
	t.Helper()
	v, err := validation.NewEmbeddedLexiconValidator(false)
	require.NoError(t, err)
	return newMockPDSCommentService(WithLexiconValidator(v))
}

// newUnvalidatedCommentService builds the same comment service with no lexicon
// validator wired in.
func newUnvalidatedCommentService(t *testing.T) (Service, *mockPDSClient) {
	t.Helper()
	return newMockPDSCommentService()
}

// newMockPDSCommentService builds a comment service over an in-memory author
// PDS with the given options.
func newMockPDSCommentService(opts ...CommentServiceOption) (Service, *mockPDSClient) {
	client := newMockPDSClient(lexiconCommentAuthorDID)
	factory := &mockPDSClientFactory{client: client}
	service := NewCommentServiceWithPDSFactory(
		newMockCommentRepo(),
		newMockUserRepo(),
		newMockPostRepo(),
		newMockCommunityRepo(),
		nil,
		factory.create,
		opts...,
	)
	return service, client
}

// seedStoredComment places a decoded copy of storedLiteral in the author's PDS
// under the edit rkey.
func seedStoredComment(t *testing.T, client *mockPDSClient, storedLiteral string) {
	t.Helper()
	client.records[commentCollection] = map[string]interface{}{
		lexiconCommentEditRkey: lexiconCommentObject(t, storedLiteral),
	}
}

// writtenComment returns the single comment record a successful write left in
// the author's PDS.
func writtenComment(t *testing.T, client *mockPDSClient) CommentRecord {
	t.Helper()
	stored := client.records[commentCollection]
	require.Len(t, stored, 1, "exactly one comment record in the author's PDS")
	for _, record := range stored {
		written, ok := record.(CommentRecord)
		require.Truef(t, ok, "written record is a CommentRecord, got %T", record)
		return written
	}
	return CommentRecord{}
}

// lexiconCommentWrite is one create or update submission. For an update, the
// stored comment literal is seeded first.
type lexiconCommentWrite struct {
	name   string
	update bool
	stored string // update only
	embed  string // JSON literal; "" sends no embed
	langs  []string
	// content is the submitted content; "" sends "A comment" on create and
	// "Edited comment" on update.
	content string
	facets  string // JSON array literal; "" sends no facets
}

func (w lexiconCommentWrite) submit(t *testing.T, service Service) error {
	t.Helper()
	var embed interface{}
	if w.embed != "" {
		embed = lexiconCommentJSON(t, w.embed)
	}
	var facets []interface{}
	if w.facets != "" {
		facets = lexiconCommentFacets(t, w.facets)
	}
	session := createTestSession(lexiconCommentAuthorDID)
	if w.update {
		content := w.content
		if content == "" {
			content = "Edited comment"
		}
		_, err := service.UpdateComment(context.Background(), session, UpdateCommentRequest{
			URI:     lexiconCommentEditURI,
			Content: content,
			Facets:  facets,
			Embed:   embed,
			Langs:   w.langs,
		})
		return err
	}
	content := w.content
	if content == "" {
		content = "A comment"
	}
	_, err := service.CreateComment(context.Background(), session, CreateCommentRequest{
		Reply:   lexiconCommentReply(),
		Content: content,
		Facets:  facets,
		Embed:   embed,
		Langs:   w.langs,
	})
	return err
}

// assertNothingWritten checks the author's PDS after a refused write: a
// refused create leaves no comment record, a refused update leaves the stored
// record exactly as seeded.
func (w lexiconCommentWrite) assertNothingWritten(t *testing.T, client *mockPDSClient) {
	t.Helper()
	if !w.update {
		assert.Empty(t, client.records[commentCollection], "a refused create writes no comment record")
		return
	}
	assert.Equal(t,
		map[string]interface{}{lexiconCommentEditRkey: lexiconCommentObject(t, w.stored)},
		client.records[commentCollection],
		"a refused update leaves the stored comment unchanged")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCommentWrite_InvalidEmbedRejected(t *testing.T) {
	const imageWithoutBlob = `{"$type":"social.coves.embed.images","images":[{"alt":"A harbour at dusk"}]}`
	const externalEmbed = `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/article","title":"An article","description":"About the article"}}`

	cases := []lexiconCommentWrite{
		{name: "create images entry without image blob", embed: imageWithoutBlob},
		{name: "create quoted post without cid", embed: `{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:lexcommentquotedauthor/social.coves.community.post/3m2lexcommentquote"}}`},
		{name: "create embed is a JSON string", embed: `"social.coves.embed.images"`},
		{name: "create embed object without $type", embed: `{"images":[{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213}}]}`},
		{name: "update images entry without image blob", update: true, stored: lexiconStoredComment, embed: imageWithoutBlob},
		{name: "create external embed", embed: externalEmbed},
		{name: "update external embed", update: true, stored: lexiconStoredComment, embed: externalEmbed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, client := newLexiconCommentService(t)
			if tc.update {
				seedStoredComment(t, client, tc.stored)
			}

			err := tc.submit(t, service)

			assert.Truef(t, errors.Is(err, ErrInvalidEmbed), "want ErrInvalidEmbed, got %v", err)
			tc.assertNothingWritten(t, client)
		})
	}
}

func TestCommentWrite_LexiconInvalidRecordIsValidationError(t *testing.T) {
	cases := []lexiconCommentWrite{
		{name: "create valid embed with invalid language", embed: lexiconOneImageEmbed, langs: []string{"not a language!"}},
		{name: "update merged with stored non-datetime createdAt", update: true, stored: lexiconStoredCommentStaleCreatedAt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, client := newLexiconCommentService(t)
			if tc.update {
				seedStoredComment(t, client, tc.stored)
			}

			err := tc.submit(t, service)

			assert.Truef(t, IsValidationError(err), "want a client validation error, got %v", err)
			assert.Falsef(t, errors.Is(err, ErrInvalidEmbed), "a non-embed lexicon failure is not ErrInvalidEmbed, got %v", err)
			tc.assertNothingWritten(t, client)
		})
	}
}

// TestCommentWrite_LexiconValidRecordsAccepted guards the validation against
// false rejections: valid embeds and current client shapes are written as
// submitted.
func TestCommentWrite_LexiconValidRecordsAccepted(t *testing.T) {
	t.Run("create with eight images", func(t *testing.T) {
		service, client := newLexiconCommentService(t)

		_, err := service.CreateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), CreateCommentRequest{
			Reply:   lexiconCommentReply(),
			Content: "Eight photos from the trip",
			Embed:   lexiconCommentJSON(t, lexiconEightImageEmbed),
			Langs:   []string{"en"},
		})

		require.NoError(t, err)
		assert.Equal(t, lexiconCommentObject(t, lexiconEightImageEmbed), writtenComment(t, client).Embed)
	})

	t.Run("create with quoted post", func(t *testing.T) {
		service, client := newLexiconCommentService(t)

		_, err := service.CreateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), CreateCommentRequest{
			Reply:   lexiconCommentReply(),
			Content: "Quoting this",
			Embed:   lexiconCommentJSON(t, lexiconQuotedPostEmbed),
		})

		require.NoError(t, err)
		assert.Equal(t, lexiconCommentObject(t, lexiconQuotedPostEmbed), writtenComment(t, client).Embed)
	})

	t.Run("update with eight images", func(t *testing.T) {
		service, client := newLexiconCommentService(t)
		seedStoredComment(t, client, lexiconStoredComment)

		_, err := service.UpdateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), UpdateCommentRequest{
			URI:     lexiconCommentEditURI,
			Content: "Eight photos from the trip",
			Embed:   lexiconCommentJSON(t, lexiconEightImageEmbed),
			Langs:   []string{"en"},
		})

		require.NoError(t, err)
		assert.Equal(t, lexiconCommentObject(t, lexiconEightImageEmbed), writtenComment(t, client).Embed)
	})

	t.Run("update with quoted post", func(t *testing.T) {
		service, client := newLexiconCommentService(t)
		seedStoredComment(t, client, lexiconStoredComment)

		_, err := service.UpdateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), UpdateCommentRequest{
			URI:     lexiconCommentEditURI,
			Content: "Quoting this",
			Embed:   lexiconCommentJSON(t, lexiconQuotedPostEmbed),
		})

		require.NoError(t, err)
		assert.Equal(t, lexiconCommentObject(t, lexiconQuotedPostEmbed), writtenComment(t, client).Embed)
	})

	t.Run("create without embed with link facet", func(t *testing.T) {
		service, client := newLexiconCommentService(t)

		_, err := service.CreateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), CreateCommentRequest{
			Reply:   lexiconCommentReply(),
			Content: lexiconLinkContent,
			Facets:  lexiconCommentFacets(t, lexiconLinkFacets),
		})

		require.NoError(t, err)
		written := writtenComment(t, client)
		assert.Equal(t, "Read https://example.com/guide now", written.Content)
		assert.Equal(t, lexiconCommentFacets(t, lexiconLinkFacets), written.Facets)
		assert.Nil(t, written.Embed)
	})

	t.Run("update without embed with link facet", func(t *testing.T) {
		service, client := newLexiconCommentService(t)
		seedStoredComment(t, client, lexiconStoredComment)

		_, err := service.UpdateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), UpdateCommentRequest{
			URI:     lexiconCommentEditURI,
			Content: lexiconLinkContent,
			Facets:  lexiconCommentFacets(t, lexiconLinkFacets),
		})

		require.NoError(t, err)
		written := writtenComment(t, client)
		assert.Equal(t, "Read https://example.com/guide now", written.Content)
		assert.Equal(t, lexiconCommentFacets(t, lexiconLinkFacets), written.Facets)
		assert.Nil(t, written.Embed)
		assert.Equal(t, ReplyRef{
			Root:   StrongRef{URI: "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", CID: "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"},
			Parent: StrongRef{URI: "at://did:plc:lexcommentpostauthor234/social.coves.community.post/3m2lexcommentroot", CID: "bafyreiglod7oajufkjezuzvum6qoq6epg34443qwt4q4xwvgspgzj3vra4"},
		}, written.Reply)
		assert.Equal(t, "2026-10-01T12:00:00Z", written.CreatedAt)
	})
}

// ---------------------------------------------------------------------------
// Image blob checks and the eight-image cap
// ---------------------------------------------------------------------------

// Whether or not a lexicon validator is wired in, a comment's images embed is
// held to the same rules as a post's: at most eight images (ErrTooManyImages,
// never ErrInvalidEmbed), and every image a sha2-256 blob of an accepted image
// type within the size limit (ErrInvalidEmbed). Both checks run on the
// submitted embed before the stored record of an update is fetched.

const (
	// lexiconNineImageEmbed is one image over the eight-image maximum; every
	// entry is otherwise valid.
	lexiconNineImageEmbed = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"1"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"2"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"3"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"4"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"5"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"6"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"7"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"8"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"9"}
	]}`

	// lexiconNineImageEmbedOneMalformed is nine entries, the fifth of which has
	// no image blob: the count is refused before any entry's shape.
	lexiconNineImageEmbedOneMalformed = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"1"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"2"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"3"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"4"},
		{"alt":"5"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"6"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"7"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"8"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"9"}
	]}`
)

// imageCheckWrite is one write submitted to a service with or without the
// lexicon validator, optionally while the author's PDS fails every GetRecord.
type imageCheckWrite struct {
	lexiconCommentWrite
	withValidator  bool
	getRecordFails bool
}

// run submits the write and returns the author's PDS and the error.
func (w imageCheckWrite) run(t *testing.T) (*mockPDSClient, error) {
	t.Helper()
	service, client := newUnvalidatedCommentService(t)
	if w.withValidator {
		service, client = newLexiconCommentService(t)
	}
	if w.update {
		seedStoredComment(t, client, w.stored)
	}
	if w.getRecordFails {
		client.getError = pds.ErrNotFound
	}
	return client, w.submit(t, service)
}

func TestCommentWrite_TooManyImagesRejected(t *testing.T) {
	cases := []imageCheckWrite{
		{lexiconCommentWrite: lexiconCommentWrite{name: "create nine images with validator", embed: lexiconNineImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create nine entries one without image with validator", embed: lexiconNineImageEmbedOneMalformed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create nine images without validator", embed: lexiconNineImageEmbed}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update nine images with validator", update: true, stored: lexiconStoredComment, embed: lexiconNineImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update nine images without validator", update: true, stored: lexiconStoredComment, embed: lexiconNineImageEmbed}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update nine images without validator while stored record unreadable", update: true, stored: lexiconStoredComment, embed: lexiconNineImageEmbed}, getRecordFails: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.run(t)

			assert.Truef(t, errors.Is(err, ErrTooManyImages), "want ErrTooManyImages, got %v", err)
			assert.Falsef(t, errors.Is(err, ErrInvalidEmbed), "too many images is not ErrInvalidEmbed, got %v", err)
			tc.assertNothingWritten(t, client)
		})
	}
}

func TestCommentWrite_InvalidImageBlobRejected(t *testing.T) {
	const (
		imageNotBlobTyped = `{"$type":"social.coves.embed.images","images":[
			{"image":{"ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"A harbour at dusk"}
		]}`
		imageRefNotCID = `{"$type":"social.coves.embed.images","images":[
			{"image":{"$type":"blob","ref":{"$link":"not-a-cid"},"mimeType":"image/jpeg","size":48213},"alt":"A harbour at dusk"}
		]}`
		// bafkqaaa is a decodable CIDv1 (raw codec) whose multihash is the
		// identity hash, not sha2-256.
		imageRefIdentityHashCID = `{"$type":"social.coves.embed.images","images":[
			{"image":{"$type":"blob","ref":{"$link":"bafkqaaa"},"mimeType":"image/jpeg","size":48213},"alt":"A harbour at dusk"}
		]}`
		imageSVG = `{"$type":"social.coves.embed.images","images":[
			{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/svg+xml","size":48213},"alt":"A harbour at dusk"}
		]}`
		imageOverSizeLimit = `{"$type":"social.coves.embed.images","images":[
			{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":10000001},"alt":"A harbour at dusk"}
		]}`
	)

	cases := []imageCheckWrite{
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image without blob $type without validator", embed: imageNotBlobTyped}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image ref not a CID without validator", embed: imageRefNotCID}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image ref CID not sha2-256 with validator", embed: imageRefIdentityHashCID}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image svg without validator", embed: imageSVG}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image over size limit without validator", embed: imageOverSizeLimit}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update image svg without validator", update: true, stored: lexiconStoredComment, embed: imageSVG}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update image ref not a CID without validator while stored record unreadable", update: true, stored: lexiconStoredComment, embed: imageRefNotCID}, getRecordFails: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.run(t)

			assert.Truef(t, errors.Is(err, ErrInvalidEmbed), "want ErrInvalidEmbed, got %v", err)
			tc.assertNothingWritten(t, client)
		})
	}
}

// TestCommentWrite_ImagesAcceptedWithoutValidator guards the image checks
// against false rejections on a service with no lexicon validator: eight valid
// images are written as submitted.
func TestCommentWrite_ImagesAcceptedWithoutValidator(t *testing.T) {
	service, client := newUnvalidatedCommentService(t)

	_, err := service.CreateComment(context.Background(), createTestSession(lexiconCommentAuthorDID), CreateCommentRequest{
		Reply:   lexiconCommentReply(),
		Content: "Eight photos from the trip",
		Embed:   lexiconCommentJSON(t, lexiconEightImageEmbed),
	})

	require.NoError(t, err)
	assert.Equal(t, lexiconCommentObject(t, lexiconEightImageEmbed), writtenComment(t, client).Embed)
}

// ---------------------------------------------------------------------------
// #image facet placement rules
// ---------------------------------------------------------------------------

// A comment's #image facets are held to the same placement rules as a post's,
// whether or not a lexicon validator is wired in: each #image facet is the only
// feature in its facet, covers a non-empty range inside the content and outside
// any #code range, and names a distinct image of the comment's images embed. A
// facet that breaks a rule is refused as ErrInvalidImageFacet (never
// ErrInvalidFacets or ErrInvalidEmbed) and nothing is written. A broken images
// embed is reported as the embed error even when an #image facet is also
// broken. On update the rules apply to the submitted facets and embed before
// the stored record is fetched.

const (
	// lexiconImageFacetContent is the content #image facet rows annotate (12
	// bytes).
	lexiconImageFacetContent = "Look at this"

	// lexiconTwoImageEmbed is a lexicon-valid two-image embed.
	lexiconTwoImageEmbed = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"1"},
		{"image":{"$type":"blob","ref":{"$link":"bafkreid2fkw4d6a45m3xsmwpddnydzfa3eiacxp5u4ezt6zt4h7hsuepzi"},"mimeType":"image/jpeg","size":48213},"alt":"2"}
	]}`

	// lexiconImageEmbedRefNotCID is a single image whose blob ref is not a CID.
	lexiconImageEmbedRefNotCID = `{"$type":"social.coves.embed.images","images":[
		{"image":{"$type":"blob","ref":{"$link":"not-a-cid"},"mimeType":"image/jpeg","size":48213},"alt":"1"}
	]}`

	// lexiconImageFacetIndexPastCount names image 5 of a two-image embed.
	lexiconImageFacetIndexPastCount = `[{"index":{"byteStart":0,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#image","index":5}]}]`

	// lexiconImageFacetZeroLength places image 0 on the empty range [4, 4).
	lexiconImageFacetZeroLength = `[{"index":{"byteStart":4,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`

	// lexiconImageFacetPastEnd places image 0 on [8, 20) of 12-byte content.
	lexiconImageFacetPastEnd = `[{"index":{"byteStart":8,"byteEnd":20},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`
)

func TestCommentWrite_InvalidImageFacetRejected(t *testing.T) {
	const (
		sharedWithLink = `[{"index":{"byteStart":0,"byteEnd":4},"features":[
			{"$type":"social.coves.richtext.facet#image","index":0},
			{"$type":"social.coves.richtext.facet#link","uri":"https://example.com/guide"}
		]}]`
		duplicateIndex = `[
			{"index":{"byteStart":0,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]},
			{"index":{"byteStart":5,"byteEnd":7},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}
		]`
		insideCode = `[
			{"index":{"byteStart":0,"byteEnd":12},"features":[{"$type":"social.coves.richtext.facet#code"}]},
			{"index":{"byteStart":5,"byteEnd":7},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}
		]`
	)

	cases := []imageCheckWrite{
		// Create (B1).
		{lexiconCommentWrite: lexiconCommentWrite{name: "create index past image count with validator", content: lexiconImageFacetContent, facets: lexiconImageFacetIndexPastCount, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image facet shared with link with validator", content: lexiconImageFacetContent, facets: sharedWithLink, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create duplicate image index with validator", content: lexiconImageFacetContent, facets: duplicateIndex, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image range inside code with validator", content: lexiconImageFacetContent, facets: insideCode, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create zero-length image range with validator", content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image range past content end with validator", content: lexiconImageFacetContent, facets: lexiconImageFacetPastEnd, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create image facet without embed with validator", content: lexiconImageFacetContent, facets: `[{"index":{"byteStart":0,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "create index past image count without validator", content: lexiconImageFacetContent, facets: lexiconImageFacetIndexPastCount, embed: lexiconTwoImageEmbed}},
		// Update (B2).
		{lexiconCommentWrite: lexiconCommentWrite{name: "update index past image count with validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetIndexPastCount, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update zero-length image range with validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update image range past content end with validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetPastEnd, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update index past image count without validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetIndexPastCount, embed: lexiconTwoImageEmbed}},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update index past image count without validator while stored record unreadable", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetIndexPastCount, embed: lexiconTwoImageEmbed}, getRecordFails: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.run(t)

			assert.Truef(t, errors.Is(err, ErrInvalidImageFacet), "want ErrInvalidImageFacet, got %v", err)
			assert.Falsef(t, errors.Is(err, ErrInvalidFacets), "an #image rule failure is not ErrInvalidFacets, got %v", err)
			assert.Falsef(t, errors.Is(err, ErrInvalidEmbed), "an #image rule failure is not ErrInvalidEmbed, got %v", err)
			tc.assertNothingWritten(t, client)
		})
	}
}

// TestCommentWrite_BrokenImageEmbedPrecedesImageFacet pins the order of the
// checks: a broken images embed is reported as the embed error, not as the
// #image facet that also breaks a rule.
func TestCommentWrite_BrokenImageEmbedPrecedesImageFacet(t *testing.T) {
	cases := []struct {
		write   imageCheckWrite
		wantErr error
	}{
		{write: imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{name: "create nine images with zero-length image facet with validator", content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconNineImageEmbed}, withValidator: true}, wantErr: ErrTooManyImages},
		{write: imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{name: "create image ref not a CID with zero-length image facet with validator", content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconImageEmbedRefNotCID}, withValidator: true}, wantErr: ErrInvalidEmbed},
		{write: imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{name: "update nine images with zero-length image facet with validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconNineImageEmbed}, withValidator: true}, wantErr: ErrTooManyImages},
		{write: imageCheckWrite{lexiconCommentWrite: lexiconCommentWrite{name: "update image ref not a CID with zero-length image facet without validator", update: true, stored: lexiconStoredComment, content: lexiconImageFacetContent, facets: lexiconImageFacetZeroLength, embed: lexiconImageEmbedRefNotCID}}, wantErr: ErrInvalidEmbed},
	}

	for _, tc := range cases {
		t.Run(tc.write.name, func(t *testing.T) {
			client, err := tc.write.run(t)

			assert.Truef(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
			assert.Falsef(t, errors.Is(err, ErrInvalidImageFacet), "a broken images embed is not ErrInvalidImageFacet, got %v", err)
			tc.write.assertNothingWritten(t, client)
		})
	}
}

// TestCommentWrite_ValidImageFacetAccepted guards the #image rules against
// false rejections: an #image facet over multibyte content that ends exactly
// at the content's byte length and names the second of two images is written
// as submitted.
func TestCommentWrite_ValidImageFacetAccepted(t *testing.T) {
	// "é📷" is 6 bytes: é is [0, 2), 📷 is [2, 6).
	const (
		content = "é📷"
		facets  = `[{"index":{"byteStart":2,"byteEnd":6},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`
	)

	cases := []imageCheckWrite{
		{lexiconCommentWrite: lexiconCommentWrite{name: "create", content: content, facets: facets, embed: lexiconTwoImageEmbed}, withValidator: true},
		{lexiconCommentWrite: lexiconCommentWrite{name: "update", update: true, stored: lexiconStoredComment, content: content, facets: facets, embed: lexiconTwoImageEmbed}, withValidator: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.run(t)

			require.NoError(t, err)
			written := writtenComment(t, client)
			assert.Equal(t, "é📷", written.Content)
			assert.Equal(t, lexiconCommentFacets(t, `[{"index":{"byteStart":2,"byteEnd":6},"features":[{"$type":"social.coves.richtext.facet#image","index":1}]}]`), written.Facets)
			assert.Equal(t, lexiconCommentObject(t, lexiconTwoImageEmbed), written.Embed)
		})
	}
}
