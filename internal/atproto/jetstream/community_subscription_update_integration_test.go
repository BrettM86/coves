//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Subscription records are rewritten in place (putRecord) when a viewer changes
// contentVisibility, so the firehose delivers them as update events at the
// existing record URI. These tests drive the community consumer with a real
// Postgres repository and the production rev gate, using increasing revs as one
// repo's successive commits would carry.

const subscriptionCollection = "social.coves.community.subscription"

// subscriptionUpdateFixture indexes one subscriber and the given communities and
// returns a rev-gated community consumer wired to the real repository.
func subscriptionUpdateFixture(t *testing.T, db *sql.DB, userDID string, communityDIDs ...string) *CommunityEventConsumer {
	t.Helper()
	ownerDID := userDID + "owner"
	insertBridgedUser(t, db, userDID, "subupdate-"+testkit.UniqueID(t)+".test")
	insertBridgedUser(t, db, ownerDID, "subupdateowner-"+testkit.UniqueID(t)+".test")
	for _, communityDID := range communityDIDs {
		insertBridgedCommunity(t, db, communityDID, "subupdatecommunity-"+testkit.UniqueID(t)+".test", ownerDID)
	}
	return NewCommunityEventConsumer(postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), "did:web:test.local", true, nil,
		WithCommunityRevGate(NewRevGate(db)))
}

func subscriptionRecord(subject string, contentVisibility interface{}) map[string]interface{} {
	record := map[string]interface{}{
		"$type":     subscriptionCollection,
		"subject":   subject,
		"createdAt": "2026-03-01T03:00:00Z",
	}
	if contentVisibility != nil {
		record["contentVisibility"] = contentVisibility
	}
	return record
}

type indexedSubscription struct {
	communityDID      string
	recordURI         string
	recordCID         string
	contentVisibility int
}

