//go:build integration

package jetstream

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Image-only comments: the consumer accepts a comment whose content is the raw
// string "" when the record's own embed carries at least one image. The raw
// comparison is kept (whitespace-only content stays indexed as sent), and a
// record with no content key, or content: null, stays a permanent rejection.

const (
	imageOnlyImagesEmbed = `{"$type":"social.coves.embed.images","images":[{"alt":"","image":{"$type":"blob","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"mimeType":"image/jpeg","size":10}}]}`
	imageOnlyEmptyImages = `{"$type":"social.coves.embed.images","images":[]}`
	imageOnlyIWAEmbed    = `{"$type":"social.coves.embed.imagesWithAttachment","images":{"$type":"social.coves.embed.images","images":[{"alt":"comment gallery","image":{"$type":"blob","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"mimeType":"image/jpeg","size":10}}]},"attachment":{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:quoted/social.coves.community.postv2/3kquoted","cid":"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}}}`
	imageOnlyQuoteEmbed  = `{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:quoted/social.coves.community.postv2/3kquoted","cid":"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}}`

	imageOnlyOriginalContent = "original text"
	imageOnlyOriginalCID     = "bafimageonlyoriginal"
	imageOnlyEventCID        = "bafimageonlyevent"
)

// imageOnlyContent describes the record's content key: absent, JSON null, or a string.
type imageOnlyContent struct {
	present bool
	value   interface{}
}

var (
	contentMissing = imageOnlyContent{}
	contentNull    = imageOnlyContent{present: true, value: nil}
)

func contentText(text string) imageOnlyContent {
	return imageOnlyContent{present: true, value: text}
}

type imageOnlyCase struct {
	name string
	// operation is "create" (fresh insert) or "update" (edit of an indexed
	// comment that has imageOnlyOriginalContent and no embed).
	operation string
	content   imageOnlyContent
	// embed is the record's embed as JSON; "" means the record has no embed.
	embed         string
	wantPermanent bool
	// wantContent is the indexed content when the event is accepted.
	wantContent string
}

type imageOnlyRow struct {
	content string
	cid     string
	embed   sql.NullString
}

