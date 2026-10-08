//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestCommentConsumer_ResurrectionLifecycle_DeleteKeepsRowsAndSameParentRecreateNotifiesOnlyNewRecipients(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	mentionedID := testkit.UniqueID(t)
	mentionedDID := "did:plc:" + mentionedID
	mentionedHandle := mentionedID + ".test"
	insertBridgedUserOnPDS(t, db, mentionedDID, mentionedHandle, bridgedTestNativePDS)
	newID := testkit.UniqueID(t)
	newDID, newHandle := "did:plc:"+newID, newID+".test"
	insertBridgedUserOnPDS(t, db, newDID, newHandle, bridgedTestNativePDS)

	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)))
	parentKey := testkit.TID()
	parentURI := "at://" + revTestAuthor + "/" + CommentCollection + "/" + parentKey
	parentCID := "bafyreiresurrectionparent"
	parentRecord := revCommentRecord("B comments on B's post", postURI, postCID, postURI, postCID)
	parentRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestAuthor, CommentCollection, "create", parentKey, testkit.TID(), parentCID,
		time.Now().UnixMicro(), parentRecord,
	)), "index B's parent comment")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, parentURI),
		"fixture: B's parent comment must be indexed")

	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	commentRecord := func(content string) map[string]interface{} {
		record := revCommentRecord(content, postURI, postCID, parentURI, parentCID)
		record["createdAt"] = createdAt
		record["facets"] = []interface{}{commentMentionFacet(t, content, mentionedHandle, mentionedDID)}
		return record
	}
	firstCID := "bafyreiresurrectionoriginal"
	firstRevision := testkit.TID()
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, firstRevision, firstCID,
		time.Now().UnixMicro(), commentRecord("A replies to B and mentions @"+mentionedHandle),
	)), "index A's reply to B's comment mentioning D")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'commentReply' AND record_cid = $3`,
		commentURI, revTestAuthor, firstCID), "B must receive exactly one reply to X")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
		commentURI, mentionedDID, firstCID), "D must receive exactly one mention from Y")
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"Y must have only B's reply and D's mention")
	before := notificationRowsForRecordOrSubject(t, db, commentURI)
	require.Len(t, before, 2)

	deleteRevision := testkit.TID()
	require.Less(t, firstRevision, deleteRevision, "delete revision must be newer than the create")
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", commentKey, deleteRevision, "",
		time.Now().UnixMicro(), nil,
	)), "author deletes Y")
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, db, commentURI),
		"author deletion must keep the original reply and mention rows")

	recreatedCID := "bafyreiresurrectionrecreated"
	recreateRevision := testkit.TID()
	require.Less(t, deleteRevision, recreateRevision, "re-create revision must be newer than the delete")
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, recreateRevision, recreatedCID,
		time.Now().UnixMicro(), func() map[string]interface{} {
			content := "A re-creates Y mentioning @" + mentionedHandle + " @" + newHandle
			record := commentRecord(content)
			record["facets"] = []interface{}{
				commentMentionFacet(t, content, mentionedHandle, mentionedDID),
				commentMentionFacet(t, content, newHandle, newDID),
			}
			return record
		}(),
	)), "author re-creates Y with the same parent and root")
	after := notificationRowsForRecordOrSubject(t, db, commentURI)
	require.Len(t, after, 3, "only F receives a new notification")
	require.Equal(t, before[0], after[0], "B keeps the original id, CID, root and sort time")
	require.Equal(t, before[1], after[1], "D keeps the original id, CID, root and sort time")
	require.Equal(t, notificationRowSnapshot{recipient: newDID, reason: "mention",
		recordCID: sql.NullString{String: recreatedCID, Valid: true}, rootPostURI: postURI},
		notificationRowSnapshot{recipient: after[2].recipient, reason: after[2].reason,
			recordCID: after[2].recordCID, rootPostURI: after[2].rootPostURI}, "F alone receives the new CID")
}

func TestCommentConsumer_SameParentResurrectionNotificationFailureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)))
	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	originalCID := "bafyreisameparentrollbackoriginal"
	createRevision := testkit.TID()
	originalRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	originalRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, createRevision, originalCID,
		time.Now().UnixMicro(), originalRecord,
	)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'postReply'`, commentURI, revTestAuthor),
		"fixture: the first create must notify B")

	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", commentKey, deleteRevision, "",
		time.Now().UnixMicro(), nil,
	)))
	var deletedAt time.Time
	var deletedContent, deletedCID string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT deleted_at, content, cid FROM comments WHERE uri = $1`, commentURI,
	).Scan(&deletedAt, &deletedContent, &deletedCID), "fixture: Y must be soft-deleted before re-creation")
	require.Empty(t, deletedContent, "fixture: author delete must blank Y's content")
	require.Equal(t, originalCID, deletedCID, "fixture: author delete must preserve Y's old CID")
	keptRows := notificationRowsForRecordOrSubject(t, db, commentURI)
	require.Len(t, keptRows, 1, "fixture: author deletion must keep Y's old notification")
	var commentCountBefore int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT comment_count FROM posts WHERE uri = $1`, postURI).
		Scan(&commentCountBefore))
	require.Equal(t, 1, commentCountBefore, "fixture: Y still contributes to its parent's comment count")
	var storedRevision string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI,
	).Scan(&storedRevision))
	require.Equal(t, deleteRevision, storedRevision)

	injectedError := errors.New("injected same-parent resurrection notification write failure")
	failingRepository := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(db), failure: injectedError,
	}
	failingConsumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(failingRepository))
	recreateRevision := testkit.TID()
	require.Less(t, deleteRevision, recreateRevision)
	recreatedRecord := revCommentRecord("A re-creates Y on B's post", postURI, postCID, postURI, postCID)
	recreatedRecord["createdAt"] = createdAt
	err := failingConsumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, recreateRevision,
		"bafyreisameparentrollbackrecreated", time.Now().UnixMicro(), recreatedRecord,
	))
	require.ErrorIs(t, err, injectedError, "same-parent resurrection must fail when its notification write fails")
	require.Len(t, failingRepository.intents, 1, "re-creation must attempt B's postReply notification")

	var afterDeletedAt sql.NullTime
	var afterContent, afterCID string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT deleted_at, content, cid FROM comments WHERE uri = $1`, commentURI,
	).Scan(&afterDeletedAt, &afterContent, &afterCID))
	require.True(t, afterDeletedAt.Valid, "failed re-creation must leave Y soft-deleted")
	require.True(t, afterDeletedAt.Time.Equal(deletedAt), "failed re-creation must preserve the delete timestamp")
	require.Equal(t, deletedContent, afterContent, "failed re-creation must preserve Y's blanked content")
	require.Equal(t, deletedCID, afterCID, "failed re-creation must preserve Y's old CID")
	var commentCountAfter int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT comment_count FROM posts WHERE uri = $1`, postURI).
		Scan(&commentCountAfter))
	require.Equal(t, commentCountBefore, commentCountAfter, "failed re-creation must not change the parent's count")
	require.Equal(t, keptRows, notificationRowsForRecordOrSubject(t, db, commentURI),
		"failed re-creation must preserve the kept notification")
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI,
	).Scan(&storedRevision))
	require.Equal(t, deleteRevision, storedRevision, "failed re-creation must not advance Y's revision")
}
