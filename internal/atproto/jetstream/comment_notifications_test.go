//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentConsumer_WritesReplyNotificationsOnNewInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	authorDID := "did:plc:" + uniqueID + "author"
	commenterDID := "did:plc:" + uniqueID + "commenter"
	communityDID := "did:plc:" + uniqueID + "community"

	for _, user := range []struct{ did, handle string }{
		{authorDID, uniqueID + "author.test"},
		{commenterDID, uniqueID + "commenter.test"},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			user.did, user.handle, bridgedTestNativePDS)
		require.NoError(t, err, "index notification recipient and commenter")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		 VALUES ($1, $2, $3, $4, $4, $4, $5, NOW())`,
		communityDID, uniqueID+"community.test", "Notification test community", authorDID, bridgedTestNativePDS)
	require.NoError(t, err, "index the post's community")

	userService := newMockUserService()
	userService.users[authorDID] = &users.User{DID: authorDID, Handle: uniqueID + "author.test"}
	postConsumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		userService, db,
		WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
	postKey := testkit.TID()
	postURI := pv2URI(authorDID, postKey)
	postCID := "bafyreicommentnotificationpost"
	require.NoError(t, postConsumer.HandleEvent(ctx, pv2Event(
		authorDID, "create", postKey, testkit.TID(), postCID, time.Now().UnixMicro(),
		pv2Record(communityDID, "Reply notification target", "A post to reply to"),
	)), "index B's author-owned post through the post consumer")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1 AND author_did = $2`, postURI, authorDID))

	// Keep the record's display time fixed across all events in this test, after
	// activation but before indexing. The default migration cutoff is NOW().
	var recordCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT NOW() - INTERVAL '1 minute'`).Scan(&recordCreatedAt))
	_, err = db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, recordCreatedAt.Add(-time.Minute))
	require.NoError(t, err, "activate notifications before the test record's createdAt")
	createdAt := recordCreatedAt.UTC().Format(time.RFC3339Nano)

	consumer := NewCommentEventConsumer(
		postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)),
	)
	commentKey := testkit.TID()
	commentURI := "at://" + commenterDID + "/" + CommentCollection + "/" + commentKey
	commentCID := "bafyreicommentnotificationreply"
	commentRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	commentRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		commenterDID, CommentCollection, "create", commentKey, testkit.TID(), commentCID,
		time.Now().UnixMicro(), commentRecord,
	)), "index A's top-level reply")
	var clockAfterIndex time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&clockAfterIndex))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: A's reply must be indexed before asserting its notification")

	var revisionUpdatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT updated_at FROM jetstream_record_revs WHERE record_uri = $1`, commentURI,
	).Scan(&revisionUpdatedAt), "fixture: the comment must have a rev-gate timestamp")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications`),
		"missing postReply notification for A's newly indexed comment on B's post")

	var recipientDID, reason, actorDID, recordURI, recordCID, subjectURI, rootPostURI string
	var storedCreatedAt, sortAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT recipient_did, reason, actor_did, record_uri, record_cid, subject_uri,
		        root_post_uri, record_created_at, sort_at FROM notifications WHERE record_uri = $1`,
		commentURI,
	).Scan(&recipientDID, &reason, &actorDID, &recordURI, &recordCID, &subjectURI,
		&rootPostURI, &storedCreatedAt, &sortAt))
	assert.Equal(t, authorDID, recipientDID)
	assert.Equal(t, "postReply", reason)
	assert.Equal(t, commenterDID, actorDID)
	assert.Equal(t, commentURI, recordURI)
	assert.Equal(t, commentCID, recordCID)
	assert.Equal(t, postURI, subjectURI)
	assert.Equal(t, postURI, rootPostURI)
	assert.Truef(t, storedCreatedAt.Equal(recordCreatedAt), "record_created_at = %s, want record createdAt %s", storedCreatedAt, recordCreatedAt)
	// The rev gate's updated_at is the index transaction's now(); sort_at is the
	// notification INSERT's clock_timestamp(), taken later in that transaction.
	assert.Truef(t, sortAt.After(revisionUpdatedAt) && !sortAt.After(clockAfterIndex),
		"sort_at = %s, want the INSERT's clock_timestamp() in (index transaction now() %s, %s]",
		sortAt, revisionUpdatedAt, clockAfterIndex)

	// B's own reply to B's post is indexed as the parent comment but must not
	// notify B. The next comment targets this parent, not the root post.
	parentKey := testkit.TID()
	parentURI := "at://" + authorDID + "/" + CommentCollection + "/" + parentKey
	parentCID := "bafyreicommentnotificationparent"
	parentRecord := revCommentRecord("B comments on their own post", postURI, postCID, postURI, postCID)
	parentRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		authorDID, CommentCollection, "create", parentKey, testkit.TID(), parentCID,
		time.Now().UnixMicro(), parentRecord,
	)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, parentURI))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications`),
		"B's own comment on B's post must not create a self-notification")

	replyKey := testkit.TID()
	replyURI := "at://" + commenterDID + "/" + CommentCollection + "/" + replyKey
	replyCID := "bafyreicommentnotificationnested"
	replyRecord := revCommentRecord("A replies to B's comment", postURI, postCID, parentURI, parentCID)
	replyRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		commenterDID, CommentCollection, "create", replyKey, testkit.TID(), replyCID,
		time.Now().UnixMicro(), replyRecord,
	)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, replyURI))
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM notifications`),
		"A's commentReply must add exactly one notification to B's existing postReply")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = $2 AND actor_did = $3 AND record_uri = $4
		  AND record_cid = $5 AND subject_uri = $6 AND root_post_uri = $7`,
		authorDID, "commentReply", commenterDID, replyURI, replyCID, parentURI, postURI),
		"missing commentReply notification targeting B's parent comment")
}

// The migration's default activation is NOW(), so a record timestamped before
// indexing needs an earlier cutoff to be eligible for a notification.
func activatedCommentNotificationTime(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	var createdAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT NOW() - INTERVAL '1 minute'`).Scan(&createdAt))
	_, err := db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, createdAt.Add(-time.Minute))
	require.NoError(t, err)
	return createdAt.UTC().Format(time.RFC3339Nano)
}

