//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestImagesWithAttachmentPostRemovalBlocksAndRecordsAllMedia(t *testing.T) {
	requireImagesWithAttachmentModeration(t, false, false)
}

func TestImagesWithAttachmentCommentRemovalBlocksAndRecordsGallery(t *testing.T) {
	requireImagesWithAttachmentModeration(t, true, false)
}

func TestImagesWithAttachmentRemovedPostEditBlocksAndRecordsAddedMedia(t *testing.T) {
	requireImagesWithAttachmentModeration(t, false, true)
}

func TestImagesWithAttachmentRemovedCommentEditBlocksAndRecordsAddedImage(t *testing.T) {
	requireImagesWithAttachmentModeration(t, true, true)
}

type attachmentModerationCDNPurger struct {
	mu    sync.Mutex
	blobs []imageproxy.BlockedBlob
}

func (p *attachmentModerationCDNPurger) PurgeBlobs(_ context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	p.mu.Lock()
	p.blobs = append(p.blobs, blobs...)
	p.mu.Unlock()
	return imageproxy.CDNPurgeResult{Acknowledged: blobs}
}

func (p *attachmentModerationCDNPurger) recorded() []imageproxy.BlockedBlob {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]imageproxy.BlockedBlob(nil), p.blobs...)
}

func requireImagesWithAttachmentModeration(t *testing.T, comment, edit bool) {
	t.Helper()
	purger := &attachmentModerationCDNPurger{}
	var queue *moderation.CDNPurgeQueue
	h := newEditMediaHarnessWithTargets(t, true, func(db *sql.DB) moderation.CDNPurgeTargets {
		queue = moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(db), purger,
			moderation.CDNPurgeQueueConfig{WriteTimeout: 10 * time.Second})
		t.Cleanup(func() {
			queue.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, queue.Wait(ctx))
		})
		return queue
	})
	galleryOne := editMediaCID(t.Name() + " gallery one")
	galleryTwo := editMediaCID(t.Name() + " gallery two")
	thumb := editMediaCID(t.Name() + " external thumb")
	externalImage := editMediaCID(t.Name() + " external image")
	addedGallery := editMediaCID(t.Name() + " added gallery image")
	addedExternal := editMediaCID(t.Name() + " added external image")
	quotedImage := editMediaCID(t.Name() + " quoted post image")
	revisions := increasingTIDs(t, 2)
	started := time.Now().Add(-time.Minute)
	postRkey := testkit.TID()
	root := moderation.StrongRef{
		URI: pv2URI(pv2Author, postRkey), CID: editMediaCID(t.Name() + " post record"),
	}
	initial := postModerationRecord(quotedImage)
	composite := attachmentModerationEmbed(root, comment, []string{galleryOne, galleryTwo}, thumb, []string{externalImage})
	if !comment {
		initial["embed"] = composite
	}
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey,
		revisions[0], root.CID, started.UnixMicro(), initial)))
	subject, rkey := root, postRkey
	if comment {
		rkey = testkit.TID()
		subject = moderation.StrongRef{
			URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + rkey,
			CID: editMediaCID(t.Name() + " comment record"),
		}
		event := editMediaCommentEvent(root, "create", rkey, revisions[0], subject.CID, started)
		event.Commit.Record["embed"] = composite
		require.NoError(t, h.comments.HandleEvent(t.Context(), event))
	}
	requireAttachmentModerationIndexed(t, h.db, comment, subject, composite)
	removed, err := h.moderator.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "attachmentadmin")), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-" + testkit.UniqueID(t),
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.NotNil(t, removed.Action)
	wanted := []string{galleryOne, galleryTwo}
	if !comment {
		wanted = []string{galleryOne, galleryTwo, thumb, externalImage}
	}
	// Check removal before the edit so a later reconciliation cannot mask a
	// missing removal block or target for the original media.
	requireAttachmentModerationOutcomes(t, h, queue, purger, removed.Action.ID, wanted)
	if !edit {
		return
	}

	editedEmbed := attachmentModerationEmbed(root, comment, []string{galleryOne, galleryTwo, addedGallery}, thumb, []string{externalImage, addedExternal})
	edited := moderation.StrongRef{URI: subject.URI, CID: editMediaCID(t.Name() + " edited record")}
	if comment {
		event := editMediaCommentEvent(root, "update", rkey, revisions[1], edited.CID, started.Add(time.Second))
		event.Commit.Record["embed"] = editedEmbed
		require.NoError(t, h.comments.HandleEvent(t.Context(), event))
		wanted = []string{galleryOne, galleryTwo, addedGallery}
	} else {
		record := postModerationRecord()
		record["embed"] = editedEmbed
		require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", rkey,
			revisions[1], edited.CID, started.Add(time.Second).UnixMicro(), record)))
		wanted = []string{galleryOne, galleryTwo, thumb, externalImage, addedGallery, addedExternal}
	}
	requireAttachmentModerationIndexed(t, h.db, comment, edited, editedEmbed)
	requireAttachmentModerationOutcomes(t, h, queue, purger, removed.Action.ID, wanted)
}

