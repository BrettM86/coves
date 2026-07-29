//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/userblocks"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a social.coves.actor.block commit does to the index.
//
// Moved from tests/integration/userblock_indexing_test.go. It runs against real
// Postgres because what is under test is the row: the block record lives in the
// BLOCKER's repo and names the blocked user in its subject, so the consumer
// crosses the commit's repo DID and the record's subject into
// (blocker_did, blocked_did) and reconstructs the AT-URI the delete path later
// looks the row up by. A mock repository would accept any of those wrong.
//
// The consumer's DB-free block behaviour lives with its neighbours rather than
// here, so it does not need Postgres to run: the permanent-rejection paths are
// in error_taxonomy_test.go, and the nil-repo and update-operation cases are in
// user_consumer_test.go.

// userBlockEvent builds the Jetstream commit a block or unblock produces.
// Delete commits carry no record, which is why subject is optional.
func userBlockEvent(blockerDID, rkey, operation, subject string) *JetstreamEvent {
	commit := &CommitEvent{
		Rev:        "3lublock" + rkey,
		Operation:  operation,
		Collection: CovesActorBlockCollection,
		RKey:       rkey,
		CID:        "bafyublock" + rkey,
	}
	if subject != "" {
		commit.Record = map[string]interface{}{
			"$type":     CovesActorBlockCollection,
			"subject":   subject,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}
	}
	return &JetstreamEvent{
		Did:    blockerDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: commit,
	}
}

// newUserBlockConsumer wires the users consumer over a fresh database clone with
// only the block repository attached.
//
// The user service and identity resolver are nil because the block path uses
// neither; a block event is routed on its collection before any profile or
// identity handling is reached.
func newUserBlockConsumer(t *testing.T) (*UserEventConsumer, userblocks.Repository) {
	t.Helper()

	repo := postgres.NewUserBlockRepository(testkit.DB(t))
	return NewUserEventConsumer(nil, nil, WithUserBlockRepo(repo)), repo
}

func TestUserConsumer_BlockCreateIndexesTheBlock(t *testing.T) {
	t.Parallel()

	consumer, repo := newUserBlockConsumer(t)
	ctx := context.Background()
	const blockerDID = "did:plc:ublkcreator"
	const blockedDID = "did:plc:ublkcreated"

	require.NoError(t, consumer.HandleEvent(ctx, userBlockEvent(blockerDID, "ub1", "create", blockedDID)))

	block, err := repo.GetBlock(ctx, blockerDID, blockedDID)
	require.NoError(t, err, "the block was not indexed")

	assert.Equal(t, blockerDID, block.BlockerDID)
	assert.Equal(t, blockedDID, block.BlockedDID)
	// The URI is rebuilt from the commit rather than carried on it, and it is
	// the only handle the delete path has on this row.
	assert.Equal(t, "at://"+blockerDID+"/"+CovesActorBlockCollection+"/ub1", block.RecordURI)
	assert.Equal(t, "bafyublockub1", block.RecordCID)

	blocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
	require.NoError(t, err)
	assert.True(t, blocked, "IsBlocked must agree with the row the consumer just wrote")
}

func TestUserConsumer_BlockDeleteRemovesTheBlock(t *testing.T) {
	t.Parallel()

	consumer, repo := newUserBlockConsumer(t)
	ctx := context.Background()
	const blockerDID = "did:plc:ublkdeleter"
	const blockedDID = "did:plc:ublkdeleted"

	require.NoError(t, consumer.HandleEvent(ctx, userBlockEvent(blockerDID, "ub2", "create", blockedDID)))

	// A delete commit carries no subject, so the consumer resolves the blocked
	// DID through the URI it rebuilds from the rkey.
	require.NoError(t, consumer.HandleEvent(ctx, userBlockEvent(blockerDID, "ub2", "delete", "")))

	blocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
	require.NoError(t, err)
	assert.False(t, blocked)

	_, err = repo.GetBlock(ctx, blockerDID, blockedDID)
	assert.Truef(t, userblocks.IsNotFound(err), "expected the block to be gone, got %v", err)
}

func TestUserConsumer_BlockCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	// Duplicates are guaranteed in production: the connector rewinds its cursor
	// on every reconnect and the dead-letter redriver re-invokes HandleEvent.
	consumer, repo := newUserBlockConsumer(t)
	ctx := context.Background()
	const blockerDID = "did:plc:ublkidem"

	event := userBlockEvent(blockerDID, "ub3", "create", "did:plc:ublkidemtarget")
	require.NoError(t, consumer.HandleEvent(ctx, event))
	require.NoError(t, consumer.HandleEvent(ctx, event))

	blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, blocks, 1)
}

func TestUserConsumer_BlockDeleteOfAnUnknownRecordIsANoOp(t *testing.T) {
	t.Parallel()

	// An unblock whose create this AppView never saw must not fail: erroring
	// would dead-letter an event that can never succeed on redrive, because the
	// row it refers to will never exist.
	consumer, _ := newUserBlockConsumer(t)

	require.NoError(t, consumer.HandleEvent(context.Background(),
		userBlockEvent("did:plc:ublkghost", "ub-never-created", "delete", "")))
}
