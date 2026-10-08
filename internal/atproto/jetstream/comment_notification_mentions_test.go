//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func commentMentionFacet(t *testing.T, content, handle, did string) interface{} {
	t.Helper()
	start := strings.Index(content, handle)
	require.NotEqual(t, -1, start, "fixture: mentioned handle must appear in the comment content")
	return map[string]interface{}{
		"index": map[string]interface{}{"byteStart": start, "byteEnd": start + len(handle)},
		"features": []interface{}{
			map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": did},
		},
	}
}

func requireStoredMentionFacets(t *testing.T, db *sql.DB, commentURI string, mentionedDIDs ...string) {
	t.Helper()
	var facets sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT content_facets FROM comments WHERE uri = $1`, commentURI).Scan(&facets))
	require.True(t, facets.Valid, "fixture: valid mention facets must survive comment indexing")
	for _, did := range mentionedDIDs {
		require.Contains(t, facets.String, did, "fixture: the mention feature must be stored")
	}
}

func TestCommentConsumer_MentionOnPostReplyDeduplicatesReplyRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	post := fixture.posts[0]
	otherDID := "did:plc:" + testkit.UniqueID(t) + "mentioned"
	otherHandle := testkit.UniqueID(t) + "mentioned.test"
	_, err := fixture.db.ExecContext(ctx,
		`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
		otherDID, otherHandle, bridgedTestNativePDS)
	require.NoError(t, err, "index D as an eligible mention recipient")
	createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)

	var postAuthorHandle string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT handle FROM users WHERE did = $1`, post.authorDID).Scan(&postAuthorHandle))
	content := "A replies to @" + postAuthorHandle + " and @" + otherHandle
	commentRecord := revCommentRecord(content, post.uri, post.cid, post.uri, post.cid)
	commentRecord["createdAt"] = createdAt
	commentRecord["facets"] = []interface{}{
		commentMentionFacet(t, content, postAuthorHandle, post.authorDID),
		commentMentionFacet(t, content, otherHandle, otherDID),
	}
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	commentCID := "bafyreicommentmentionreply"
	require.NoError(t, fixture.commentConsumer().HandleEvent(ctx, revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, testkit.TID(), commentCID,
		time.Now().UnixMicro(), commentRecord,
	)), "index A's top-level comment mentioning B and D")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the comment must be indexed")
	requireStoredMentionFacets(t, fixture.db, commentURI, post.authorDID, otherDID)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'postReply' AND record_uri = $2`, post.authorDID, commentURI),
		"B receives exactly one postReply")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`, post.authorDID, commentURI),
		"B must not also receive a mention for the same reply")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`, otherDID, commentURI),
		"D must receive exactly one mention for A's comment")

	var actorDID, recordURI, recordCID, rootPostURI string
	var subjectURI sql.NullString
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT actor_did, record_uri, record_cid, subject_uri, root_post_uri
		FROM notifications WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`,
		otherDID, commentURI).Scan(&actorDID, &recordURI, &recordCID, &subjectURI, &rootPostURI))
	require.False(t, subjectURI.Valid, "mention subject_uri must be NULL")
	require.Equal(t, post.uri, rootPostURI)
	require.Equal(t, fixture.commenterDID, actorDID)
	require.Equal(t, commentURI, recordURI)
	require.Equal(t, commentCID, recordCID)
}

func TestCommentConsumer_MentionWithUnsupportedParentCollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	post := fixture.posts[0]
	createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
	parentURI := "at://" + post.authorDID + "/app.bsky.feed.post/" + testkit.TID()
	var handle string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT handle FROM users WHERE did = $1`, post.authorDID).Scan(&handle))
	content := "A mentions @" + handle + " in this thread"
	commentRecord := revCommentRecord(content, post.uri, post.cid, parentURI, "bafyreicommentmentionparent")
	commentRecord["createdAt"] = createdAt
	commentRecord["facets"] = []interface{}{commentMentionFacet(t, content, handle, post.authorDID)}
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	commentCID := "bafyreicommentmentionunsupported"
	require.NoError(t, fixture.commentConsumer().HandleEvent(ctx, revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, testkit.TID(), commentCID,
		time.Now().UnixMicro(), commentRecord,
	)), "index A's comment with an unsupported parent collection")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the comment must still be indexed")
	requireStoredMentionFacets(t, fixture.db, commentURI, post.authorDID)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND actor_did = $2 AND record_uri = $3
		  AND record_cid = $4 AND subject_uri IS NULL AND root_post_uri = $5`,
		post.authorDID, fixture.commenterDID, commentURI, commentCID, post.uri),
		"B must receive exactly one mention even when the parent collection is unsupported")
}

// A root that parses but is not a post cannot become a notification's
// root_post_uri: the comment indexes with its facets, and nobody is notified.
func TestCommentConsumer_MentionWithNonPostRootNotifiesNobody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	post := fixture.posts[0]
	createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
	rootURI := "at://" + post.authorDID + "/app.bsky.feed.post/" + testkit.TID()
	const rootCID = "bafyreicommentmentionnonpostroot"
	var handle string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT handle FROM users WHERE did = $1`, post.authorDID).Scan(&handle))
	content := "A mentions @" + handle + " under a Bluesky post"
	commentRecord := revCommentRecord(content, rootURI, rootCID, rootURI, rootCID)
	commentRecord["createdAt"] = createdAt
	commentRecord["facets"] = []interface{}{commentMentionFacet(t, content, handle, post.authorDID)}
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	require.NoError(t, fixture.commentConsumer().HandleEvent(ctx, revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, testkit.TID(), "bafyreicommentmentionnonpost",
		time.Now().UnixMicro(), commentRecord,
	)), "index A's comment whose root is a Bluesky post")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the comment must still be indexed")
	requireStoredMentionFacets(t, fixture.db, commentURI, post.authorDID)
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"indexed, eligible B must not get a mention whose root_post_uri names a Bluesky post")
}