func TestCommentConsumer_ReplyNotificationLegacyPostAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	suffix := testkit.UniqueID(t)
	authorDID := "did:plc:" + suffix + "legacyauthor"
	communityDID := "did:plc:" + suffix + "legacycommunity"
	commenterDID := "did:plc:" + suffix + "legacycommenter"
	postKey := testkit.TID()
	postURI := "at://" + communityDID + "/social.coves.community.post/" + postKey
	seedIndexedPost(t, db, postURI, communityDID, authorDID, postKey)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1 AND author_did = $2`, postURI, authorDID),
		"fixture: legacy URI authority is the community; the post author is a user")
	createdAt := activatedCommentNotificationTime(t, db, ctx)

	consumer := NewCommentEventConsumer(
		postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)),
	)
	commentKey := testkit.TID()
	commentURI := "at://" + commenterDID + "/" + CommentCollection + "/" + commentKey
	commentRecord := revCommentRecord("A replies to C's legacy post", postURI, "bafredrivesubject", postURI, "bafredrivesubject")
	commentRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		commenterDID, CommentCollection, "create", commentKey, testkit.TID(), "bafyreplylegacy",
		time.Now().UnixMicro(), commentRecord,
	)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the top-level reply must be indexed")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications`),
		"a top-level reply to a legacy post must notify its author exactly once")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND reason = 'postReply' AND actor_did = $2
			AND record_uri = $3 AND subject_uri = $4 AND root_post_uri = $4`,
		authorDID, commenterDID, commentURI, postURI),
		"the recipient is posts.author_did, never the legacy URI's community DID")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE recipient_did = $1`, communityDID))
}

