//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const activeRecreateCID = "bafyreiactiverecreatenew"

func activeRecreateEvent(t *testing.T, fixture mentionEditFixture, createdAt string, recipients ...mentionEditRecipient) (*JetstreamEvent, string) {
	t.Helper()
	revision := testkit.TID()
	require.Less(t, fixture.revision, revision, "active re-create must have a newer rev")
	record := fixture.record(t, createdAt, recipients...)
	content := record["content"].(string) + " re-created"
	record["content"] = content
	return revCommitEvent(fixture.gate.commenterDID, CommentCollection, "create", fixture.key,
		revision, activeRecreateCID, time.Now().Add(time.Second).UnixMicro(), record), content
}

func requireActiveRecreateApplied(t *testing.T, fixture mentionEditFixture, content string) {
	t.Helper()
	var cid, storedContent string
	var deletedAt sql.NullTime
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT cid, content, deleted_at FROM comments WHERE uri = $1`, fixture.uri).
		Scan(&cid, &storedContent, &deletedAt))
	require.False(t, deletedAt.Valid, "re-created comment must remain active")
	require.Equal(t, activeRecreateCID, cid, "the newer create must replace the active row's CID")
	require.Equal(t, content, storedContent, "the newer create must replace the active row's content")
}

func TestCommentConsumer_ActiveRecreateNotifiesOnlyNewMentionAndKeepsExistingRows(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	postAuthor := fixture.postAuthor(t)
	kept := fixture.recipient(t)
	added := fixture.recipient(t)
	fixture.create(t, kept)
	require.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"))
	require.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"))
	previousRows := countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri)
	require.Equal(t, 2, previousRows)
	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, kept, added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
		fixture.uri, added.did, activeRecreateCID), "newly mentioned E gets one mention with the re-created CID")
	require.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"), "retained D must not receive a duplicate")
	require.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"), "the existing reply stays")
	require.Equal(t, previousRows+1, countRows(t, fixture.gate.db,
		`SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri))
}

func TestCommentConsumer_ActiveRecreateDiffUsesStoredFacetsEvenWhenOriginalRecipientWasUnindexed(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	id := testkit.UniqueID(t)
	kept := mentionEditRecipient{did: "did:plc:" + id + "mentioned", handle: id + "mentioned.test"}
	added := fixture.recipient(t)
	fixture.create(t, kept)
	requireStoredMentionFacets(t, fixture.gate.db, fixture.uri, kept.did)
	require.Zero(t, mentionEditRows(t, fixture, kept, "mention"), "unindexed D receives no initial notification")
	_, err := fixture.gate.db.Exec(`INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())`, kept.did, kept.handle, bridgedTestNativePDS)
	require.NoError(t, err, "D becomes eligible only after the original comment")
	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, kept, added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
		fixture.uri, added.did, activeRecreateCID), "only newly mentioned E should be notified")
	require.Zero(t, mentionEditRows(t, fixture, kept, "mention"),
		"D was in the stored facets despite being unindexed at create; no backfill on re-create")
}

func TestCommentConsumer_ActiveRecreateDoesNotMentionReplyRecipient(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"post reply", "comment reply"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			fixture := newMentionEditFixture(t)
			replyRecipient := fixture.postAuthor(t)
			reason := "postReply"
			if kind == "comment reply" {
				replyRecipient = fixture.recipient(t)
				reason = "commentReply"
				parentKey := testkit.TID()
				fixture.parentURI = "at://" + replyRecipient.did + "/" + CommentCollection + "/" + parentKey
				fixture.parentCID = "bafyreiactiverecreateparent"
				parentRecord := revCommentRecord("C comments on B's post", fixture.post.uri,
					fixture.post.cid, fixture.post.uri, fixture.post.cid)
				parentRecord["createdAt"] = fixture.createdAt
				require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
					replyRecipient.did, CommentCollection, "create", parentKey, testkit.TID(),
					fixture.parentCID, time.Now().UnixMicro(), parentRecord)))
			}
			added := fixture.recipient(t)
			fixture.create(t)
			require.Equal(t, 1, mentionEditRows(t, fixture, replyRecipient, reason),
				"reply recipient must have the original reply notification")
			require.Zero(t, mentionEditRows(t, fixture, replyRecipient, "mention"))
			event, content := activeRecreateEvent(t, fixture, fixture.createdAt, replyRecipient, added)
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
			requireActiveRecreateApplied(t, fixture, content)
			require.Equal(t, 1, mentionEditRows(t, fixture, replyRecipient, reason))
			require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
				WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, replyRecipient.did),
				"resolved reply recipient must retain only the reply row")
			require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
				WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
				fixture.uri, added.did, activeRecreateCID), "E receives exactly one new mention")
		})
	}
}

