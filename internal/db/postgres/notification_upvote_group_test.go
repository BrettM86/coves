//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type upvoteGroupFixture struct {
	ctx               context.Context
	db                *sql.DB
	repository        notifications.Repository
	recipientDID      string
	otherRecipientDID string
	subjectURI        string
	rootPostURI       string
}

func newUpvoteGroupFixture(t *testing.T) upvoteGroupFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	recipientDID := "did:plc:" + uniqueID + "recipient"
	otherRecipientDID := "did:plc:" + uniqueID + "other"
	createTestUser(t, db, uniqueID+"recipient.test", recipientDID)
	createTestUser(t, db, uniqueID+"other.test", otherRecipientDID)
	return upvoteGroupFixture{
		ctx: ctx, db: db, repository: NewNotificationRepository(db),
		recipientDID: recipientDID, otherRecipientDID: otherRecipientDID,
		subjectURI:  "at://" + recipientDID + "/social.coves.community.comment/subject",
		rootPostURI: "at://" + recipientDID + "/social.coves.community.postv2/root",
	}
}

func (fixture upvoteGroupFixture) bump() notifications.UpvoteGroupIntent {
	return notifications.UpvoteGroupIntent{
		Action: notifications.UpvoteGroupBump, RecipientDID: fixture.recipientDID,
		SubjectURI: fixture.subjectURI, RootPostURI: fixture.rootPostURI,
	}
}

// clockTimestamp reads clock_timestamp() through queryer. Reads taken around a
// notification write bound its sort_at, which is the write statement's
// clock_timestamp() rather than the transaction's now().
func clockTimestamp(t *testing.T, ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
},
) time.Time {
	t.Helper()
	var clock time.Time
	require.NoError(t, queryer.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&clock))
	return clock
}