func TestCommentConsumer_ReplyNotificationDuplicateDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := NewCommentEventConsumer(
		postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)),
	)
	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	commentCID := "bafyreplyduplicate"
	commentRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	commentRecord["createdAt"] = createdAt
	event := revCommitEvent(revTestCommenter, CommentCollection, "create", commentKey,
		testkit.TID(), commentCID, time.Now().UnixMicro(), commentRecord)
	require.NoError(t, consumer.HandleEvent(ctx, event))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the first delivery must index A's comment")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"the first delivery must create one reply notification")
	var firstCID string
	var firstSortAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT record_cid, sort_at FROM notifications WHERE record_uri = $1`, commentURI,
	).Scan(&firstCID, &firstSortAt))
	require.Equal(t, commentCID, firstCID)

	require.NoError(t, consumer.HandleEvent(ctx, event), "an identical create event must be idempotent")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications`),
		"duplicate delivery must leave exactly one notification in the inbox")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"a duplicate delivery must not create a second notification")
	var replayCID string
	var replaySortAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT record_cid, sort_at FROM notifications WHERE record_uri = $1`, commentURI,
	).Scan(&replayCID, &replaySortAt))
	require.Equal(t, firstCID, replayCID, "the duplicate must preserve the first record CID")
	require.True(t, replaySortAt.Equal(firstSortAt), "the duplicate must preserve the first sort_at")
}

func TestCommentConsumer_ReplyNotificationStaleCreateReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := NewCommentEventConsumer(
		postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)),
	)
	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	olderRev := testkit.TID()
	newerRev := testkit.TID()
	require.Less(t, olderRev, newerRev, "fixture: repo revisions must be lexicographically ordered")
	commentRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	commentRecord["createdAt"] = createdAt
	const newerCID = "bafyreplynewer"
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, newerRev, newerCID,
		time.Now().UnixMicro(), commentRecord,
	)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"fixture: the newer create must index A's comment")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"the newer create must write one reply notification")
	var firstCID string
	var firstSortAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT record_cid, sort_at FROM notifications WHERE record_uri = $1`, commentURI,
	).Scan(&firstCID, &firstSortAt))
	require.Equal(t, newerCID, firstCID)

	// The stale revision threads under a different indexed user's post, so a
	// fan-out for it would notify that user instead of deduplicating into B's row.
	const otherAuthorDID = revTestPrefix + "staleotherauthor"
	insertBridgedUser(t, db, otherAuthorDID, "revstaleotherauthor.test")
	otherPostURI := pv2URI(otherAuthorDID, "revstaleotherpost")
	staleRecord := revCommentRecord("A replied to C's post at an older revision",
		otherPostURI, "bafyrevstaleotherpost", otherPostURI, "bafyrevstaleotherpost")
	staleRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentKey, olderRev, "bafyreplyolder",
		time.Now().Add(time.Minute).UnixMicro(), staleRecord,
	)), "an older revision replayed later must not replace the newer notification")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE recipient_did = $1`, otherAuthorDID),
		"a rev-gated stale create must not fan out to the recipient its own threading names")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications`),
		"a stale create must leave exactly one notification in the inbox")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"the stale create must not add another notification")
	var replayCID string
	var replaySortAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT record_cid, sort_at FROM notifications WHERE record_uri = $1`, commentURI,
	).Scan(&replayCID, &replaySortAt))
	require.Equal(t, newerCID, replayCID, "the stale CID must never overwrite the newer CID")
	require.True(t, replaySortAt.Equal(firstSortAt), "the stale replay must preserve the first sort_at")
}

// A thread URI that passes the consumer's lenient validateATURI but not
// syntax.ParseATURI is a payload defect: the comment still indexes as it did
// before notifications existed, and no one is notified.
func TestCommentConsumer_UnparsableThreadURIIndexesWithoutNotification(t *testing.T) {
	t.Parallel()
	const parentAuthorDID = revTestPrefix + "unparsableparent"
	for _, test := range []struct {
		threading func(postURI string) (rootURI, parentURI string)
		name      string
	}{
		{
			name: "top-level reply to a trailing-slash post URI",
			threading: func(postURI string) (string, string) {
				return postURI + "/", postURI + "/"
			},
		},
		{
			name: "nested reply to a trailing-slash parent comment URI",
			threading: func(postURI string) (string, string) {
				return postURI, "at://" + parentAuthorDID + "/" + CommentCollection + "/" + testkit.TID() + "/"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := testkit.DB(t)
			_, postURI, postCID := setupRevFixtures(t, db)
			insertBridgedUser(t, db, parentAuthorDID, "revunparsableparent.test")
			createdAt := activatedCommentNotificationTime(t, db, ctx)
			rootURI, parentURI := test.threading(postURI)
			for _, uri := range []string{rootURI, parentURI} {
				require.NoErrorf(t, validateATURI(uri), "fixture: %s must pass the consumer's validation", uri)
			}
			_, parseErr := syntax.ParseATURI(parentURI)
			require.Error(t, parseErr, "fixture: the parent URI must not parse as a strict AT-URI")

			consumer := NewCommentEventConsumer(
				postgres.NewCommentRepository(db), db,
				WithCommentNotifications(postgres.NewNotificationRepository(db)),
			)
			commentKey := testkit.TID()
			commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
			commentRecord := revCommentRecord("A replies through a malformed thread URI",
				rootURI, postCID, parentURI, "bafyunparsableparent")
			commentRecord["createdAt"] = createdAt
			require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
				revTestCommenter, CommentCollection, "create", commentKey, testkit.TID(), "bafyunparsablereply",
				time.Now().UnixMicro(), commentRecord,
			)), "an unparsable thread URI must not fail indexing")
			require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
				"the comment must index despite its unparsable thread URI")
			require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE recipient_did IN ($1, $2)`,
				revTestAuthor, parentAuthorDID), "no indexed author may be notified through an unparsable thread URI")
			require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI))
		})
	}
}

