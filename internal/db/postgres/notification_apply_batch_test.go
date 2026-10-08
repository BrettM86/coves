//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_ApplyTx_OneInsertForManyMentions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	actorDID := "did:plc:" + uniqueID + "actor"
	recordURI := "at://" + actorDID + "/social.coves.community.comment/mention"
	rootPostURI := "at://" + actorDID + "/social.coves.community.postv2/root"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	intents := make([]notifications.Intent, 100)
	for index := range intents {
		recipientDID := fmt.Sprintf("did:plc:%s%03d", uniqueID, index)
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			recipientDID, fmt.Sprintf("%s%03d.test", uniqueID, index), "https://native.pds.test")
		require.NoError(t, err, "index recipient %d", index)
		intents[index] = notifications.Intent{
			Reason: notifications.ReasonMention, RecipientDID: recipientDID, ActorDID: actorDID,
			RecordURI: recordURI, RecordCID: "bafyrenotificationbatch", RootPostURI: rootPostURI,
			RecordCreatedAt: createdAt,
		}
	}

	transaction, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, NewNotificationRepository(db).ApplyTx(ctx, transaction, intents))

	// An inserted tuple's xmin identifies its writing subtransaction, and cmin
	// identifies the INSERT command. Inspect both before ending this transaction.
	var rowCount, writingTransactions, insertCommands int
	require.NoError(t, transaction.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT xmin::text), count(DISTINCT cmin::text)
		FROM notifications WHERE record_uri = $1`, recordURI,
	).Scan(&rowCount, &writingTransactions, &insertCommands))
	require.Equal(t, 100, rowCount)
	require.Equal(t, 1, writingTransactions, "all mention rows must be written in one savepoint")
	require.Equal(t, 1, insertCommands, "all mention rows must be written by one INSERT")
	require.NoError(t, transaction.Commit())
}

func TestNotificationRepository_ApplyTx_MissingRecipientDoesNotAbortOtherIntents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	actorDID := "did:plc:" + uniqueID + "actor"
	recordURI := "at://" + actorDID + "/social.coves.community.comment/mention"
	rootPostURI := "at://" + actorDID + "/social.coves.community.postv2/root"
	firstRecipient := "did:plc:" + uniqueID + "b"
	missingRecipient := "did:plc:" + uniqueID + "x"
	lastRecipient := "did:plc:" + uniqueID + "d"
	for _, recipient := range []struct{ did, handle string }{
		{firstRecipient, uniqueID + "b.test"},
		{lastRecipient, uniqueID + "d.test"},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			recipient.did, recipient.handle, "https://native.pds.test")
		require.NoError(t, err)
	}
	intents := make([]notifications.Intent, 0, 3)
	for _, did := range []string{firstRecipient, missingRecipient, lastRecipient} {
		intents = append(intents, notifications.Intent{
			Reason: notifications.ReasonMention, RecipientDID: did, ActorDID: actorDID,
			RecordURI: recordURI, RecordCID: "bafyrenotificationbatch", RootPostURI: rootPostURI,
			RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
		})
	}

	transaction, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	// The sentinel stands for the caller's earlier writes in the index
	// transaction, which the fallback's rollback of the batch must not discard.
	sentinelDID := "did:plc:" + uniqueID + "s"
	_, err = transaction.ExecContext(ctx,
		`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
		sentinelDID, uniqueID+"s.test", "https://native.pds.test")
	require.NoError(t, err)
	var transactionTime, clockBeforeWrite time.Time
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT now(), clock_timestamp()`).
		Scan(&transactionTime, &clockBeforeWrite))
	require.Truef(t, clockBeforeWrite.After(transactionTime),
		"fixture: clock_timestamp() %s must be after now() %s so a now()-stamped row fails the bound",
		clockBeforeWrite, transactionTime)
	require.NoError(t, NewNotificationRepository(db).ApplyTx(ctx, transaction, intents),
		"only the missing recipient's foreign-key violation may be skipped")
	clockAfterWrite := clockTimestamp(t, ctx, transaction)
	var sentinelInTransaction bool
	require.NoError(t, transaction.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE did = $1)`, sentinelDID).Scan(&sentinelInTransaction))
	require.True(t, sentinelInTransaction,
		"the batch rollback must undo only the batch, not the caller's earlier writes in the transaction")
	rows, err := transaction.QueryContext(ctx,
		`SELECT recipient_did, sort_at FROM notifications WHERE record_uri = $1 ORDER BY recipient_did`, recordURI)
	require.NoError(t, err)
	var recipients []string
	for rows.Next() {
		var did string
		var sortAt time.Time
		require.NoError(t, rows.Scan(&did, &sortAt))
		// The surviving rows come from the per-intent fallback INSERT.
		requireSortAtWithin(t, sortAt, clockBeforeWrite, clockAfterWrite)
		recipients = append(recipients, did)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{firstRecipient, lastRecipient}, recipients,
		"the missing recipient between two valid recipients must leave both valid inserts intact")
	var answer int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT 1`).Scan(&answer),
		"the transaction must remain usable after the foreign-key violation")
	require.Equal(t, 1, answer)
	require.NoError(t, transaction.Commit())
	var sentinelCommitted bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE did = $1)`, sentinelDID).Scan(&sentinelCommitted))
	require.True(t, sentinelCommitted, "the caller's earlier write must commit with the notifications")
}

func TestNotificationRepository_ApplyTx_DuplicateIntentInOneCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	recipientDID := "did:plc:" + uniqueID + "recipient"
	actorDID := "did:plc:" + uniqueID + "actor"
	createTestUser(t, db, uniqueID+".test", recipientDID)
	intent := notifications.Intent{
		Reason: notifications.ReasonMention, RecipientDID: recipientDID, ActorDID: actorDID,
		RecordURI: "at://" + actorDID + "/social.coves.community.comment/mention",
		RecordCID: "bafyrenotificationbatch", RootPostURI: "at://" + actorDID + "/social.coves.community.postv2/root",
		RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
	}
	transaction, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, NewNotificationRepository(db).ApplyTx(ctx, transaction, []notifications.Intent{intent, intent}))
	var rowCount int
	require.NoError(t, transaction.QueryRowContext(ctx,
		`SELECT count(*) FROM notifications WHERE record_uri = $1`, intent.RecordURI).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "duplicate intents in one call must insert only one notification")
	require.NoError(t, transaction.Commit())
}

func TestNotificationRepository_ApplyTx_NilIntents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := testkit.DB(t)
	transaction, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, NewNotificationRepository(db).ApplyTx(ctx, transaction, nil))
	require.NoError(t, transaction.Commit())
}
