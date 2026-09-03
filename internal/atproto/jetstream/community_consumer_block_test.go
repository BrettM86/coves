//go:build integration

package jetstream

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a social.coves.community.block commit does to the index.
//
// This is the consumer half of tests/integration/community_blocking_test.go
// (the repository half is internal/db/postgres/community_repo_blocks_test.go).
// It runs against real Postgres because the behaviour under test is precisely
// what lands in community_blocks: the block record lives in the USER's repo and
// names the community in its subject field, so the consumer has to cross those
// over into (user_did, community_did) — a swap no mock can catch, and one that
// would make every block silently apply to the wrong party.
//
// The consumer is built without a rev gate, matching a single-feed deployment;
// multi-feed ordering is rev_gate_test.go's subject.

const communityBlockCollection = "social.coves.community.block"

// blockEvent builds the Jetstream commit a client's block/unblock produces.
// A delete commit carries no record, which is why the consumer has to look the
// block up by URI to learn which community was unblocked.
func blockEvent(userDID, rkey, operation, subject string) *JetstreamEvent {
	commit := &CommitEvent{
		Rev:        "3lblock" + rkey,
		Operation:  operation,
		Collection: communityBlockCollection,
		RKey:       rkey,
		CID:        "bafyblock" + rkey,
	}
	if subject != "" {
		commit.Record = map[string]interface{}{
			"$type":     communityBlockCollection,
			"subject":   subject,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}
	}
	return &JetstreamEvent{
		Did:    userDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: commit,
	}
}

// newCommunityBlockConsumer wires the consumer over a fresh database clone and
// returns it with the repository the assertions read through.
//
// Verification is skipped and the identity resolver is nil: the block path
// neither resolves handles nor verifies a community's DID document, so wiring
// either would only add a network dependency to a test about two columns.
func newCommunityBlockConsumer(t *testing.T) (*CommunityEventConsumer, communities.Repository) {
	t.Helper()

	repo := postgres.NewCommunityRepository(testkit.DB(t), credentialciphertest.Fixed())
	return NewCommunityEventConsumer(repo, "did:web:coves.social", true, nil), repo
}

// seedBlockableCommunity inserts the community a block will point at.
func seedBlockableCommunity(t *testing.T, repo communities.Repository, name string) *communities.Community {
	t.Helper()

	did := "did:plc:jsblk" + name
	community, err := repo.Create(context.Background(), &communities.Community{
		DID:          did,
		Handle:       "c-" + name + ".coves.social",
		Name:         name,
		DisplayName:  name,
		OwnerDID:     did,
		CreatedByDID: "did:plc:jsblkcreator",
		HostedByDID:  "did:web:coves.social",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoErrorf(t, err, "seeding community %s", name)
	return community
}

func TestCommunityConsumer_BlockCreateIndexesTheBlock(t *testing.T) {
	t.Parallel()

	consumer, repo := newCommunityBlockConsumer(t)
	ctx := context.Background()
	community := seedBlockableCommunity(t, repo, "createblock")
	const userDID = "did:plc:jsblkcreator1"

	require.NoError(t, consumer.HandleEvent(ctx, blockEvent(userDID, "cb1", "create", community.DID)))

	block, err := repo.GetBlock(ctx, userDID, community.DID)
	require.NoError(t, err, "the block was not indexed")

	// The crossover: the commit's repo DID is the BLOCKER and the record's
	// subject is the community. Storing them the other way round would block
	// the wrong party while every count and listing still looked plausible.
	assert.Equal(t, userDID, block.UserDID)
	assert.Equal(t, community.DID, block.CommunityDID)
	assert.Equal(t, "at://"+userDID+"/"+communityBlockCollection+"/cb1", block.RecordURI)

	blocked, err := repo.IsBlocked(ctx, userDID, community.DID)
	require.NoError(t, err)
	assert.True(t, blocked, "IsBlocked must agree with the row the consumer just wrote")
}

func TestCommunityConsumer_BlockDeleteRemovesTheBlock(t *testing.T) {
	t.Parallel()

	consumer, repo := newCommunityBlockConsumer(t)
	ctx := context.Background()
	community := seedBlockableCommunity(t, repo, "deleteblock")
	const userDID = "did:plc:jsblkdeleter"

	require.NoError(t, consumer.HandleEvent(ctx, blockEvent(userDID, "cb2", "create", community.DID)))

	// The delete commit carries no subject, so the consumer must find the
	// community through the record URI it reconstructs from the rkey.
	require.NoError(t, consumer.HandleEvent(ctx, blockEvent(userDID, "cb2", "delete", "")))

	_, err := repo.GetBlock(ctx, userDID, community.DID)
	assert.Truef(t, communities.IsNotFound(err), "expected the block to be gone, got %v", err)

	blocked, err := repo.IsBlocked(ctx, userDID, community.DID)
	require.NoError(t, err)
	assert.False(t, blocked)
}

func TestCommunityConsumer_BlockCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	// Duplicates are guaranteed: the connector rewinds its cursor on every
	// reconnect and the dead-letter redriver re-invokes HandleEvent. A second
	// copy of the same block must not produce a second row.
	consumer, repo := newCommunityBlockConsumer(t)
	ctx := context.Background()
	community := seedBlockableCommunity(t, repo, "idemblock")
	const userDID = "did:plc:jsblkidem"

	event := blockEvent(userDID, "cb3", "create", community.DID)
	require.NoError(t, consumer.HandleEvent(ctx, event))
	require.NoError(t, consumer.HandleEvent(ctx, event))

	blocks, err := repo.ListBlockedCommunities(ctx, userDID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, blocks, 1)
}

func TestCommunityConsumer_BlockDeleteOfAnUnknownRecordIsANoOp(t *testing.T) {
	t.Parallel()

	// An unblock whose create this AppView never saw — backfill gaps, a feed
	// joined mid-stream — must not fail the event. Erroring would dead-letter
	// and then redrive it forever, since the record will never exist.
	consumer, _ := newCommunityBlockConsumer(t)

	require.NoError(t, consumer.HandleEvent(context.Background(),
		blockEvent("did:plc:jsblkghost", "cb-never-created", "delete", "")))
}
