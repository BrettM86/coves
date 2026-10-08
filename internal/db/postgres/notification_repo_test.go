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

func TestNotificationRepository_LookupsTx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	authorDID := "did:plc:notificationauthor" + uniqueID
	communityDID := "did:plc:notificationcommunity" + uniqueID
	createTestUser(t, db, "notificationauthor"+uniqueID+".test", authorDID)
	createTestCommunity(t, db, communityDID, "c.notification"+uniqueID, authorDID)
	postURI := "at://" + communityDID + "/social.coves.community.post/legacy"
	_, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		postURI, "bafyrenotificationlegacy", "legacy", authorDID, communityDID, "Legacy post")
	require.NoError(t, err, "seed a legacy post whose author is not its URI authority")

	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	lookups := NewNotificationRepository(db).LookupsTx(transaction)
	postAuthorDID, found, err := lookups.LegacyPostAuthor(ctx, postURI)
	require.NoError(t, err)
	require.True(t, found, "LegacyPostAuthor must find the indexed legacy post")
	require.Equal(t, authorDID, postAuthorDID, "legacy post author is posts.author_did, not the URI's community DID")
	postAuthorDID, found, err = lookups.LegacyPostAuthor(ctx,
		"at://"+communityDID+"/social.coves.community.post/missing")
	require.NoError(t, err)
	require.False(t, found, "a missing legacy post has no author")
	require.Empty(t, postAuthorDID)
}

