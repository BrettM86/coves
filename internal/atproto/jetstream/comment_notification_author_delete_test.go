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

type commentAuthorDeleteFixture struct {
	db        *sql.DB
	consumer  *CommentEventConsumer
	postURI   string
	postCID   string
	createdAt string
}

func newCommentAuthorDeleteFixture(t *testing.T) commentAuthorDeleteFixture {
	t.Helper()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	return commentAuthorDeleteFixture{
		db: db, consumer: NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
			WithCommentNotifications(postgres.NewNotificationRepository(db))),
		postURI: postURI, postCID: postCID,
		createdAt: activatedCommentNotificationTime(t, db, context.Background()),
	}
}

func (fixture commentAuthorDeleteFixture) parent(t *testing.T, authorDID string) (string, string) {
	t.Helper()
	key := testkit.TID()
	uri := "at://" + authorDID + "/" + CommentCollection + "/" + key
	cid := "bafyreiauthordeleteparent"
	record := revCommentRecord("An indexed parent comment", fixture.postURI, fixture.postCID, fixture.postURI, fixture.postCID)
	record["createdAt"] = fixture.createdAt
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		authorDID, CommentCollection, "create", key, testkit.TID(), cid, time.Now().UnixMicro(), record,
	)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, uri),
		"fixture: parent comment must be indexed")
	return uri, cid
}

func (fixture commentAuthorDeleteFixture) replyRecord(content, parentURI, parentCID string) map[string]interface{} {
	record := revCommentRecord(content, fixture.postURI, fixture.postCID, parentURI, parentCID)
	record["createdAt"] = fixture.createdAt
	return record
}

func (fixture commentAuthorDeleteFixture) createReply(t *testing.T, key, revision, cid string, record map[string]interface{}) string {
	t.Helper()
	uri := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "create", key, revision, cid, time.Now().UnixMicro(), record,
	)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, uri),
		"fixture: reply must be indexed")
	return uri
}

func (fixture commentAuthorDeleteFixture) mentionRecipient(t *testing.T) (string, string) {
	t.Helper()
	id := testkit.UniqueID(t)
	did, handle := "did:plc:"+id, id+".test"
	insertBridgedUserOnPDS(t, fixture.db, did, handle, bridgedTestNativePDS)
	return did, handle
}

func (fixture commentAuthorDeleteFixture) mentionedReply(t *testing.T, parentURI, parentCID, mentionedDID, mentionedHandle string) map[string]interface{} {
	t.Helper()
	content := "A replies and mentions @" + mentionedHandle
	record := fixture.replyRecord(content, parentURI, parentCID)
	record["facets"] = []interface{}{commentMentionFacet(t, content, mentionedHandle, mentionedDID)}
	return record
}

func TestCommentConsumer_AuthorDeleteKeepsRecordNotificationsAndOtherRowsAndUpvoteGroup(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	parentURI, parentCID := fixture.parent(t, revTestAuthor)
	mentionedDID, mentionedHandle := fixture.mentionRecipient(t)
	replierDID, _ := fixture.mentionRecipient(t)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeleteoriginal",
		fixture.mentionedReply(t, parentURI, parentCID, mentionedDID, mentionedHandle))
	replyKey := testkit.TID()
	replyURI := "at://" + replierDID + "/" + CommentCollection + "/" + replyKey
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		replierDID, CommentCollection, "create", replyKey, testkit.TID(), "bafyreiauthordeletechild",
		time.Now().UnixMicro(), fixture.replyRecord("E replies to Y", commentURI, "bafyreiauthordeleteoriginal"),
	)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND subject_uri = $2 AND recipient_did = $3 AND reason = 'commentReply'`,
		replyURI, commentURI, revTestCommenter))

	// Directly seed the group: this test concerns comment deletion, not vote indexing.
	_, err := fixture.db.ExecContext(context.Background(), `INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $3)`, revTestCommenter, commentURI, fixture.postURI)
	require.NoError(t, err)
	before := notificationRowsForRecordOrSubject(t, fixture.db, commentURI)
	require.Len(t, before, 4, "fixture: Y's reply, Y's mention, E's reply to Y and Y's upvote group")

	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, deleteRevision, "", time.Now().UnixMicro(), nil,
	)))
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRow(`SELECT deleted_at FROM comments WHERE uri = $1`, commentURI).Scan(&deletedAt))
	require.True(t, deletedAt.Valid, "author delete must soft-delete Y")
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, fixture.db, commentURI),
		"author delete must preserve all four rows, including their IDs, CIDs, roots and sort times")
}

func TestCommentConsumer_AuthorDeleteMissingCommentKeepsNotificationsAndAdvancesTombstone(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	key := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
	mentionedDID, _ := fixture.mentionRecipient(t)
	_, err := fixture.db.ExecContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at)
		VALUES ($1, 'postReply', $2, $3, $4, $5, $5, NOW()),
		       ($6, 'mention', $2, $3, $4, NULL, $5, NOW())`,
		revTestAuthor, commentURI, "bafyreiauthordeleteunindexed", revTestCommenter, fixture.postURI, mentionedDID)
	require.NoError(t, err)
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	deleteRevision := testkit.TID()
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, deleteRevision, "", time.Now().UnixMicro(), nil,
	)))
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"zero-row delete must keep notifications for a comment it never indexed")
	var storedRevision string
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI).Scan(&storedRevision))
	require.Equal(t, deleteRevision, storedRevision, "zero-row delete must commit its tombstone revision")
}

