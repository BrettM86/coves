//go:build integration

package postgres

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
)

// TestUserRepo_Delete_ConcurrentDeletionsSharingCommunitiesDoNotDeadlock pins
// the lock discipline around the subscriber-count trigger. Deleting an account
// removes every subscription it holds, and the trigger updates one communities
// row per removal inside the same transaction. Two deletions whose accounts
// share several communities would acquire those rows in heap order and could
// deadlock; Delete pre-locks the affected communities in DID order so they
// serialize instead. A deadlock surfaces here as SQLSTATE 40P01 from one of
// the goroutines, which the assertions below refuse.
func TestUserRepo_Delete_ConcurrentDeletionsSharingCommunitiesDoNotDeadlock(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	communityRepo := NewCommunityRepository(db, credentialciphertest.Fixed())
	userRepo := NewUserRepository(db)

	const communityCount = 6
	const accountCount = 8
	communityDIDs := make([]string, 0, communityCount)
	for i := 0; i < communityCount; i++ {
		id := testkit.UniqueID(t)
		community, err := communityRepo.Create(ctx, &communities.Community{
			DID:          "did:plc:shared" + id,
			Handle:       "c-shared-" + id + ".coves.local",
			Name:         "shared-" + id,
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:sharedcreator",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		})
		require.NoError(t, err)
		communityDIDs = append(communityDIDs, community.DID)
	}

	accountDIDs := make([]string, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		handle := testkit.UniqueIDWithPrefix(t, "deadlock")
		did := fixtures.DID(handle)
		fixtures.User(t, db, handle+".test", did)
		accountDIDs = append(accountDIDs, did)
		// Every account subscribes to every community, half of them in reverse
		// order so the heap order of their subscription rows differs.
		for j := range communityDIDs {
			target := communityDIDs[j]
			if i%2 == 1 {
				target = communityDIDs[len(communityDIDs)-1-j]
			}
			_, err := communityRepo.SubscribeWithCount(ctx, &communities.Subscription{
				UserDID: did, CommunityDID: target, ContentVisibility: 3, SubscribedAt: time.Now(),
			})
			require.NoError(t, err)
		}
	}

	errs := make([]error, accountCount)
	var start, done sync.WaitGroup
	start.Add(1)
	for i, did := range accountDIDs {
		done.Add(1)
		go func(i int, did string) {
			defer done.Done()
			start.Wait()
			errs[i] = userRepo.Delete(ctx, did)
		}(i, did)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "deleting account %s failed; a 40P01 here means the community row locks were taken out of order", accountDIDs[i])
	}

	for _, communityDID := range communityDIDs {
		community, err := communityRepo.GetByDID(ctx, communityDID)
		require.NoError(t, err)
		assert.Zerof(t, community.SubscriberCount, "community %s must count zero subscribers once every account is gone", communityDID)
	}
}