func requireSortAtWithin(t *testing.T, sortAt, notBefore, notAfter time.Time) {
	t.Helper()
	require.Falsef(t, sortAt.Before(notBefore) || sortAt.After(notAfter),
		"sort_at = %s, want the write's clock_timestamp() in [%s, %s]", sortAt, notBefore, notAfter)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_CreatesGroupInCallerTransaction(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	transaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	var transactionTime, clockBeforeWrite time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT now(), clock_timestamp()`).
		Scan(&transactionTime, &clockBeforeWrite))
	require.Truef(t, clockBeforeWrite.After(transactionTime),
		"fixture: clock read %s must follow transaction start %s", clockBeforeWrite, transactionTime)
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, fixture.bump()))
	clockAfterWrite := clockTimestamp(t, fixture.ctx, transaction)

	var count int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT count(*) FROM notifications`).Scan(&count))
	require.Equal(t, 1, count, "the caller transaction must see one upvote group")
	var recipientDID, reason, subjectURI, rootPostURI string
	var recordURI, recordCID, actorDID sql.NullString
	var recordCreatedAt sql.NullTime
	var sortAt time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT recipient_did, reason, subject_uri, root_post_uri,
		       record_uri, record_cid, actor_did, record_created_at, sort_at
		FROM notifications`).Scan(&recipientDID, &reason, &subjectURI, &rootPostURI,
		&recordURI, &recordCID, &actorDID, &recordCreatedAt, &sortAt))
	require.Equal(t, fixture.recipientDID, recipientDID)
	require.Equal(t, string(notifications.ReasonUpvote), reason)
	require.Equal(t, fixture.subjectURI, subjectURI)
	require.Equal(t, fixture.rootPostURI, rootPostURI)
	require.False(t, recordURI.Valid, "a group has no record URI")
	require.False(t, recordCID.Valid, "a group has no record CID")
	require.False(t, actorDID.Valid, "a group has no actor")
	require.False(t, recordCreatedAt.Valid, "a group has no record creation time")
	requireSortAtWithin(t, sortAt, clockBeforeWrite, clockAfterWrite)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_RaisesSortWithoutReplacingGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	firstTransaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer firstTransaction.Rollback()
	var firstTime time.Time
	require.NoError(t, firstTransaction.QueryRowContext(fixture.ctx, `SELECT now()`).Scan(&firstTime))
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, firstTransaction, fixture.bump()))
	require.NoError(t, firstTransaction.Commit())
	var firstID int64
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT id FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&firstID), "first bump must create the group")

	secondTransaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer secondTransaction.Rollback()
	var secondTime, clockBeforeWrite time.Time
	require.NoError(t, secondTransaction.QueryRowContext(fixture.ctx, `SELECT now(), clock_timestamp()`).
		Scan(&secondTime, &clockBeforeWrite))
	require.True(t, secondTime.After(firstTime), "the second transaction must begin later: first %s, second %s", firstTime, secondTime)
	require.Truef(t, clockBeforeWrite.After(secondTime),
		"fixture: clock read %s must follow transaction start %s", clockBeforeWrite, secondTime)
	intent := fixture.bump()
	intent.RootPostURI = "at://" + fixture.otherRecipientDID + "/social.coves.community.postv2/different"
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, secondTransaction, intent))
	clockAfterWrite := clockTimestamp(t, fixture.ctx, secondTransaction)
	require.NoError(t, secondTransaction.Commit())

	var count int
	var storedID int64
	var storedRoot string
	var sortAt time.Time
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT count(*), min(id), min(root_post_uri), min(sort_at)
		FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&count, &storedID, &storedRoot, &sortAt))
	require.Equal(t, 1, count, "a later bump must not insert a second group")
	require.Equal(t, firstID, storedID, "a later bump must retain the group's id")
	require.Equal(t, fixture.rootPostURI, storedRoot, "the first root_post_uri must survive a conflicting bump")
	requireSortAtWithin(t, sortAt, clockBeforeWrite, clockAfterWrite)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_EarlierTransactionCannotMoveSortBackwards(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	earlyTransaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer earlyTransaction.Rollback()
	var earlyTime time.Time
	require.NoError(t, earlyTransaction.QueryRowContext(fixture.ctx, `SELECT now()`).Scan(&earlyTime))

	lateTransaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer lateTransaction.Rollback()
	var lateTime, lateClockBeforeWrite time.Time
	require.NoError(t, lateTransaction.QueryRowContext(fixture.ctx, `SELECT now(), clock_timestamp()`).
		Scan(&lateTime, &lateClockBeforeWrite))
	require.True(t, earlyTime.Before(lateTime), "early now() %s must precede late now() %s", earlyTime, lateTime)
	require.Truef(t, lateClockBeforeWrite.After(lateTime),
		"fixture: clock read %s must follow transaction start %s", lateClockBeforeWrite, lateTime)
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, lateTransaction, fixture.bump()))
	lateClockAfterWrite := clockTimestamp(t, fixture.ctx, lateTransaction)
	require.NoError(t, lateTransaction.Commit())
	var lateGroupID int64
	var lateSortAt time.Time
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT id, sort_at FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&lateGroupID, &lateSortAt), "late bump must create the group")
	requireSortAtWithin(t, lateSortAt, lateClockBeforeWrite, lateClockAfterWrite)
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, earlyTransaction, fixture.bump()))
	require.NoError(t, earlyTransaction.Commit())

	var count int
	var storedID int64
	var sortAt time.Time
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT count(*), min(id), min(sort_at) FROM notifications
		WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&count, &storedID, &sortAt))
	require.Equal(t, 1, count)
	require.Equal(t, lateGroupID, storedID)
	require.Falsef(t, sortAt.Before(lateSortAt),
		"early transaction must not lower sort_at: got %s, want at least the late write's %s", sortAt, lateSortAt)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_ZeroActionDoesNotCreateGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	intent := fixture.bump()
	intent.Action = notifications.UpvoteGroupNoChange
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, intent))
	var count int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT count(*) FROM notifications`).Scan(&count))
	require.Zero(t, count, "the zero action must not create a group")
}

