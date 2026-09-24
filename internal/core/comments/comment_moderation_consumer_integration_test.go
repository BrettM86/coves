//go:build integration

package comments_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type consumerPurgeCall struct {
	ownerDID string
	blobCID  string
	visible  bool
	err      error
}

type consumerPurger struct {
	db    *sql.DB
	calls []consumerPurgeCall
}

func (p *consumerPurger) record(ownerDID, blobCID string) error {
	call := consumerPurgeCall{ownerDID: ownerDID, blobCID: blobCID}
	// A separate connection cannot see an uncommitted consumer transaction.
	call.err = p.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM moderation_media_blocks
		               WHERE owner_did IS NOT DISTINCT FROM NULLIF($1, '')
		                 AND blob_cid = $2 AND active)
	`, ownerDID, blobCID).Scan(&call.visible)
	p.calls = append(p.calls, call)
	return call.err
}

func (p *consumerPurger) PurgeOwnerBlob(ownerDID, blobCID string) error {
	return p.record(ownerDID, blobCID)
}

func (p *consumerPurger) PurgeBlob(blobCID string) error {
	return p.record("", blobCID)
}

func consumerCommentEvent(authorDID, postURI, postCID, rkey, operation, rev, cid, text, imageCID string, eventTime time.Time) *jetstream.JetstreamEvent {
	event := &jetstream.JetstreamEvent{
		Did: authorDID, Kind: "commit", TimeUS: eventTime.UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: rev, Operation: operation, Collection: moderation.CommentCollection,
			RKey: rkey, CID: cid,
		},
	}
	if operation != "delete" {
		record := map[string]interface{}{
			"$type": moderation.CommentCollection, "content": text,
			"reply": map[string]interface{}{
				"root":   map[string]interface{}{"uri": postURI, "cid": postCID},
				"parent": map[string]interface{}{"uri": postURI, "cid": postCID},
			},
			"createdAt": eventTime.Format(time.RFC3339),
		}
		if imageCID != "" {
			record["embed"] = map[string]interface{}{
				"$type": "social.coves.embed.images",
				"images": []interface{}{map[string]interface{}{
					"alt": "indexed image",
					"image": map[string]interface{}{
						"$type": "blob", "ref": map[string]interface{}{"$link": imageCID},
						"mimeType": "image/png", "size": 10,
					},
				}},
			}
		}
		event.Commit.Record = record
	}
	return event
}

func TestModerationCommentConsumerReconcilesRemovedImages(t *testing.T) {
	for _, scenario := range []struct {
		name, reason, operation string
		initialImage            bool
		remove                  bool
	}{
		{name: "edit adds image under spam removal", reason: moderationTestReason, operation: "update", initialImage: true, remove: true},
		{name: "illegal content edit adds ownerless block", reason: "social.coves.moderation.defs#reasonIllegalContent", operation: "update", remove: true},
		{name: "duplicate create after removal does not purge", reason: moderationTestReason, operation: "duplicate", initialImage: true, remove: true},
		{name: "author delete then recreate same URI", reason: moderationTestReason, operation: "recreate", remove: true},
		{name: "newer-rev re-create of the active row", reason: moderationTestReason, operation: "recreate-active", initialImage: true, remove: true},
		{name: "author delete then recreate under an unsupported parent", reason: moderationTestReason, operation: "recreate-unsupported-parent", remove: true},
		{name: "purged row recreated by a fresh insert", reason: moderationTestReason, operation: "purge-recreate", initialImage: true, remove: true},
		{name: "ordinary edit does not block image", operation: "update"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			ctx := t.Context()
			owner := testkit.UniqueIDWithPrefix(t, "mediaowner")
			authorDID := fixtures.DID(owner)
			fixtures.User(t, db, owner+".test", authorDID)
			communityName := testkit.UniqueIDWithPrefix(t, "mediathread")
			communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
			require.NoError(t, err)
			postURI := fixtures.Post(t, db, communityDID, authorDID, "media reconciliation", 0, time.Now())
			post, err := postgres.NewPostRepository(db).GetRawIndexedRow(ctx, postURI)
			require.NoError(t, err)

			moderationRepo := postgres.NewModerationRepository(db)
			purger := &consumerPurger{db: db}
			consumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
				jetstream.WithCommentMediaReconciler(moderation.NewMediaReconciler(moderationRepo, fixtures.InstanceDID(), purger)))
			moderationService := moderation.NewService(
				moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
				moderationRepo,
				moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
			)
			commentService := setupCommentService(db)
			rkey := testkit.TID()
			uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
			initialImageCID := ""
			if scenario.initialImage {
				initialImageCID = moderatedImageCID
			}
			started := time.Now().Add(-2 * time.Minute)
			create := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v1", moderatedCommentCID, "original comment", initialImageCID, started)
			require.NoError(t, consumer.HandleEvent(ctx, create))
			initial, err := postgres.NewCommentRepository(db).GetByURI(ctx, uri)
			require.NoError(t, err)
			assert.Equal(t, moderatedCommentCID, initial.CID)
			var removal *moderation.MutationResult
			if scenario.remove {
				removal, err = moderationService.RemoveContent(ctx, fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
					Subject: moderation.StrongRef{URI: uri, CID: moderatedCommentCID}, ExpectedVersion: "v0",
					IdempotencyKey: "consumer-remove", Reason: scenario.reason,
				})
				require.NoError(t, err)
				require.Equal(t, moderation.OutcomeApplied, removal.Outcome)
			}
			assert.Empty(t, purger.calls, "the fake is installed on the consumer, not the removal service")

			newImageCID := moderatedCommentCID // Distinct from the original image CID.
			switch scenario.operation {
			case "update":
				update := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "update", "v2", moderatedImageCID, "edited comment", newImageCID, started.Add(time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, update))
			case "duplicate":
				require.NoError(t, consumer.HandleEvent(ctx, create))
			case "recreate":
				deleted := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "delete", "v2", "", "", "", started.Add(time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, deleted))
				recreated := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v3", moderatedImageCID, "recreated comment", newImageCID, started.Add(2*time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, recreated))
			case "recreate-active":
				recreated := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v2", moderatedImageCID, "recreated comment", newImageCID, started.Add(time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, recreated))
			case "recreate-unsupported-parent":
				deleted := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "delete", "v2", "", "", "", started.Add(time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, deleted))
				recreated := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v3", moderatedImageCID, "recreated comment", newImageCID, started.Add(2*time.Minute))
				recreated.Commit.Record["reply"].(map[string]interface{})["parent"] = map[string]interface{}{
					"uri": "at://" + authorDID + "/social.coves.unsupported.collection/" + testkit.TID(), "cid": post.CID,
				}
				require.NoError(t, consumer.HandleEvent(ctx, recreated))
			case "purge-recreate":
				// Account-deletion purge removes the row outright; the next create
				// takes the fresh-insert path rather than resurrection.
				_, err := db.ExecContext(ctx, `DELETE FROM comments WHERE uri = $1`, uri)
				require.NoError(t, err)
				recreated := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v2", moderatedImageCID, "recreated comment", newImageCID, started.Add(time.Minute))
				require.NoError(t, consumer.HandleEvent(ctx, recreated))
			}

			indexed, err := postgres.NewCommentRepository(db).GetByURI(ctx, uri)
			require.NoError(t, err)
			if scenario.operation == "duplicate" {
				assert.Equal(t, "original comment", indexed.Content)
				assert.Equal(t, moderatedCommentCID, indexed.CID)
			} else {
				assert.Equal(t, moderatedImageCID, indexed.CID, "the new record must actually be indexed")
				require.NotNil(t, indexed.Embed)
				assert.Contains(t, *indexed.Embed, newImageCID)
				assert.Nil(t, indexed.DeletedAt, "recreated comment must be present in the index")
			}

			state, err := moderationService.GetSubjectState(ctx, uri)
			require.NoError(t, err)
			require.NotNil(t, state.CurrentSubject)
			assert.Equal(t, indexed.CID, state.CurrentSubject.CID)
			response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{PostURI: postURI, Sort: "new", Depth: 1, Limit: 10})
			require.NoError(t, err)
			if scenario.operation == "recreate-unsupported-parent" {
				require.Empty(t, response.Comments, "a comment under an unsupported parent is not a top-level reply")
			} else {
				require.Len(t, response.Comments, 1)
			}
			if scenario.remove {
				var activeAction string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT active_action_id FROM moderation_decisions WHERE subject_uri = $1 AND authority_did = $2 AND kind = 'removal' AND active`, uri, fixtures.InstanceDID()).Scan(&activeAction))
				assert.Equal(t, removal.Action.ID, activeAction)
				assert.Equal(t, removal.State.Version, state.Version, "an author event must not advance the moderation version")
				assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
				require.NotNil(t, state.LocalRemoval)
				assert.Equal(t, activeAction, state.LocalRemoval.ActionID)
			}
			if scenario.remove && len(response.Comments) == 1 {
				view := response.Comments[0].Comment
				assert.True(t, view.IsDeleted)
				require.NotNil(t, view.Moderation)
				assert.Equal(t, moderation.ModerationStateRemoved, view.Moderation.State)
				assert.Nil(t, view.Record, "the edited/recreated text must stay hidden")
				assert.Nil(t, view.Embed)
			} else if !scenario.remove {
				view := response.Comments[0].Comment
				assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
				assert.False(t, view.IsDeleted)
				assert.Nil(t, view.Moderation)
				require.NotNil(t, view.Record)
			}

			var blocks int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM moderation_media_blocks WHERE blob_cid = $1 AND active`, newImageCID).Scan(&blocks))
			if scenario.operation == "duplicate" || !scenario.remove {
				assert.Zero(t, blocks)
				assert.Empty(t, purger.calls, "replays and unremoved comments must never trigger cache purges")
				if scenario.initialImage {
					blocked, checkErr := moderationRepo.IsBlocked(ctx, authorDID, initialImageCID)
					require.NoError(t, checkErr)
					assert.True(t, blocked, "the original removal's image block must survive duplicate delivery")
				}
				return
			}
			blocked, err := moderationRepo.IsBlocked(ctx, authorDID, newImageCID)
			require.NoError(t, err)
			assert.True(t, blocked, "new images on a removed comment must be blocked")
			require.NotEmpty(t, purger.calls, "new pair must be purged")
			assert.Equal(t, consumerPurgeCall{ownerDID: authorDID, blobCID: newImageCID, visible: true}, purger.calls[0], "pair purge must happen after commit")
			if scenario.reason == "social.coves.moderation.defs#reasonIllegalContent" {
				assert.Equal(t, 2, blocks, "illegal content blocks both the owner pair and every owner")
				require.Len(t, purger.calls, 2)
				assert.Contains(t, purger.calls, consumerPurgeCall{blobCID: newImageCID, visible: true}, "ownerless purge must happen after commit")
				otherOwnerBlocked, checkErr := moderationRepo.IsBlocked(ctx, fixtures.DID("otherimageowner"), newImageCID)
				require.NoError(t, checkErr)
				assert.True(t, otherOwnerBlocked)
			} else {
				assert.Equal(t, 1, blocks, "spam blocks only the image owner's pair")
				require.Len(t, purger.calls, 1)
				otherOwnerBlocked, checkErr := moderationRepo.IsBlocked(ctx, fixtures.DID("otherimageowner"), newImageCID)
				require.NoError(t, checkErr)
				assert.False(t, otherOwnerBlocked, "a spam block must not affect another owner")
			}
			if scenario.initialImage {
				oldBlocked, checkErr := moderationRepo.IsBlocked(ctx, authorDID, initialImageCID)
				require.NoError(t, checkErr)
				assert.True(t, oldBlocked, "editing must not release the original image block")
			}
		})
	}
}

// An author controls the embed of a removed comment. No shape of it may make
// the consumer's reconciliation fail, and every image blob the comment view can
// serve — including the legacy top-level cid encoding — must be blocked.
func TestModerationCommentConsumerReconcilesMalformedEmbeds(t *testing.T) {
	blob := func(cid string) map[string]interface{} {
		return map[string]interface{}{"$type": "blob", "ref": map[string]interface{}{"$link": cid}, "mimeType": "image/png", "size": 10}
	}
	for _, test := range []struct {
		name    string
		embed   map[string]interface{}
		blocked []string
	}{
		{name: "images is a string", embed: map[string]interface{}{"$type": "social.coves.embed.images", "images": "x"}},
		{name: "image is a string", embed: map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{map[string]interface{}{"image": "x"}}}},
		{name: "ref is a string", embed: map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{map[string]interface{}{"image": map[string]interface{}{"ref": moderatedCommentCID}}}}},
		{name: "type is not a string", embed: map[string]interface{}{"$type": 5, "images": []interface{}{map[string]interface{}{"image": blob(moderatedCommentCID)}}}},
		{
			name: "legacy cid blob beside malformed entries",
			embed: map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{
				"x", map[string]interface{}{"image": 7},
				map[string]interface{}{"image": map[string]interface{}{"cid": moderatedCommentCID, "mimeType": "image/png"}},
			}},
			blocked: []string{moderatedCommentCID},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			ctx := t.Context()
			owner := testkit.UniqueIDWithPrefix(t, "malformedowner")
			authorDID := fixtures.DID(owner)
			fixtures.User(t, db, owner+".test", authorDID)
			communityName := testkit.UniqueIDWithPrefix(t, "malformedthread")
			communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
			require.NoError(t, err)
			postURI := fixtures.Post(t, db, communityDID, authorDID, "malformed embed reconciliation", 0, time.Now())
			post, err := postgres.NewPostRepository(db).GetRawIndexedRow(ctx, postURI)
			require.NoError(t, err)

			moderationRepo := postgres.NewModerationRepository(db)
			purger := &consumerPurger{db: db}
			consumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
				jetstream.WithCommentMediaReconciler(moderation.NewMediaReconciler(moderationRepo, fixtures.InstanceDID(), purger)))
			moderationService := moderation.NewService(
				moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
				moderationRepo,
				moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
			)
			rkey := testkit.TID()
			uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
			started := time.Now().Add(-2 * time.Minute)
			create := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", "v1", moderatedCommentCID, "original comment", moderatedImageCID, started)
			require.NoError(t, consumer.HandleEvent(ctx, create))
			removal, err := moderationService.RemoveContent(ctx, fixtures.DID(testkit.UniqueIDWithPrefix(t, "malformedadmin")), moderation.RemoveContentRequest{
				Subject: moderation.StrongRef{URI: uri, CID: moderatedCommentCID}, ExpectedVersion: "v0",
				IdempotencyKey: "malformed-remove", Reason: moderationTestReason,
			})
			require.NoError(t, err)
			require.Equal(t, moderation.OutcomeApplied, removal.Outcome)

			update := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "update", "v2", moderatedImageCID, "edited comment", "", started.Add(time.Minute))
			update.Commit.Record["embed"] = test.embed
			require.NoError(t, consumer.HandleEvent(ctx, update), "a malformed embed must not fail reconciliation")
			indexed, err := postgres.NewCommentRepository(db).GetByURI(ctx, uri)
			require.NoError(t, err)
			assert.Equal(t, moderatedImageCID, indexed.CID, "the edit must be indexed")

			rows, err := db.QueryContext(ctx, `
				SELECT blob_cid FROM moderation_media_blocks
				WHERE action_id = $1 AND active AND blob_cid <> $2 ORDER BY blob_cid
			`, removal.Action.ID, moderatedImageCID)
			require.NoError(t, err)
			defer rows.Close()
			var blocked []string
			for rows.Next() {
				var blobCID string
				require.NoError(t, rows.Scan(&blobCID))
				blocked = append(blocked, blobCID)
			}
			require.NoError(t, rows.Err())
			assert.Equal(t, test.blocked, blocked)
			originalBlocked, err := moderationRepo.IsBlocked(ctx, authorDID, moderatedImageCID)
			require.NoError(t, err)
			assert.True(t, originalBlocked, "the original image block must survive the edit")
		})
	}
}