func TestCommentConsumer_StaleAuthorDeleteKeepsCommentNotificationsAndRevision(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	parentURI, parentCID := fixture.parent(t, revTestAuthor)
	mentionedDID, mentionedHandle := fixture.mentionRecipient(t)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeletestale",
		fixture.mentionedReply(t, parentURI, parentCID, mentionedDID, mentionedHandle))
	require.Less(t, revA, createRevision, "stale delete must lose the revision gate")
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, revA, "", time.Now().UnixMicro(), nil,
	)))
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"stale delete must keep both notification rows")
	var content, storedRevision string
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRow(`SELECT content, deleted_at FROM comments WHERE uri = $1`, commentURI).
		Scan(&content, &deletedAt))
	require.Equal(t, "A replies and mentions @"+mentionedHandle, content)
	require.False(t, deletedAt.Valid, "stale delete must leave Y active")
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI).Scan(&storedRevision))
	require.Equal(t, createRevision, storedRevision, "stale delete must not advance the revision")
}

func TestCommentConsumer_ChangedParentRecreateNotifiesNewParentAndEditMentionsOldParent(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	oldParentURI, oldParentCID := fixture.parent(t, revTestAuthor)
	newParentDID, newParentHandle := fixture.mentionRecipient(t)
	newParentURI, newParentCID := fixture.parent(t, newParentDID)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeletefirstparent",
		fixture.replyRecord("A replies to B's comment", oldParentURI, oldParentCID))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'commentReply'`, commentURI, revTestAuthor))
	deleteRevision := testkit.TID()
	recreateRevision := testkit.TID()
	editRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	require.Less(t, deleteRevision, recreateRevision)
	require.Less(t, recreateRevision, editRevision)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, deleteRevision, "", time.Now().UnixMicro(), nil,
	)))
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "create", key, recreateRevision, "bafyreiauthordeletechangedparent",
		time.Now().UnixMicro(), fixture.replyRecord("A now replies to C's comment", newParentURI, newParentCID),
	)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'commentReply' AND subject_uri = $3
		AND record_cid = $4`, commentURI, newParentDID, newParentURI, "bafyreiauthordeletechangedparent"),
		"C must have one reply notification for the re-created Y")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2`, commentURI, revTestAuthor),
		"B must have no notification for Y after its deletion and re-creation under C")

	var oldParentHandle string
	require.NoError(t, fixture.db.QueryRow(`SELECT handle FROM users WHERE did = $1`, revTestAuthor).Scan(&oldParentHandle))
	content := "A edits Y and mentions @" + oldParentHandle + " while replying to @" + newParentHandle
	editRecord := fixture.replyRecord(content, newParentURI, newParentCID)
	editRecord["facets"] = []interface{}{commentMentionFacet(t, content, oldParentHandle, revTestAuthor)}
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "update", key, editRevision, "bafyreiauthordeleteedited",
		time.Now().Add(time.Second).UnixMicro(), editRecord,
	)))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`,
		commentURI, revTestAuthor, "bafyreiauthordeleteedited"), "B receives exactly one edit-added mention")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2`, commentURI, revTestAuthor),
		"B must have only the mention, not the old reply")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'commentReply'`, commentURI, newParentDID),
		"C keeps exactly one reply to Y after its edit")
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
}

type failingCommentAuthorDeleteErasureGateRepository struct {
	notifications.Repository
	failure error
}

func (repository *failingCommentAuthorDeleteErasureGateRepository) ErasureGateTx(_ context.Context, _ *sql.Tx, _ string) (bool, error) {
	return false, repository.failure
}

func TestCommentConsumer_AuthorDeleteErasureGateFailureRollsBackContentRowsAndRevision(t *testing.T) {
	t.Parallel()
	fixture := newCommentAuthorDeleteFixture(t)
	parentURI, parentCID := fixture.parent(t, revTestAuthor)
	mentionedDID, mentionedHandle := fixture.mentionRecipient(t)
	key := testkit.TID()
	createRevision := testkit.TID()
	commentURI := fixture.createReply(t, key, createRevision, "bafyreiauthordeleterollback",
		fixture.mentionedReply(t, parentURI, parentCID, mentionedDID, mentionedHandle))
	require.Equal(t, 2, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
	before := notificationRowsForRecordOrSubject(t, fixture.db, commentURI)
	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	injectedError := errors.New("injected author-delete erasure gate failure")
	failingRepository := &failingCommentAuthorDeleteErasureGateRepository{
		Repository: postgres.NewNotificationRepository(fixture.db), failure: injectedError,
	}
	failingConsumer := NewCommentEventConsumer(postgres.NewCommentRepository(fixture.db), fixture.db,
		WithCommentNotifications(failingRepository))
	err := failingConsumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", key, deleteRevision, "", time.Now().UnixMicro(), nil,
	))
	require.ErrorIs(t, err, injectedError, "delete must propagate the erasure gate failure and roll back its transaction")
	var content, storedRevision string
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRow(`SELECT content, deleted_at FROM comments WHERE uri = $1`, commentURI).
		Scan(&content, &deletedAt))
	require.Equal(t, "A replies and mentions @"+mentionedHandle, content, "rolled-back delete must preserve Y's content")
	require.False(t, deletedAt.Valid, "rolled-back delete must leave Y active")
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, fixture.db, commentURI),
		"rolled-back delete must leave every notification row unchanged")
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI).Scan(&storedRevision))
	require.Equal(t, createRevision, storedRevision, "rolled-back delete must not claim its revision")
}
