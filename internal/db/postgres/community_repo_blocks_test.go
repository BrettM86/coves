//go:build integration

package postgres

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The community-block half of community_repo_blocks.go against real SQL.
//
// This is the repository half of tests/integration/community_blocking_test.go;
// the Jetstream half of that file — what a social.coves.community.block commit
// does to these rows — is now
// internal/atproto/jetstream/community_consumer_block_test.go.
//
// The community rows are seeded through the repository even though
// community_blocks has no foreign key to communities: a block row that points
// at a DID nothing hosts is a state the AppView never reaches, and seeding the
// real row is what makes a future join (a listing that returns community
// profiles rather than DIDs) fail here instead of in production.

// blockableCommunity seeds one community and returns it.
func blockableCommunity(t *testing.T, repo communities.Repository, name string) *communities.Community {
	t.Helper()

	did := "did:plc:blk" + name
	created, err := repo.Create(context.Background(), &communities.Community{
		DID:          did,
		Handle:       "c-" + name + ".coves.social",
		Name:         name,
		DisplayName:  name,
		Description:  "a community that gets blocked",
		OwnerDID:     did,
		CreatedByDID: "did:plc:blockcreator",
		HostedByDID:  "did:web:coves.social",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoErrorf(t, err, "seeding community %s", name)
	return created
}

// blockCommunity indexes one block of community by user, as the consumer would.
func blockCommunity(t *testing.T, repo communities.Repository, userDID string, community *communities.Community, rkey string, blockedAt time.Time) *communities.CommunityBlock {
	t.Helper()

	block, err := repo.BlockCommunity(context.Background(), &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: community.DID,
		BlockedAt:    blockedAt,
		RecordURI:    "at://" + userDID + "/social.coves.community.block/" + rkey,
		RecordCID:    "bafy" + rkey,
	})
	require.NoErrorf(t, err, "blocking community %s", community.Name)
	return block
}

func TestCommunityRepo_ListBlockedCommunities(t *testing.T) {
	t.Parallel()

	repo := NewCommunityRepository(testkit.DB(t))
	ctx := context.Background()
	userDID := "did:plc:blocklister"

	// Deterministic, strictly increasing timestamps: the listing is ORDER BY
	// blocked_at DESC, and rows written in one loop with time.Now() can share a
	// timestamp and make the order arbitrary.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"listalpha", "listbravo", "listcharlie"} {
		community := blockableCommunity(t, repo, name)
		blockCommunity(t, repo, userDID, community, name, base.Add(time.Duration(i)*time.Hour))
	}

	t.Run("lists every community the user has blocked", func(t *testing.T) {
		blocks, err := repo.ListBlockedCommunities(ctx, userDID, 10, 0)
		require.NoError(t, err)
		require.Len(t, blocks, 3)
		for _, block := range blocks {
			assert.Equal(t, userDID, block.UserDID)
		}
	})

	t.Run("pagination follows blocked_at DESC", func(t *testing.T) {
		first, err := repo.ListBlockedCommunities(ctx, userDID, 2, 0)
		require.NoError(t, err)
		require.Len(t, first, 2)

		second, err := repo.ListBlockedCommunities(ctx, userDID, 2, 2)
		require.NoError(t, err)
		require.Len(t, second, 1)

		// The assertion is on the ORDER, not the count: an offset applied to an
		// unsorted query also returns 2 then 1.
		assert.Equal(t, "did:plc:blklistcharlie", first[0].CommunityDID)
		assert.Equal(t, "did:plc:blklistbravo", first[1].CommunityDID)
		assert.Equal(t, "did:plc:blklistalpha", second[0].CommunityDID)
	})

	t.Run("a user with no blocks gets an empty list", func(t *testing.T) {
		blocks, err := repo.ListBlockedCommunities(ctx, "did:plc:blocksnobody", 10, 0)
		require.NoError(t, err)
		assert.Empty(t, blocks)
	})
}

func TestCommunityRepo_IsBlocked(t *testing.T) {
	t.Parallel()

	repo := NewCommunityRepository(testkit.DB(t))
	ctx := context.Background()

	t.Run("false when no block exists", func(t *testing.T) {
		community := blockableCommunity(t, repo, "isblockednone")
		blocked, err := repo.IsBlocked(ctx, "did:plc:isblockeduser1", community.DID)
		require.NoError(t, err)
		assert.False(t, blocked)
	})

	t.Run("true once the block is indexed", func(t *testing.T) {
		community := blockableCommunity(t, repo, "isblockedyes")
		userDID := "did:plc:isblockeduser2"
		blockCommunity(t, repo, userDID, community, "isblockedyes", time.Now())

		blocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		require.NoError(t, err)
		assert.True(t, blocked)
	})

	// Self-contained on purpose: the version this replaced unblocked whatever
	// the PREVIOUS subtest had blocked, so reordering or skipping that subtest
	// turned this one into an assertion that nothing was ever blocked.
	t.Run("false again after the block is removed", func(t *testing.T) {
		community := blockableCommunity(t, repo, "isblockedgone")
		userDID := "did:plc:isblockeduser3"
		blockCommunity(t, repo, userDID, community, "isblockedgone", time.Now())

		require.NoError(t, repo.UnblockCommunity(ctx, userDID, community.DID))

		blocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		require.NoError(t, err)
		assert.False(t, blocked)
	})
}

func TestCommunityRepo_GetBlock(t *testing.T) {
	t.Parallel()

	repo := NewCommunityRepository(testkit.DB(t))
	ctx := context.Background()
	userDID := "did:plc:getblockuser"
	community := blockableCommunity(t, repo, "getblock")

	t.Run("ErrBlockNotFound when nothing is indexed", func(t *testing.T) {
		_, err := repo.GetBlock(ctx, userDID, community.DID)
		assert.Truef(t, communities.IsNotFound(err), "expected ErrBlockNotFound, got %v", err)
	})

	stored := blockCommunity(t, repo, userDID, community, "getblock", time.Now())

	t.Run("by user and community DID", func(t *testing.T) {
		block, err := repo.GetBlock(ctx, userDID, community.DID)
		require.NoError(t, err)
		assert.Equal(t, userDID, block.UserDID)
		assert.Equal(t, community.DID, block.CommunityDID)
		assert.Equal(t, stored.RecordURI, block.RecordURI)
	})

	// The consumer's DELETE path has only the record URI to work with — Jetstream
	// delete commits carry no record — so this lookup is what turns an unblock
	// into a row it can remove.
	t.Run("by record URI", func(t *testing.T) {
		block, err := repo.GetBlockByURI(ctx, stored.RecordURI)
		require.NoError(t, err)
		assert.Equal(t, stored.RecordURI, block.RecordURI)
		assert.Equal(t, community.DID, block.CommunityDID)
		assert.Equal(t, userDID, block.UserDID)
	})
}