func TestCommentConsumer_ActiveRecreateActivationUsesStoredCreatedAt(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	storedCreatedAt, err := time.Parse(time.RFC3339Nano, fixture.createdAt)
	require.NoError(t, err)
	activation := storedCreatedAt.Add(30 * time.Second)
	incomingCreatedAt := storedCreatedAt.Add(time.Minute)
	require.True(t, storedCreatedAt.Before(activation) && activation.Before(incomingCreatedAt))
	_, err = fixture.gate.db.Exec(`UPDATE notification_activation SET activated_at = $1`, activation)
	require.NoError(t, err)
	fixture.create(t)
	require.Zero(t, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri),
		"stored pre-activation comment must not generate a reply")
	event, content := activeRecreateEvent(t, fixture, incomingCreatedAt.UTC().Format(time.RFC3339Nano), added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	require.Zero(t, mentionEditRows(t, fixture, added, "mention"),
		"incoming createdAt cannot activate a comment whose stored created_at predates activation")
}

func TestCommentConsumer_ActiveRecreateNotificationFailureRollsBackContentFacetsAndRevision(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	var beforeCID, beforeContent, beforeRevision string
	var beforeFacets sql.NullString
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT cid, content, content_facets FROM comments WHERE uri = $1`,
		fixture.uri).Scan(&beforeCID, &beforeContent, &beforeFacets))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`,
		fixture.uri).Scan(&beforeRevision))
	require.Equal(t, fixture.revision, beforeRevision)
	injectedError := errors.New("injected active re-create notification failure")
	failingRepository := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(fixture.gate.db), failure: injectedError,
	}
	fixture.consumer = fixture.gate.commentConsumer(WithCommentNotifications(failingRepository))
	event, _ := activeRecreateEvent(t, fixture, fixture.createdAt, added)
	err := fixture.consumer.HandleEvent(context.Background(), event)
	require.ErrorIs(t, err, injectedError, "notification failure must propagate from active re-create")
	require.Len(t, failingRepository.intents, 1, "new mention must reach ApplyTx")
	require.Equal(t, notifications.ReasonMention, failingRepository.intents[0].Reason)
	require.Equal(t, added.did, failingRepository.intents[0].RecipientDID)
	var afterCID, afterContent, afterRevision string
	var afterFacets sql.NullString
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT cid, content, content_facets FROM comments WHERE uri = $1`,
		fixture.uri).Scan(&afterCID, &afterContent, &afterFacets))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`,
		fixture.uri).Scan(&afterRevision))
	require.Equal(t, beforeCID, afterCID, "failed re-create must retain the original CID")
	require.Equal(t, beforeContent, afterContent, "failed re-create must retain the original content")
	require.Equal(t, beforeFacets, afterFacets, "failed re-create must retain the original facets")
	require.Equal(t, beforeRevision, afterRevision, "failed re-create must roll back the rev gate")
	require.Zero(t, mentionEditRows(t, fixture, added, "mention"))
}

// The consumer still indexes an erased actor's events, so an active re-create
// can arrive after A is erased while A's comment row is still present.
func TestCommentConsumer_ActiveRecreateByErasedActorDoesNotNotifyAddedMention(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	_, err := fixture.gate.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.gate.commenterDID)
	require.NoError(t, err, "fixture: retain the active comment row and mark A erased")
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM comments WHERE uri = $1 AND deleted_at IS NULL`, fixture.uri))
	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	require.Zero(t, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, added.did),
		"erased A's active re-create must not notify newly mentioned E")
}