func attachmentModerationEmbed(root moderation.StrongRef, comment bool, gallery []string, thumb string, externalGallery []string) map[string]interface{} {
	attachment := map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post":  map[string]interface{}{"uri": root.URI, "cid": root.CID},
	}
	if !comment {
		attachment = map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri": "https://example.invalid/article", "title": "nested link card",
				"thumb": map[string]interface{}{
					"$type": "blob", "ref": map[string]interface{}{"$link": thumb}, "mimeType": "image/png", "size": 10,
				},
				"images": postModerationRecord(externalGallery...)["embed"].(map[string]interface{})["images"],
			},
		}
	}
	return map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment",
		"images": postModerationRecord(gallery...)["embed"], "attachment": attachment,
	}
}

func requireAttachmentModerationIndexed(t *testing.T, db *sql.DB, comment bool, subject moderation.StrongRef, embed map[string]interface{}) {
	t.Helper()
	query := `SELECT cid, embed::text FROM posts WHERE uri = $1`
	if comment {
		query = `SELECT cid, embed::text FROM comments WHERE uri = $1`
	}
	var indexedCID, indexedEmbed string
	require.NoError(t, db.QueryRowContext(t.Context(), query, subject.URI).Scan(&indexedCID, &indexedEmbed))
	require.Equal(t, subject.CID, indexedCID, "the composite record revision must be indexed")
	encoded, err := json.Marshal(embed)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), indexedEmbed, "the indexed record must retain both composite parts")
}

func requireAttachmentModerationOutcomes(t *testing.T, h *editMediaHarness, queue *moderation.CDNPurgeQueue, purger *attachmentModerationCDNPurger, action string, cids []string) {
	t.Helper()
	type blockRow struct{ owner, cid, action string }
	var wantedBlocks []blockRow
	var wantedTargets []imageproxy.BlockedBlob
	for _, cid := range cids {
		wantedBlocks = append(wantedBlocks, blockRow{pv2Author, cid, action})
		wantedTargets = append(wantedTargets, imageproxy.BlockedBlob{OwnerDID: pv2Author, CID: cid})
	}
	rows, err := h.db.QueryContext(t.Context(), `SELECT COALESCE(owner_did, ''), blob_cid, action_id FROM moderation_media_blocks WHERE active`)
	require.NoError(t, err)
	var blocks []blockRow
	for rows.Next() {
		var row blockRow
		require.NoError(t, rows.Scan(&row.owner, &row.cid, &row.action))
		blocks = append(blocks, row)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, wantedBlocks, blocks, "every composite CID must have exactly this removal's owner-scoped block")

	rows, err = h.db.QueryContext(t.Context(), `SELECT owner_did, blob_cid FROM moderation_media_purges`)
	require.NoError(t, err)
	var targets []imageproxy.BlockedBlob
	for rows.Next() {
		var target imageproxy.BlockedBlob
		require.NoError(t, rows.Scan(&target.OwnerDID, &target.CID))
		targets = append(targets, target)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, wantedTargets, targets, "every composite CID must have its durable owner/CID purge target")

	store := postgres.NewModerationRepository(h.db)
	for _, cid := range cids {
		blocked, err := store.IsBlocked(t.Context(), h.otherOwner, cid)
		require.NoError(t, err)
		require.False(t, blocked, "the same CID at another owner must remain unblocked")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, queue.Wait(ctx))
	require.ElementsMatch(t, wantedTargets, purger.recorded(), "the configured CDN purger must receive every owner/CID")
}