func TestCommentConsumer_MentionDuplicateDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	post := fixture.posts[0]
	mentionedDID := "did:plc:" + testkit.UniqueID(t) + "mentioned"
	mentionedHandle := testkit.UniqueID(t) + "mentioned.test"
	_, err := fixture.db.ExecContext(ctx,
		`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
		mentionedDID, mentionedHandle, bridgedTestNativePDS)
	require.NoError(t, err, "index D as an eligible mention recipient")
	var postAuthorHandle string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT handle FROM users WHERE did = $1`, post.authorDID).Scan(&postAuthorHandle))
	content := "A replies to @" + postAuthorHandle + " and @" + mentionedHandle
	commentRecord := revCommentRecord(content, post.uri, post.cid, post.uri, post.cid)
	commentRecord["createdAt"] = activatedCommentNotificationTime(t, fixture.db, ctx)
	commentRecord["facets"] = []interface{}{
		commentMentionFacet(t, content, postAuthorHandle, post.authorDID),
		commentMentionFacet(t, content, mentionedHandle, mentionedDID),
	}
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	commentCID := "bafyreicommentmentionduplicate"
	event := revCommitEvent(fixture.commenterDID, CommentCollection, "create", commentKey,
		testkit.TID(), commentCID, time.Now().UnixMicro(), commentRecord)
	consumer := fixture.commentConsumer()
	require.NoError(t, consumer.HandleEvent(ctx, event), "index A's comment mentioning B and D")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	requireStoredMentionFacets(t, fixture.db, commentURI, post.authorDID, mentionedDID)
	require.NoError(t, consumer.HandleEvent(ctx, event), "the identical create event must be idempotent")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'postReply' AND record_uri = $2`, post.authorDID, commentURI),
		"B must have exactly one postReply after duplicate delivery")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`, post.authorDID, commentURI),
		"B's reply notification must not also be a mention")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`, mentionedDID, commentURI),
		"D must have exactly one mention after duplicate delivery")
	require.Equal(t, 2, countRows(t, fixture.db,
		`SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"a duplicate delivery must leave exactly B's reply and D's mention")
}