type failingCommentNotificationRepository struct {
	delegate notifications.Repository
	failure  error
	intents  []notifications.Intent
}

func (repository *failingCommentNotificationRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, actorDID string) (bool, error) {
	return repository.delegate.ErasureGateTx(ctx, tx, actorDID)
}

func (repository *failingCommentNotificationRepository) LookupsTx(tx *sql.Tx) notifications.Lookups {
	return repository.delegate.LookupsTx(tx)
}

func (repository *failingCommentNotificationRepository) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	return repository.delegate.ApplyUpvoteGroupTx(ctx, tx, intent)
}

func (repository *failingCommentNotificationRepository) RecordPostAuthorDeleteWithdrawalTx(ctx context.Context, tx *sql.Tx, postURI string) error {
	return repository.delegate.RecordPostAuthorDeleteWithdrawalTx(ctx, tx, postURI)
}

func (repository *failingCommentNotificationRepository) RepairResurrectedCommentNotificationsTx(ctx context.Context, tx *sql.Tx, recordURI, replySubjectURI, rootPostURI string) error {
	return repository.delegate.RepairResurrectedCommentNotificationsTx(ctx, tx, recordURI, replySubjectURI, rootPostURI)
}

func (repository *failingCommentNotificationRepository) DeleteReplyRecipientMentionsTx(ctx context.Context, tx *sql.Tx, recordURI string) error {
	return repository.delegate.DeleteReplyRecipientMentionsTx(ctx, tx, recordURI)
}

func (repository *failingCommentNotificationRepository) ReplaceUpvoteGroupRootTx(ctx context.Context, tx *sql.Tx, recipientDID, subjectURI, rootPostURI string) error {
	return repository.delegate.ReplaceUpvoteGroupRootTx(ctx, tx, recipientDID, subjectURI, rootPostURI)
}

func (repository *failingCommentNotificationRepository) ApplyTx(_ context.Context, _ *sql.Tx, intents []notifications.Intent) error {
	repository.intents = intents
	return repository.failure
}

