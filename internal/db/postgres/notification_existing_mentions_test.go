//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_ExistingMentionRecipientsReadsOnlyRecordMentionsInTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	actorDID := "did:plc:" + uniqueID + "actor"
	recordURI := "at://" + actorDID + "/social.coves.community.comment/first"
	otherRecordURI := "at://" + actorDID + "/social.coves.community.comment/second"
	rootPostURI := "at://" + actorDID + "/social.coves.community.postv2/root"
	recipients := []string{
		"did:plc:" + uniqueID + "first",
		"did:plc:" + uniqueID + "second",
		"did:plc:" + uniqueID + "reply",
		"did:plc:" + uniqueID + "other",
	}
	for index, recipientDID := range recipients {
		createTestUser(t, db, uniqueID+string(rune('a'+index))+".test", recipientDID)
	}
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	makeIntent := func(recipientDID string, reason notifications.Reason, uri string) notifications.Intent {
		intent := notifications.Intent{
			RecipientDID: recipientDID, Reason: reason, ActorDID: actorDID,
			RecordURI: uri, RecordCID: "bafyreiexistingmentions", RootPostURI: rootPostURI,
			RecordCreatedAt: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
		}
		if reason == notifications.ReasonCommentReply {
			intent.SubjectURI = "at://" + recipients[2] + "/social.coves.community.comment/parent"
		}
		return intent
	}
	repository := NewNotificationRepository(db)
	require.NoError(t, repository.ApplyTx(ctx, transaction, []notifications.Intent{
		makeIntent(recipients[0], notifications.ReasonMention, recordURI),
		makeIntent(recipients[1], notifications.ReasonMention, recordURI),
		makeIntent(recipients[2], notifications.ReasonCommentReply, recordURI),
		makeIntent(recipients[3], notifications.ReasonMention, otherRecordURI),
	}), "seed uncommitted notifications in the lookup's transaction")
	var seeded int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT count(*) FROM notifications`).Scan(&seeded))
	require.Equal(t, 4, seeded, "fixture: all four rows must exist in the writing transaction")

	lookups := repository.LookupsTx(transaction)
	got, err := lookups.ExistingMentionRecipients(ctx, recordURI)
	require.NoError(t, err)
	require.Len(t, got, 2, "only mention recipients for this record consume its budget")
	for index, expectedDID := range recipients[:2] {
		found := false
		for _, actualDID := range got {
			found = found || actualDID == expectedDID
		}
		require.True(t, found, "mention recipient %d must be returned", index)
	}
	got, err = lookups.ExistingMentionRecipients(ctx, "at://"+actorDID+"/social.coves.community.comment/missing")
	require.NoError(t, err)
	require.Empty(t, got, "a record without mentions has no existing recipients")
}
