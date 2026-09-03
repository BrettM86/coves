//go:build integration

package postgres

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration045RecountsAndMaintainsCommunitySubscribers verifies both the
// one-time repair and the ongoing relationship-backed database invariant.
func TestMigration045RecountsAndMaintainsCommunitySubscribers(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	require.EqualValues(t, 46, testkit.MigrateDownOne(t, db, 46),
		"046 (drop encryption_keys) sits on top of 045 and must be rolled back first")
	require.EqualValues(t, 45, testkit.MigrateDownOne(t, db, 45),
		"this test seeds the record-asserted state migration 045 repairs")

	ctx := context.Background()
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())

	// Down uses IF EXISTS throughout, so a misnamed object would roll back
	// "successfully" and leave the trigger live. Prove it is actually gone.
	require.False(t, subscriberTriggerExists(t, db),
		"045 Down must remove the subscriber-count trigger")
	seedCommunity := func(name string) *communities.Community {
		t.Helper()
		did := "did:plc:m45" + name
		community, err := repo.Create(ctx, &communities.Community{
			DID:          did,
			Handle:       "c-m45-" + name + ".coves.local",
			Name:         "m45-" + name,
			OwnerDID:     did,
			CreatedByDID: "did:plc:m45creator",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		})
		require.NoError(t, err)
		return community
	}

	withRelationships := seedCommunity(testkit.UniqueIDWithPrefix(t, "relationships"))
	for i, subscriberDID := range []string{"did:plc:m45subscriberone", "did:plc:m45subscribertwo"} {
		_, err := repo.SubscribeWithCount(ctx, &communities.Subscription{
			UserDID:           subscriberDID,
			CommunityDID:      withRelationships.DID,
			SubscribedAt:      time.Now().Add(time.Duration(i) * time.Second),
			ContentVisibility: 3,
		})
		require.NoError(t, err)
	}

	withoutRelationships := seedCommunity(testkit.UniqueIDWithPrefix(t, "empty"))
	// The original column definition was nullable. The recount must clear a
	// NULL before SET NOT NULL runs, or the migration fails on real data.
	withNull := seedCommunity(testkit.UniqueIDWithPrefix(t, "null"))
	_, err := db.ExecContext(ctx, `
		UPDATE communities
		SET subscriber_count = CASE did WHEN $1 THEN 2000000000 WHEN $2 THEN 91 WHEN $3 THEN NULL END
		WHERE did IN ($1, $2, $3)
	`, withRelationships.DID, withoutRelationships.DID, withNull.DID)
	require.NoError(t, err)

	testkit.MigrateUp(t, db)
	require.True(t, subscriberTriggerExists(t, db))

	recountedNull, err := repo.GetByDID(ctx, withNull.DID)
	require.NoError(t, err)
	assert.Zero(t, recountedNull.SubscriberCount,
		"a NULL count must be recounted to zero before the column becomes NOT NULL")

	recounted, err := repo.GetByDID(ctx, withRelationships.DID)
	require.NoError(t, err)
	assert.Equal(t, 2, recounted.SubscriberCount,
		"the materialized count must be rebuilt from indexed subscription relationships")

	recountedEmpty, err := repo.GetByDID(ctx, withoutRelationships.DID)
	require.NoError(t, err)
	assert.Zero(t, recountedEmpty.SubscriberCount,
		"a record-asserted count with no supporting relationships must be cleared")

	lateSubscriberDID := "did:plc:m45late" + testkit.UniqueID(t)
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
		VALUES ($1, $2, NOW())
	`, lateSubscriberDID, withRelationships.DID)
	require.NoError(t, err)

	afterInsert, err := repo.GetByDID(ctx, withRelationships.DID)
	require.NoError(t, err)
	assert.Equal(t, 3, afterInsert.SubscriberCount,
		"future relationship inserts must advance the materialized count")
	untouched, err := repo.GetByDID(ctx, withoutRelationships.DID)
	require.NoError(t, err)
	assert.Zero(t, untouched.SubscriberCount,
		"an insert for one community moved another community's count")

	// UPDATE OF community_did fires whenever the column is in the SET list.
	// Re-pointing to the same community must not move anything; re-pointing
	// to a different one must move both.
	_, err = db.ExecContext(ctx, `
		UPDATE community_subscriptions SET community_did = community_did
		WHERE user_did = $1 AND community_did = $2
	`, lateSubscriberDID, withRelationships.DID)
	require.NoError(t, err)
	sameTarget, err := repo.GetByDID(ctx, withRelationships.DID)
	require.NoError(t, err)
	assert.Equal(t, 3, sameTarget.SubscriberCount,
		"an UPDATE that leaves community_did unchanged must not move the count")

	_, err = db.ExecContext(ctx, `
		UPDATE community_subscriptions SET community_did = $3
		WHERE user_did = $1 AND community_did = $2
	`, lateSubscriberDID, withRelationships.DID, withoutRelationships.DID)
	require.NoError(t, err)
	movedFrom, err := repo.GetByDID(ctx, withRelationships.DID)
	require.NoError(t, err)
	movedTo, err := repo.GetByDID(ctx, withoutRelationships.DID)
	require.NoError(t, err)
	assert.Equal(t, 2, movedFrom.SubscriberCount, "re-pointing a subscription must decrement the old community")
	assert.Equal(t, 1, movedTo.SubscriberCount, "re-pointing a subscription must increment the new community")
	_, err = db.ExecContext(ctx, `
		UPDATE community_subscriptions SET community_did = $3
		WHERE user_did = $1 AND community_did = $2
	`, lateSubscriberDID, withoutRelationships.DID, withRelationships.DID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		DELETE FROM community_subscriptions
		WHERE user_did = $1 AND community_did = $2
	`, lateSubscriberDID, withRelationships.DID)
	require.NoError(t, err)

	afterDelete, err := repo.GetByDID(ctx, withRelationships.DID)
	require.NoError(t, err)
	assert.Equal(t, 2, afterDelete.SubscriberCount,
		"future relationship deletes must reduce the materialized count")

	// The constraints are the only loud part of the migration; prove they exist.
	_, err = db.ExecContext(ctx, `UPDATE communities SET subscriber_count = -1 WHERE did = $1`, withRelationships.DID)
	require.Error(t, err, "a negative subscriber count must be rejected by the CHECK constraint")
	_, err = db.ExecContext(ctx, `UPDATE communities SET subscriber_count = NULL WHERE did = $1`, withRelationships.DID)
	require.Error(t, err, "a NULL subscriber count must be rejected by NOT NULL")
}

func subscriberTriggerExists(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'maintain_community_subscriber_count')`,
	).Scan(&exists))
	return exists
}