func TestCommentConsumer_NotificationWriteFailureRollsBackCommentAndRevForRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)

	var originalCommentCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT comment_count FROM posts WHERE uri = $1`, postURI,
	).Scan(&originalCommentCount), "fixture: B's post must exist")

	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	commentRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	commentRecord["createdAt"] = createdAt
	event := revCommitEvent(revTestCommenter, CommentCollection, "create", commentKey,
		testkit.TID(), "bafyreplyrollback", time.Now().UnixMicro(), commentRecord)

	injectedError := errors.New("injected notification write failure")
	failingRepository := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(db),
		failure:  injectedError,
	}
	failingConsumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(failingRepository))
	err := failingConsumer.HandleEvent(ctx, event)
	require.ErrorIs(t, err, injectedError)
	assert.NotErrorIs(t, err, ErrPermanentEvent)
	assert.NotErrorIs(t, err, ErrUnresolvedReference)
	assert.False(t, skipsInlineRetries(err), "a notification write failure must enter the connector's transient retry lane")
	require.Len(t, failingRepository.intents, 1, "fixture: fan-out must request B's postReply before ApplyTx fails")
	assert.Equal(t, revTestAuthor, failingRepository.intents[0].RecipientDID)
	assert.Equal(t, notifications.ReasonPostReply, failingRepository.intents[0].Reason)

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"the comment insert must roll back")
	var commentCountAfterFailure int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT comment_count FROM posts WHERE uri = $1`, postURI,
	).Scan(&commentCountAfterFailure))
	assert.Equal(t, originalCommentCount, commentCountAfterFailure, "the post count must roll back")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, commentURI),
		"the failed delivery must not claim the revision")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"the failed delivery must not leave a notification")

	retryingConsumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)))
	require.NoError(t, retryingConsumer.HandleEvent(ctx, event), "the identical event must succeed on retry")
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	var commentCountAfterRetry int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT comment_count FROM posts WHERE uri = $1`, postURI,
	).Scan(&commentCountAfterRetry))
	assert.Equal(t, originalCommentCount+1, commentCountAfterRetry)
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = $3`,
		commentURI, revTestAuthor, "postReply"), "the retry must notify B exactly once")
}

// Without the activation row the notification cutoff is unknown, so the reply
// must not be indexed without its notification: the whole delivery fails
// transiently and the identical event succeeds once the row is restored.
func TestCommentConsumer_MissingActivationRowFailsIndexingForRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	_, postURI, postCID := setupRevFixtures(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)

	var activatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT activated_at FROM notification_activation`,
	).Scan(&activatedAt), "fixture: the activation row must exist before it is removed")
	result, err := db.ExecContext(ctx, `DELETE FROM notification_activation`)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rowsAffected, "fixture: the singleton activation row must be removed")

	var originalCommentCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT comment_count FROM posts WHERE uri = $1`, postURI,
	).Scan(&originalCommentCount), "fixture: B's post must exist")

	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	commentRecord := revCommentRecord("A replies to B's post", postURI, postCID, postURI, postCID)
	commentRecord["createdAt"] = createdAt
	event := revCommitEvent(revTestCommenter, CommentCollection, "create", commentKey,
		testkit.TID(), "bafyreplymissingactivation", time.Now().UnixMicro(), commentRecord)

	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)))
	err = consumer.HandleEvent(ctx, event)
	require.ErrorIs(t, err, postgres.ErrNotificationActivationMissing)
	assert.NotErrorIs(t, err, ErrPermanentEvent)
	assert.NotErrorIs(t, err, ErrUnresolvedReference)
	assert.False(t, skipsInlineRetries(err), "a missing activation row must enter the connector's transient retry lane")

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"the comment must not be indexed without its notification")
	var commentCountAfterFailure int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT comment_count FROM posts WHERE uri = $1`, postURI,
	).Scan(&commentCountAfterFailure))
	assert.Equal(t, originalCommentCount, commentCountAfterFailure, "the post count must roll back")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, commentURI),
		"the failed delivery must not claim the revision")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
		"the failed delivery must not leave a notification")

	_, err = db.ExecContext(ctx, `INSERT INTO notification_activation (activated_at) VALUES ($1)`, activatedAt)
	require.NoError(t, err, "fixture: restore the activation row")
	require.NoError(t, consumer.HandleEvent(ctx, event), "the identical event must succeed once the row is restored")
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI))
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = $3`,
		commentURI, revTestAuthor, "postReply"), "the retry must notify B exactly once")
}