func subscriptionRowsForUser(t *testing.T, db *sql.DB, userDID string) []indexedSubscription {
	t.Helper()
	rows, err := db.Query(`SELECT community_did, record_uri, record_cid, content_visibility
		FROM community_subscriptions WHERE user_did = $1 ORDER BY community_did`, userDID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []indexedSubscription
	for rows.Next() {
		var row indexedSubscription
		require.NoError(t, rows.Scan(&row.communityDID, &row.recordURI, &row.recordCID, &row.contentVisibility))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func subscriptionRowID(t *testing.T, db *sql.DB, userDID, communityDID string) int {
	t.Helper()
	var id int
	require.NoError(t, db.QueryRow(`SELECT id FROM community_subscriptions WHERE user_did = $1 AND community_did = $2`,
		userDID, communityDID).Scan(&id))
	return id
}

func communitySubscriberCount(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT subscriber_count FROM communities WHERE did = $1`, communityDID).Scan(&count))
	return count
}

// J1: an update at an indexed URI for the same community stores the update's
// (clamped or defaulted) value and CID in the existing row (same id, not an
// unsubscribe and re-insert) without moving subscriber_count.
func TestCommunityConsumer_SubscriptionUpdate_SameCommunityStoresNewValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		contentVisibility interface{}
		want              int
	}{
		{name: "value 1 is stored", contentVisibility: float64(1), want: 1},
		{name: "value 9 is clamped to 5", contentVisibility: float64(9), want: 5},
		{name: "missing value defaults to 3", contentVisibility: nil, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()

			id := testkit.UniqueID(t)
			userDID := "did:plc:subupd" + id
			communityDID := "did:plc:subupdc" + id
			cec := subscriptionUpdateFixture(t, db, userDID, communityDID)
			recordURI := "at://" + userDID + "/" + subscriptionCollection + "/3ksubupdate1"
			base := time.Now().UnixMicro()

			require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
				userDID, subscriptionCollection, "create", "3ksubupdate1", revA, "bafysubupdatecreate", base,
				subscriptionRecord(communityDID, float64(2)))))
			require.Equal(t, 1, communitySubscriberCount(t, db, communityDID), "fixture: create indexed")
			createdRowID := subscriptionRowID(t, db, userDID, communityDID)

			require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
				userDID, subscriptionCollection, "update", "3ksubupdate1", revB, "bafysubupdateupdate", base+1_000_000,
				subscriptionRecord(communityDID, tt.contentVisibility))))

			assert.Equal(t, []indexedSubscription{{
				communityDID:      communityDID,
				recordURI:         recordURI,
				recordCID:         "bafysubupdateupdate",
				contentVisibility: tt.want,
			}}, subscriptionRowsForUser(t, db, userDID))
			assert.Equal(t, createdRowID, subscriptionRowID(t, db, userDID, communityDID),
				"a same-community update must update the row in place, not delete and re-insert it")
			assert.Equal(t, 1, communitySubscriberCount(t, db, communityDID), "subscriber_count must not move on an update")
		})
	}
}

// J2: an update at an indexed URI whose subject names a different community is
// a move: the old (user, community) row goes, the new one is indexed at the
// same URI, and both counts follow. Replaying the same update changes nothing.
// The replay carries no rev so it bypasses the rev gate (which would skip an
// equal rev) and actually reaches handleSubscription.
func TestCommunityConsumer_SubscriptionUpdate_SubjectChangeMovesSubscription(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	id := testkit.UniqueID(t)
	userDID := "did:plc:submove" + id
	communityA := "did:plc:submovea" + id
	communityB := "did:plc:submoveb" + id
	cec := subscriptionUpdateFixture(t, db, userDID, communityA, communityB)
	recordURI := "at://" + userDID + "/" + subscriptionCollection + "/3ksubmove1"
	base := time.Now().UnixMicro()

	require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
		userDID, subscriptionCollection, "create", "3ksubmove1", revA, "bafysubmovecreate", base,
		subscriptionRecord(communityA, float64(3)))))
	require.Equal(t, 1, communitySubscriberCount(t, db, communityA), "fixture: create indexed for A")
	require.Equal(t, 0, communitySubscriberCount(t, db, communityB), "fixture: B starts unsubscribed")

	update := revCommitEvent(
		userDID, subscriptionCollection, "update", "3ksubmove1", revB, "bafysubmoveupdate", base+1_000_000,
		subscriptionRecord(communityB, float64(4)))

	wantRows := []indexedSubscription{{
		communityDID:      communityB,
		recordURI:         recordURI,
		recordCID:         "bafysubmoveupdate",
		contentVisibility: 4,
	}}

	require.NoError(t, cec.HandleEvent(ctx, update))
	assert.Equal(t, wantRows, subscriptionRowsForUser(t, db, userDID), "after the update")
	assert.Equal(t, 0, communitySubscriberCount(t, db, communityA), "A's subscriber_count after the update")
	assert.Equal(t, 1, communitySubscriberCount(t, db, communityB), "B's subscriber_count after the update")

	replayCommit := *update.Commit
	replayCommit.Rev = ""
	replay := *update
	replay.Commit = &replayCommit
	require.NoError(t, cec.HandleEvent(ctx, &replay))
	assert.Equal(t, wantRows, subscriptionRowsForUser(t, db, userDID), "after replaying the update")
	assert.Equal(t, 0, communitySubscriberCount(t, db, communityA), "A's subscriber_count after the replay")
	assert.Equal(t, 1, communitySubscriberCount(t, db, communityB), "B's subscriber_count after the replay")
}

// J3: an update whose create was never indexed produces the same row a create
// would have.
func TestCommunityConsumer_SubscriptionUpdate_WithoutIndexedRowIndexesIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	id := testkit.UniqueID(t)
	userDID := "did:plc:subnorow" + id
	communityDID := "did:plc:subnorowc" + id
	cec := subscriptionUpdateFixture(t, db, userDID, communityDID)
	recordURI := "at://" + userDID + "/" + subscriptionCollection + "/3ksubnorow1"

	require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
		userDID, subscriptionCollection, "update", "3ksubnorow1", revB, "bafysubnorowupdate", time.Now().UnixMicro(),
		subscriptionRecord(communityDID, float64(5)))))

	assert.Equal(t, []indexedSubscription{{
		communityDID:      communityDID,
		recordURI:         recordURI,
		recordCID:         "bafysubnorowupdate",
		contentVisibility: 5,
	}}, subscriptionRowsForUser(t, db, userDID))
	assert.Equal(t, 1, communitySubscriberCount(t, db, communityDID), "subscriber_count after the update")
}