func TestCommentConsumer_MentionStaleRevReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	post := fixture.posts[0]
	mentionedDID := "did:plc:" + testkit.UniqueID(t) + "mentioned"
	mentionedHandle := testkit.UniqueID(t) + "mentioned.test"
	staleMentionDID := "did:plc:" + testkit.UniqueID(t) + "stale"
	staleMentionHandle := testkit.UniqueID(t) + "stale.test"
	for _, recipient := range []struct{ did, handle string }{
		{mentionedDID, mentionedHandle},
		{staleMentionDID, staleMentionHandle},
	} {
		_, err := fixture.db.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			recipient.did, recipient.handle, bridgedTestNativePDS)
		require.NoError(t, err, "index D and E so a stale mention would be deliverable")
	}
	var postAuthorHandle string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT handle FROM users WHERE did = $1`, post.authorDID).Scan(&postAuthorHandle))
	createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
	content := "A replies to @" + postAuthorHandle + " and @" + mentionedHandle
	commentRecord := revCommentRecord(content, post.uri, post.cid, post.uri, post.cid)
	commentRecord["createdAt"] = createdAt
	commentRecord["facets"] = []interface{}{
		commentMentionFacet(t, content, postAuthorHandle, post.authorDID),
		commentMentionFacet(t, content, mentionedHandle, mentionedDID),
	}
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	olderRev, newerRev := testkit.TID(), testkit.TID()
	require.Less(t, olderRev, newerRev, "fixture: repo revisions must be lexicographically ordered")
	const newerCID = "bafyreicommentmentionnewer"
	consumer := fixture.commentConsumer()
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, newerRev, newerCID,
		time.Now().UnixMicro(), commentRecord,
	)), "index A's newer comment mentioning B and D")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	requireStoredMentionFacets(t, fixture.db, commentURI, post.authorDID, mentionedDID)

	type notificationSnapshot struct {
		recordCID string
		sortAt    time.Time
	}
	readNotifications := func() map[string]notificationSnapshot {
		t.Helper()
		rows, err := fixture.db.QueryContext(ctx, `
			SELECT recipient_did, reason, record_cid, sort_at FROM notifications WHERE record_uri = $1`, commentURI)
		require.NoError(t, err)
		defer rows.Close()
		got := make(map[string]notificationSnapshot)
		for rows.Next() {
			var recipientDID, reason string
			var snapshot notificationSnapshot
			require.NoError(t, rows.Scan(&recipientDID, &reason, &snapshot.recordCID, &snapshot.sortAt))
			got[recipientDID+"/"+reason] = snapshot
		}
		require.NoError(t, rows.Err())
		return got
	}
	before := readNotifications()
	var storedRev string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI).Scan(&storedRev))
	require.Equal(t, newerRev, storedRev)
	var firstCommentCID string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT cid FROM comments WHERE uri = $1`, commentURI).Scan(&firstCommentCID))
	require.Equal(t, newerCID, firstCommentCID)

	staleContent := content + " and @" + staleMentionHandle
	staleRecord := revCommentRecord(staleContent, post.uri, post.cid, post.uri, post.cid)
	staleRecord["createdAt"] = createdAt
	staleRecord["facets"] = []interface{}{
		commentMentionFacet(t, staleContent, postAuthorHandle, post.authorDID),
		commentMentionFacet(t, staleContent, mentionedHandle, mentionedDID),
		commentMentionFacet(t, staleContent, staleMentionHandle, staleMentionDID),
	}
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, olderRev, "bafyreicommentmentionolder",
		time.Now().Add(time.Minute).UnixMicro(), staleRecord,
	)), "a stale create with an additional mention must be ignored")
	require.Zero(t, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND record_uri = $2`, staleMentionDID, commentURI),
		"E is indexed and eligible, but only a stale revision mentions E")
	after := readNotifications()
	require.Equal(t, before, after, "B's and D's notification CIDs and sort_at must survive the stale replay")
	var replayCommentCID, replayRev string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT cid FROM comments WHERE uri = $1`, commentURI).Scan(&replayCommentCID))
	require.Equal(t, firstCommentCID, replayCommentCID, "the stale CID must not replace the stored comment")
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, commentURI).Scan(&replayRev))
	require.Equal(t, storedRev, replayRev, "the older revision must not replace R2")
	require.Len(t, after, 2, "the newer create must leave exactly B's reply and D's mention")
	require.Contains(t, after, post.authorDID+"/postReply")
	require.Contains(t, after, mentionedDID+"/mention")
	require.Equal(t, newerCID, after[post.authorDID+"/postReply"].recordCID)
	require.Equal(t, newerCID, after[mentionedDID+"/mention"].recordCID)
}

