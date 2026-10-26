//go:build integration

package jetstream

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The only image is placed in the body: there is no separate gallery image
// that could accidentally supply the block asserted below.
func TestPlacedImageModerationRemoveRestore(t *testing.T) {
	for _, kind := range []string{"post", "comment"} {
		t.Run(kind, func(t *testing.T) {
			h := newEditMediaHarness(t, true)
			imageCID := editMediaCID("placed " + kind + " image")
			revisions := increasingTIDs(t, 1)
			createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			postRkey := testkit.TID()
			root := moderation.StrongRef{URI: pv2URI(pv2Author, postRkey), CID: editMediaCID("placed " + kind + " root record")}
			postRecord := postModerationRecord()
			facets := []interface{}{map[string]interface{}{
				"index": map[string]interface{}{"byteStart": 0, "byteEnd": 7},
				"features": []interface{}{map[string]interface{}{
					"$type": "social.coves.richtext.facet#image", "index": 0,
				}},
			}}
			if kind == "post" {
				postRecord = postModerationRecord(imageCID)
				postRecord["content"], postRecord["facets"] = "[image]", facets
			}
			require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey,
				revisions[0], root.CID, createdAt.UnixMicro(), postRecord)))
			subject, referenceKind := root, "post_image"
			if kind == "comment" {
				commentRkey := testkit.TID()
				subject = moderation.StrongRef{
					URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + commentRkey,
					CID: editMediaCID("placed comment record"),
				}
				event := editMediaCommentEvent(root, "create", commentRkey, revisions[0], subject.CID, createdAt, imageCID)
				event.Commit.Record["content"], event.Commit.Record["facets"] = "[image]", facets
				require.NoError(t, h.comments.HandleEvent(t.Context(), event))
				referenceKind = "comment_image"
			}
			wantedReferences := []postBlobRow{{owner: pv2Author, cid: imageCID, kind: referenceKind, active: true}}
			require.ElementsMatch(t, wantedReferences, readPostBlobRows(t, h.db, subject.URI), "indexed placed image has its active reference")

			removed, err := h.moderator.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "placedadmin")), moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-" + testkit.UniqueID(t),
				Reason: "social.coves.moderation.defs#reasonSpam",
			})
			require.NoError(t, err)
			require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
			require.NotNil(t, removed.Action)
			type blockRow struct{ owner, cid, action string }
			readBlocks := func() []blockRow {
				rows, err := h.db.QueryContext(t.Context(), `SELECT COALESCE(owner_did, ''), blob_cid, action_id FROM moderation_media_blocks WHERE active`)
				require.NoError(t, err)
				defer rows.Close()
				var found []blockRow
				for rows.Next() {
					var row blockRow
					require.NoError(t, rows.Scan(&row.owner, &row.cid, &row.action))
					found = append(found, row)
				}
				require.NoError(t, rows.Err())
				return found
			}
			require.ElementsMatch(t, []blockRow{{pv2Author, imageCID, removed.Action.ID}}, readBlocks(), "removal blocks exactly the placed image for its owner")
			require.ElementsMatch(t, wantedReferences, readPostBlobRows(t, h.db, subject.URI), "admin removal preserves active references")

			h.restore(t, subject, removed)
			require.Empty(t, readBlocks(), "restore lifts the placed image block")
			require.ElementsMatch(t, wantedReferences, readPostBlobRows(t, h.db, subject.URI), "restore preserves active references")
		})
	}
}