func TestNotificationRepository_ApplyTx_ReplyIntentMappingAndIdempotency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		reason notifications.Reason
	}{
		{"postReply", notifications.ReasonPostReply},
		{"commentReply", notifications.ReasonCommentReply},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db := testkit.DB(t)
			uniqueID := testkit.UniqueID(t)
			recipientDID := "did:plc:notificationrecipient" + uniqueID
			actorDID := "did:plc:notificationactor" + uniqueID // Deliberately no users row.
			createTestUser(t, db, "notificationrecipient"+uniqueID+".test", recipientDID)
			createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
			rootPostURI := "at://" + recipientDID + "/social.coves.community.postv2/root"
			subjectURI := rootPostURI
			if test.reason == notifications.ReasonCommentReply {
				subjectURI = "at://" + recipientDID + "/social.coves.community.comment/parent"
			}
			intent := notifications.Intent{
				Reason: test.reason, RecipientDID: recipientDID, ActorDID: actorDID,
				RecordURI: "at://" + actorDID + "/social.coves.community.comment/reply",
				RecordCID: "bafyrenotificationreply", SubjectURI: subjectURI,
				RootPostURI: rootPostURI, RecordCreatedAt: createdAt,
			}
			repository := NewNotificationRepository(db)

			transaction, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer transaction.Rollback()
			// sort_at is the write statement's clock_timestamp(), so it falls between
			// clock reads taken around the first ApplyTx and after the transaction's
			// now(), which a transaction-start sort_at would equal.
			var firstTransactionTime, clockBeforeWrite, clockAfterWrite time.Time
			require.NoError(t, transaction.QueryRowContext(ctx, `SELECT now(), clock_timestamp()`).
				Scan(&firstTransactionTime, &clockBeforeWrite))
			require.Truef(t, clockBeforeWrite.After(firstTransactionTime),
				"fixture: clock read %s must follow transaction start %s", clockBeforeWrite, firstTransactionTime)
			require.NoError(t, repository.ApplyTx(ctx, transaction, []notifications.Intent{intent}))
			require.NoError(t, transaction.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&clockAfterWrite))
			require.NoError(t, repository.ApplyTx(ctx, transaction, []notifications.Intent{intent}),
				"reapplying a reply intent in the same transaction must be idempotent")
			require.NoError(t, transaction.Commit())

			var count int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM notifications`).Scan(&count))
			require.Equal(t, 1, count, "missing %s notification after committing the intent twice", test.reason)
			var recipient, reason, actor, recordURI, recordCID, subject, rootPost string
			var storedCreatedAt, sortAt time.Time
			require.NoError(t, db.QueryRowContext(ctx, `
				SELECT recipient_did, reason, actor_did, record_uri, record_cid, subject_uri,
				       root_post_uri, record_created_at, sort_at FROM notifications`,
			).Scan(&recipient, &reason, &actor, &recordURI, &recordCID, &subject, &rootPost,
				&storedCreatedAt, &sortAt))
			require.Equal(t, recipientDID, recipient)
			require.Equal(t, string(test.reason), reason)
			require.Equal(t, actorDID, actor)
			require.Equal(t, intent.RecordURI, recordURI)
			require.Equal(t, intent.RecordCID, recordCID)
			require.Equal(t, subjectURI, subject)
			require.Equal(t, rootPostURI, rootPost)
			require.Truef(t, storedCreatedAt.Equal(createdAt),
				"record_created_at = %s, want record createdAt %s", storedCreatedAt, createdAt)
			require.Falsef(t, sortAt.Before(clockBeforeWrite) || sortAt.After(clockAfterWrite),
				"sort_at = %s, want the first write's clock_timestamp() in [%s, %s]",
				sortAt, clockBeforeWrite, clockAfterWrite)
			firstSortAt := sortAt

			laterTransaction, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer laterTransaction.Rollback()
			require.NoError(t, repository.ApplyTx(ctx, laterTransaction, []notifications.Intent{intent}),
				"reapplying a committed intent in a later transaction must be idempotent")
			require.NoError(t, laterTransaction.Commit())
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM notifications`).Scan(&count))
			require.Equal(t, 1, count, "reapplying the same reply must leave exactly one row")
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT sort_at FROM notifications WHERE recipient_did = $1 AND reason = $2 AND record_uri = $3`,
				recipientDID, test.reason, intent.RecordURI).Scan(&sortAt))
			require.Truef(t, sortAt.Equal(firstSortAt),
				"a duplicate reply must preserve the first write's sort_at: got %s, want %s",
				sortAt, firstSortAt)
		})
	}
}

func TestNotificationRepository_ApplyTx_PropagatesConstraintViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	recipientDID := "did:plc:notificationrecipient" + uniqueID
	createTestUser(t, db, "notificationrecipient"+uniqueID+".test", recipientDID)
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	postURI := "at://" + recipientDID + "/social.coves.community.postv2/post"
	err = NewNotificationRepository(db).ApplyTx(ctx, transaction, []notifications.Intent{{
		Reason: "bogus", RecipientDID: recipientDID, ActorDID: "did:plc:notificationactor" + uniqueID,
		RecordURI: "at://did:plc:notificationactor" + uniqueID + "/social.coves.community.comment/reply",
		RecordCID: "bafyrenotificationreply", SubjectURI: postURI, RootPostURI: postURI,
		RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}})
	require.Error(t, err, "ApplyTx must return the notifications CHECK-constraint violation")
	var postgresError *pq.Error
	require.True(t, errors.As(err, &postgresError), "the returned error must wrap the Postgres error")
	require.Equal(t, pq.ErrorCode("23514"), postgresError.Code)
}

func TestNotificationRepository_ApplyTx_RejectsReplyWithoutSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	recipientDID := "did:plc:notificationrecipient" + uniqueID
	actorDID := "did:plc:notificationactor" + uniqueID
	createTestUser(t, db, "notificationrecipient"+uniqueID+".test", recipientDID)
	recordURI := "at://" + actorDID + "/social.coves.community.comment/reply"
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	applyError := NewNotificationRepository(db).ApplyTx(ctx, transaction, []notifications.Intent{{
		Reason: notifications.ReasonPostReply, RecipientDID: recipientDID, ActorDID: actorDID,
		RecordURI: recordURI, RecordCID: "bafyrenotificationreply",
		RootPostURI:     "at://" + recipientDID + "/social.coves.community.postv2/root",
		RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}})
	commitError := transaction.Commit()

	var storedCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM notifications WHERE record_uri = $1`, recordURI).Scan(&storedCount))
	require.Zero(t, storedCount, "a reply intent without a subject must not be stored with subject_uri = ''")
	require.Error(t, applyError, "ApplyTx must bind an empty SubjectURI as NULL so the reply CHECK rejects it")
	var postgresError *pq.Error
	require.True(t, errors.As(applyError, &postgresError), "the returned error must wrap the Postgres error")
	require.Equal(t, pq.ErrorCode("23514"), postgresError.Code)
	require.Equal(t, "notifications_check", postgresError.Constraint,
		"the per-reason shape CHECK, not the reason CHECK, must reject the intent")
	require.Error(t, commitError, "the transaction whose intent violated the CHECK must not commit")
}

func TestNotificationRepository_ApplyTx_StoresMentionWithoutSubjectAsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	recipientDID := "did:plc:notificationrecipient" + uniqueID
	actorDID := "did:plc:notificationactor" + uniqueID
	createTestUser(t, db, "notificationrecipient"+uniqueID+".test", recipientDID)
	recordURI := "at://" + actorDID + "/social.coves.community.comment/mention"
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, NewNotificationRepository(db).ApplyTx(ctx, transaction, []notifications.Intent{{
		Reason: notifications.ReasonMention, RecipientDID: recipientDID, ActorDID: actorDID,
		RecordURI: recordURI, RecordCID: "bafyrenotificationmention",
		RootPostURI:     "at://" + recipientDID + "/social.coves.community.postv2/root",
		RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}}), "a mention intent has no subject; its empty SubjectURI must satisfy the mention CHECK as NULL")
	require.NoError(t, transaction.Commit())

	var subjectURI sql.NullString
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT subject_uri FROM notifications WHERE record_uri = $1`, recordURI).Scan(&subjectURI))
	require.False(t, subjectURI.Valid, "an empty SubjectURI must be stored as NULL, got %q", subjectURI.String)
}
