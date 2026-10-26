//go:build integration

package jetstream

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestImagesWithAttachmentPostReferencesIndexed(t *testing.T) {
	db := testkit.DB(t)
	fixture := newPostBlobFixture(t, db)
	const recordEmbed = `{"$type":"social.coves.embed.imagesWithAttachment","images":{"$type":"social.coves.embed.images","images":[{"alt":"gallery","image":{"$type":"blob","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"mimeType":"image/jpeg","size":10}}]},"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://example.invalid/article","title":"Nested card","thumb":{"$type":"blob","ref":{"$link":"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"},"mimeType":"image/png","size":20},"images":[{"alt":"external gallery","image":{"$type":"blob","ref":{"$link":"bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"},"mimeType":"image/jpeg","size":30}}]}},"vendor":"retained verbatim"}`
	var embed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(recordEmbed), &embed))
	uri := createPostBlob(t, fixture, pv2Author, "nestedattachment", testkit.TID(), time.Now().UnixMicro(), embed)
	var stored string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT embed::text FROM posts WHERE uri = $1`, uri).Scan(&stored))
	require.JSONEq(t, recordEmbed, stored, "firehose accepts and stores the authored composite verbatim")
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:pv2author", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image", true},
		{"did:plc:pv2author", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "external_thumb", true},
		{"did:plc:pv2author", "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "post_image", true},
	}, readPostBlobRows(t, db, uri))
}

func TestImagesWithAttachmentCommentReferencesIndexed(t *testing.T) {
	db := testkit.DB(t)
	postURI, postCID := setupCommentThread(t, db)
	consumer := newCommentBlobConsumer(db)
	const recordEmbed = `{"$type":"social.coves.embed.imagesWithAttachment","images":{"$type":"social.coves.embed.images","images":[{"alt":"comment gallery","image":{"$type":"blob","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"mimeType":"image/jpeg","size":10}}]},"attachment":{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:quoted/social.coves.community.postv2/3kquoted","cid":"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},"vendor":"retained verbatim"}`
	var embed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(recordEmbed), &embed))
	uri := createCommentBlob(t, consumer, postURI, postCID, testkit.TID(), testkit.TID(), "bafcommentnestedattachment", time.Now().UnixMicro(), embed)
	var stored string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT embed::text FROM comments WHERE uri = $1`, uri).Scan(&stored))
	require.JSONEq(t, recordEmbed, stored, "firehose accepts and stores the authored composite verbatim")
	require.ElementsMatch(t, []postBlobRow{
		{"did:plc:brtestcommenter", "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image", true},
	}, readPostBlobRows(t, db, uri))
}
