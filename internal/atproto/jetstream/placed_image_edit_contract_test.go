//go:build integration

package jetstream

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestRemovedPlacedImagePostEditBlocksNewCID(t *testing.T) {
	requireRemovedPlacedImageEditBlock(t, false)
}

func TestRemovedPlacedImageCommentEditBlocksNewCID(t *testing.T) {
	requireRemovedPlacedImageEditBlock(t, true)
}

func requireRemovedPlacedImageEditBlock(t *testing.T, comment bool) {
	t.Helper()
	h := newEditMediaHarness(t, true)
	originalImage := editMediaCID(t.Name() + " original image")
	addedImage := editMediaCID(t.Name() + " added image")
	require.NotEqual(t, originalImage, addedImage)
	revisions := increasingTIDs(t, 2)
	started := time.Now().Add(-time.Minute)
	postRkey := testkit.TID()
	post := moderation.StrongRef{
		URI: pv2URI(pv2Author, postRkey), CID: editMediaCID(t.Name() + " original post record"),
	}
	initial := postModerationRecord(originalImage)
	initial["content"] = "[image]"
	initial["facets"] = editPlacedImageFacets(false)
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey,
		revisions[0], post.CID, started.UnixMicro(), initial)))
	subject := post
	rkey := postRkey
	if comment {
		rkey = testkit.TID()
		subject = moderation.StrongRef{
			URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + rkey,
			CID: editMediaCID(t.Name() + " original comment record"),
		}
		event := editMediaCommentEvent(post, "create", rkey, revisions[0], subject.CID, started, originalImage)
		event.Commit.Record["content"] = "[image]"
		event.Commit.Record["facets"] = editPlacedImageFacets(false)
		require.NoError(t, h.comments.HandleEvent(t.Context(), event))
	}
	removed := h.remove(t, subject, "v0")
	editedCID := editMediaCID(t.Name() + " edited record")
	if comment {
		event := editMediaCommentEvent(post, "update", rkey, revisions[1], editedCID,
			started.Add(time.Second), originalImage, addedImage)
		event.Commit.Record["content"] = "[image] [image]"
		event.Commit.Record["facets"] = editPlacedImageFacets(true)
		require.NoError(t, h.comments.HandleEvent(t.Context(), event))
	} else {
		edited := postModerationRecord(originalImage, addedImage)
		edited["content"] = "[image] [image]"
		edited["facets"] = editPlacedImageFacets(true)
		require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", rkey,
			revisions[1], editedCID, started.Add(time.Second).UnixMicro(), edited)))
	}

	// Prove this was an applied edit before inspecting its reconciliation.
	query := `SELECT cid, embed::text FROM posts WHERE uri = $1`
	if comment {
		query = `SELECT cid, embed::text FROM comments WHERE uri = $1`
	}
	var indexedCID, indexedEmbed string
	require.NoError(t, h.db.QueryRowContext(t.Context(), query, subject.URI).Scan(&indexedCID, &indexedEmbed))
	require.Equal(t, editedCID, indexedCID, "the removed subject's author edit must be indexed")
	var embed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(indexedEmbed), &embed))
	images, ok := embed["images"].([]interface{})
	require.True(t, ok)
	require.Len(t, images, 2, "the indexed edit must contain the added image")
	added, ok := images[1].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, map[string]interface{}{
		"$type": "blob", "ref": map[string]interface{}{"$link": addedImage},
		"mimeType": "image/png", "size": float64(10),
	}, added["image"])

	rows, err := h.db.QueryContext(t.Context(), `SELECT COALESCE(owner_did, ''), action_id
		FROM moderation_media_blocks WHERE blob_cid = $1 AND active`, addedImage)
	require.NoError(t, err)
	defer rows.Close()
	type block struct{ Owner, Action string }
	var blocks []block
	for rows.Next() {
		var value block
		require.NoError(t, rows.Scan(&value.Owner, &value.Action))
		blocks = append(blocks, value)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []block{{Owner: pv2Author, Action: removed.Action.ID}}, blocks,
		"the added CID must have an active owner-scoped block belonging to this removal")
}

func editPlacedImageFacets(twoImages bool) []interface{} {
	facets := []interface{}{map[string]interface{}{
		"$type":    "social.coves.richtext.facet",
		"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 7},
		"features": []interface{}{map[string]interface{}{"$type": "social.coves.richtext.facet#image", "index": 0}},
	}}
	if twoImages {
		facets = append(facets, map[string]interface{}{
			"$type":    "social.coves.richtext.facet",
			"index":    map[string]interface{}{"byteStart": 8, "byteEnd": 15},
			"features": []interface{}{map[string]interface{}{"$type": "social.coves.richtext.facet#image", "index": 1}},
		})
	}
	return facets
}