func unsupportedParentMentionEvent(t *testing.T, ctx context.Context, db *sql.DB, fixture commentErasureFixture) (*JetstreamEvent, string) {
	t.Helper()
	var recipientHandle string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT handle FROM users WHERE did = $1`, fixture.recipientDID).Scan(&recipientHandle),
		"fixture: B must be indexed before the mention")
	content := "A mentions @" + recipientHandle + " in another collection"
	parentURI := "at://" + fixture.recipientDID + "/app.bsky.feed.post/" + testkit.TID()
	record := revCommentRecord(content, fixture.postURI, fixture.postCID, parentURI, "bafyreicommentmentionparent")
	record["createdAt"] = fixture.createdAt
	record["facets"] = []interface{}{commentMentionFacet(t, content, recipientHandle, fixture.recipientDID)}
	key := testkit.TID()
	return revCommitEvent(fixture.actorDID, CommentCollection, "create", key,
			testkit.TID(), "bafyreicommentmentionunsupportedbranch", time.Now().UnixMicro(), record),
		"at://" + fixture.actorDID + "/" + CommentCollection + "/" + key
}

func TestCommentConsumer_MentionUnsupportedParentNotificationFailureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newCommentErasureFixture(t, ctx, db)
	event, commentURI := unsupportedParentMentionEvent(t, ctx, db, fixture)
	injectedError := errors.New("injected unsupported-parent mention write failure")
	failingRepository := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(db),
		failure:  injectedError,
	}
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(failingRepository))
	err := consumer.HandleEvent(ctx, event)
	assert.ErrorIs(t, err, injectedError, "a failed mention write on the early-commit branch must fail the delivery")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"the comment must roll back with the failed mention")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, commentURI),
		"the failed delivery must not claim the revision")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
	if assert.Len(t, failingRepository.intents, 1, "fixture: the eligible B mention must reach ApplyTx") {
		assert.Equal(t, fixture.recipientDID, failingRepository.intents[0].RecipientDID)
		assert.Equal(t, notifications.ReasonMention, failingRepository.intents[0].Reason)
	}
}

func TestCommentConsumer_MentionUnsupportedParentErasure_DeleteFirstWaitsBeforeContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	fixture := newCommentErasureFixture(t, ctx, db)
	priorEvent, priorURI := fixture.reply()
	require.NoError(t, fixture.consumer.HandleEvent(ctx, priorEvent))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, priorURI),
		"fixture: A has content before deletion starts")
	event, commentURI := unsupportedParentMentionEvent(t, ctx, db, fixture)
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, fixture.actorDID)
	deleteResults := make(chan error, 1)
	go func() { deleteResults <- postgres.NewUserRepository(db).Delete(ctx, fixture.actorDID) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")

	consumerResults := make(chan error, 1)
	go func() { consumerResults <- fixture.consumer.HandleEvent(ctx, event) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var waitingBeforeContent bool
		err := transaction.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				JOIN pg_locks holder ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
					AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
					AND NOT waiter.granted AND waiter.mode = 'ShareLock'
					AND NOT EXISTS (SELECT 1 FROM pg_locks other
						WHERE other.pid = waiter.pid AND NOT other.granted
							AND other.locktype IN ('tuple', 'transactionid'))
					AND NOT EXISTS (SELECT 1 FROM pg_locks content
						WHERE content.pid = waiter.pid
							AND content.relation IN ('comments'::regclass, 'posts'::regclass))
			)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("unsupported-parent comment waiting for Delete's erasure lock before touching comments or posts"))

	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(A unsupported parent)")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1`, fixture.actorDID),
		"Delete-first interleaving must leave no notifications from erased A")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'mention' AND record_uri = $2`, fixture.recipientDID, commentURI),
		"the unsupported-parent comment must not mention B after A is erased")
}
