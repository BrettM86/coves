//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_RecipientFactsReadsUncommittedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	actorDID := "did:plc:" + uniqueID + "actor"
	unrelatedDID := "did:plc:" + uniqueID + "unrelated"
	missingDID := "did:plc:" + uniqueID + "missing"
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()

	// Only the seven recipients have entries in the expected map. Index A and
	// X too, so R7's block of X is a real unrelated block, not a missing-user case.
	for _, user := range []struct{ did, handle string }{
		{actorDID, uniqueID + "actor.test"},
		{unrelatedDID, uniqueID + "unrelated.test"},
	} {
		_, err = transaction.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			user.did, user.handle, "https://native.pds.test")
		require.NoError(t, err, "index the actor and unrelated block target")
	}

	recipients := []struct {
		name  string
		facts notifications.RecipientFacts
	}{
		{name: "control"},
		{name: "erased", facts: notifications.RecipientFacts{Erased: true}},
		{name: "aggregator", facts: notifications.RecipientFacts{Aggregator: true}},
		{name: "community", facts: notifications.RecipientFacts{Community: true}},
		{name: "blockedbyactor", facts: notifications.RecipientFacts{BlockedWithActor: true}},
		{name: "blocksactor", facts: notifications.RecipientFacts{BlockedWithActor: true}},
		{name: "blocksunrelated"},
	}
	recipientDIDs := make([]string, 0, len(recipients)+1)
	want := make(map[string]notifications.RecipientFacts, len(recipients))
	for _, recipient := range recipients {
		did := "did:plc:" + uniqueID + recipient.name
		pdsURL := "https://" + uniqueID + "." + recipient.name + ".pds.test"
		_, err = transaction.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			did, uniqueID+recipient.name+".test", pdsURL)
		require.NoError(t, err, "index recipient %s", recipient.name)
		recipientDIDs = append(recipientDIDs, did)
		facts := recipient.facts
		facts.PDSURL = pdsURL
		want[did] = facts
	}

	_, err = transaction.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1), ($2)`,
		recipientDIDs[1], missingDID)
	require.NoError(t, err, "seed an erased indexed recipient and a marker without a users row")
	_, err = transaction.ExecContext(ctx,
		`INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
		recipientDIDs[2], "Notification recipient aggregator",
		"at://"+recipientDIDs[2]+"/social.coves.aggregator.service/self", "bafyreirecipientfactsaggregator")
	require.NoError(t, err, "declare an indexed aggregator")
	_, err = transaction.ExecContext(ctx,
		`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		 VALUES ($1, $2, $3, $4, $4, $4, $5, NOW())`,
		recipientDIDs[3], uniqueID+"community.test", "Recipient community", actorDID,
		want[recipientDIDs[3]].PDSURL)
	require.NoError(t, err, "declare an indexed community with the recipient's DID")
	for _, block := range []struct{ blockerDID, blockedDID string }{
		{actorDID, recipientDIDs[4]},
		{recipientDIDs[5], actorDID},
		{recipientDIDs[6], unrelatedDID},
	} {
		_, err = transaction.ExecContext(ctx,
			`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
			block.blockerDID, block.blockedDID,
			"at://"+block.blockerDID+"/social.coves.actor.block/"+testkit.TID(), "bafyreirecipientfactsblock")
		require.NoError(t, err, "index the directional block within the lookup transaction")
	}

	lookups := NewNotificationRepository(db).LookupsTx(transaction)
	emptyFacts, err := lookups.RecipientFacts(ctx, actorDID, []string{})
	require.NoError(t, err)
	require.Empty(t, emptyFacts, "no requested DIDs must yield no recipients")

	recipientDIDs = append(recipientDIDs, missingDID)
	got, err := lookups.RecipientFacts(ctx, actorDID, recipientDIDs)
	require.NoError(t, err)
	require.Equal(t, want, got,
		"the transaction must return exactly the seven indexed recipients, each with its PDS URL, erasure, aggregator, community, and directional block facts")
}