func readImageOnlyRow(t *testing.T, db *sql.DB, uri string) imageOnlyRow {
	t.Helper()
	var row imageOnlyRow
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT content, cid, embed::text FROM comments WHERE uri = $1`, uri).
		Scan(&row.content, &row.cid, &row.embed))
	return row
}

// runImageOnlyCommentCases drives each case through HandleEvent against real
// Postgres. Every comment under test is a reply to a text parent comment on the
// thread post, so parent (comment reply_count) and root (post comment_count)
// counts are both observable.
func runImageOnlyCommentCases(t *testing.T, cases []imageOnlyCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			postURI, postCID := setupCommentThread(t, db)
			consumer := newCommentBlobConsumer(db)
			revs := increasingTIDs(t, 3)
			base := time.Now().UnixMicro()

			const parentCID = "bafimageonlyparent"
			parentRkey := testkit.TID()
			parentURI := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + parentRkey
			require.NoError(t, consumer.HandleEvent(t.Context(), revCommitEvent(bridgedTestCommenter, CommentCollection, "create",
				parentRkey, revs[0], parentCID, base, revCommentRecord("parent text", postURI, postCID, postURI, postCID))))

			rkey := testkit.TID()
			uri := "at://" + bridgedTestCommenter + "/" + CommentCollection + "/" + rkey
			if tc.operation == "update" {
				require.NoError(t, consumer.HandleEvent(t.Context(), revCommitEvent(bridgedTestCommenter, CommentCollection, "create",
					rkey, revs[1], imageOnlyOriginalCID, base+1_000_000,
					revCommentRecord(imageOnlyOriginalContent, postURI, postCID, parentURI, parentCID))))
			}

			record := revCommentRecord("placeholder", postURI, postCID, parentURI, parentCID)
			if tc.content.present {
				record["content"] = tc.content.value
			} else {
				delete(record, "content")
			}
			if tc.embed != "" {
				var embed map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(tc.embed), &embed))
				record["embed"] = embed
			}

			err := consumer.HandleEvent(t.Context(), revCommitEvent(bridgedTestCommenter, CommentCollection, tc.operation,
				rkey, revs[2], imageOnlyEventCID, base+2_000_000, record))

			if tc.wantPermanent {
				require.Truef(t, errors.Is(err, ErrPermanentEvent), "HandleEvent() error = %v, want ErrPermanentEvent", err)
				if tc.operation == "create" {
					require.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
					require.Equal(t, 1, countRows(t, db, `SELECT comment_count FROM posts WHERE uri = $1`, postURI))
					require.Equal(t, 0, countRows(t, db, `SELECT reply_count FROM comments WHERE uri = $1`, parentURI))
					return
				}
				row := readImageOnlyRow(t, db, uri)
				require.Equal(t, imageOnlyOriginalContent, row.content)
				require.Equal(t, imageOnlyOriginalCID, row.cid)
				require.False(t, row.embed.Valid, "rejected update must leave the embed absent")
				require.Equal(t, 2, countRows(t, db, `SELECT comment_count FROM posts WHERE uri = $1`, postURI))
				require.Equal(t, 1, countRows(t, db, `SELECT reply_count FROM comments WHERE uri = $1`, parentURI))
				return
			}

			require.NoError(t, err)
			row := readImageOnlyRow(t, db, uri)
			require.Equal(t, tc.wantContent, row.content)
			require.Equal(t, imageOnlyEventCID, row.cid)
			if tc.embed == "" {
				require.False(t, row.embed.Valid, "comment without an embed must store no embed")
			} else {
				require.True(t, row.embed.Valid, "comment embed must be stored")
				require.JSONEq(t, tc.embed, row.embed.String)
			}
			require.Equal(t, 2, countRows(t, db, `SELECT comment_count FROM posts WHERE uri = $1`, postURI))
			require.Equal(t, 1, countRows(t, db, `SELECT reply_count FROM comments WHERE uri = $1`, parentURI))
		})
	}
}

// B6: content "" with an image embed is indexed on create and applied on update.
func TestCommentConsumer_ImageOnly_EmptyContentWithImagesIndexed(t *testing.T) {
	runImageOnlyCommentCases(t, []imageOnlyCase{
		{name: "create with images embed", operation: "create", content: contentText(""), embed: imageOnlyImagesEmbed, wantContent: ""},
		{name: "create with imagesWithAttachment embed", operation: "create", content: contentText(""), embed: imageOnlyIWAEmbed, wantContent: ""},
		{name: "update text comment to image-only", operation: "update", content: contentText(""), embed: imageOnlyImagesEmbed, wantContent: ""},
	})
}

// B7 (guard): content "" without an image stays a permanent rejection.
func TestCommentConsumer_ImageOnly_EmptyContentWithoutImagesRejected(t *testing.T) {
	runImageOnlyCommentCases(t, []imageOnlyCase{
		{name: "create with no embed", operation: "create", content: contentText(""), wantPermanent: true},
		{name: "create with quoted post embed", operation: "create", content: contentText(""), embed: imageOnlyQuoteEmbed, wantPermanent: true},
		{name: "create with empty images array", operation: "create", content: contentText(""), embed: imageOnlyEmptyImages, wantPermanent: true},
		{name: "update with no embed", operation: "update", content: contentText(""), wantPermanent: true},
		{name: "update with quoted post embed", operation: "update", content: contentText(""), embed: imageOnlyQuoteEmbed, wantPermanent: true},
	})
}

// B8 (guard): whitespace-only content is not trimmed and stays indexed as sent.
func TestCommentConsumer_ImageOnly_WhitespaceContentIndexedAsSent(t *testing.T) {
	runImageOnlyCommentCases(t, []imageOnlyCase{
		{name: "create with images embed", operation: "create", content: contentText(" \t"), embed: imageOnlyImagesEmbed, wantContent: " \t"},
		{name: "create with no embed", operation: "create", content: contentText(" \t"), wantContent: " \t"},
		{name: "create with quoted post embed", operation: "create", content: contentText(" \t"), embed: imageOnlyQuoteEmbed, wantContent: " \t"},
	})
}

// B9: an image record with no content key, or content null, is permanently
// rejected; the same record with content "" is accepted.
func TestCommentConsumer_ImageOnly_MissingOrNullContentRejected(t *testing.T) {
	runImageOnlyCommentCases(t, []imageOnlyCase{
		{name: "create with content key missing", operation: "create", content: contentMissing, embed: imageOnlyImagesEmbed, wantPermanent: true},
		{name: "create with content null", operation: "create", content: contentNull, embed: imageOnlyImagesEmbed, wantPermanent: true},
		{name: "create with empty content", operation: "create", content: contentText(""), embed: imageOnlyImagesEmbed, wantContent: ""},
		{name: "update with content key missing", operation: "update", content: contentMissing, embed: imageOnlyImagesEmbed, wantPermanent: true},
		{name: "update with content null", operation: "update", content: contentNull, embed: imageOnlyImagesEmbed, wantPermanent: true},
		{name: "update with empty content", operation: "update", content: contentText(""), embed: imageOnlyImagesEmbed, wantContent: ""},
	})
}