func TestNotificationRepository_ApplyUpvoteGroupTx_ZeroActionPreservesExistingSort(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	pastTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err := fixture.db.ExecContext(fixture.ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $3, $4)`,
		fixture.recipientDID, fixture.subjectURI, fixture.rootPostURI, pastTime)
	require.NoError(t, err)
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	intent := fixture.bump()
	intent.Action = notifications.UpvoteGroupNoChange
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, intent))
	var count int
	var sortAt time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT count(*), min(sort_at) FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&count, &sortAt))
	require.Equal(t, 1, count)
	require.Truef(t, sortAt.Equal(pastTime), "zero action changed sort_at from %s to %s", pastTime, sortAt)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DifferentRecipientGetsSeparateGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	_, err := fixture.db.ExecContext(fixture.ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $3)`, fixture.recipientDID, fixture.subjectURI, fixture.rootPostURI)
	require.NoError(t, err)
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	intent := fixture.bump()
	intent.RecipientDID = fixture.otherRecipientDID
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, intent))
	var count int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT count(*) FROM notifications WHERE reason = 'upvote' AND subject_uri = $1`, fixture.subjectURI).Scan(&count))
	require.Equal(t, 2, count, "a different recipient needs a separate group for the same subject")
	var otherRoot string
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT root_post_uri FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.otherRecipientDID, fixture.subjectURI).Scan(&otherRoot))
	require.Equal(t, fixture.rootPostURI, otherRoot)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DoesNotChangeRecordKeyedNotification(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	pastTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err := fixture.db.ExecContext(fixture.ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $3, $4)`, fixture.recipientDID, fixture.subjectURI, fixture.rootPostURI, pastTime)
	require.NoError(t, err)
	recordURI := "at://" + fixture.otherRecipientDID + "/social.coves.community.comment/reply"
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, fixture.repository.ApplyTx(fixture.ctx, transaction, []notifications.Intent{{
		Reason: notifications.ReasonPostReply, RecipientDID: fixture.recipientDID,
		ActorDID: fixture.otherRecipientDID, RecordURI: recordURI, RecordCID: "bafyrenotificationupvotereply",
		SubjectURI: fixture.subjectURI, RootPostURI: fixture.rootPostURI,
		RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}}))
	var replyID int64
	var replySortAt time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT id, sort_at FROM notifications WHERE reason = 'postReply' AND record_uri = $1`, recordURI).Scan(&replyID, &replySortAt))
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, fixture.bump()))
	var recordCount, groupCount int
	var storedReplyID int64
	var storedReplySortAt time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT count(*), min(id), min(sort_at) FROM notifications WHERE reason = 'postReply' AND record_uri = $1`,
		recordURI).Scan(&recordCount, &storedReplyID, &storedReplySortAt))
	require.Equal(t, 1, recordCount, "the record-keyed reply must remain distinct from the upvote group")
	require.Equal(t, replyID, storedReplyID)
	require.True(t, storedReplySortAt.Equal(replySortAt), "a group bump must not update the reply's sort_at")
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT count(*) FROM notifications WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&groupCount))
	require.Equal(t, 1, groupCount, "a record-keyed reply must not count as an upvote group")
}

// A recipient erased after the fan-out read its users row makes the upsert
// fail with 23503 on notifications_recipient_did_fkey. The write is skipped,
// not returned, so the vote transaction commits instead of dead-lettering.
func TestNotificationRepository_ApplyUpvoteGroupTx_MissingRecipientIsSkippedAndKeepsCallerWrites(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	transaction, err := fixture.db.BeginTx(fixture.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	// The valid recipient's group stands for the caller's earlier writes in
	// the vote transaction, which skipping the missing recipient must not discard.
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, fixture.bump()))
	intent := fixture.bump()
	intent.RecipientDID += "missing"
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, intent),
		"only the missing recipient's foreign-key violation may be skipped")

	var answer int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT 1`).Scan(&answer),
		"the transaction must remain usable after the skipped foreign-key violation")
	require.Equal(t, 1, answer)
	var missingCount int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT count(*) FROM notifications WHERE recipient_did = $1`, intent.RecipientDID).Scan(&missingCount))
	require.Zero(t, missingCount, "the missing recipient must get no upvote group")
	require.NoError(t, transaction.Commit())
	var committedCount int
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT count(*) FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&committedCount))
	require.Equal(t, 1, committedCount, "the caller's earlier group write must commit")
}

func TestNotificationRepository_ApplyUpvoteGroupTx_PropagatesOtherDatabaseErrors(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	intent := fixture.bump()
	intent.SubjectURI += "\x00"
	err = fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction, intent)
	require.Error(t, err, "an error other than the recipient foreign-key violation must be returned")
	var postgresError *pq.Error
	require.True(t, errors.As(err, &postgresError), "the error must wrap the Postgres error")
	require.Equal(t, pq.ErrorCode("22021"), postgresError.Code, "a NUL byte in text is an invalid byte sequence")
}
