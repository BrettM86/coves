package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/userblocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the error taxonomy consumed by the connector and the
// DeadLetterRedriver: PERMANENT rejections (structurally invalid records,
// security policy violations) are wrapped with ErrPermanentEvent so they are
// dead-lettered without retries and never redriven, while ORDERING-dependent
// failures ("community not found") stay TRANSIENT so the redrive can succeed
// once the missing dependency arrives.

func taxonomyEvent(did, collection, op, rkey string, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Kind:   "commit",
		Did:    did,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Operation:  op,
			Collection: collection,
			RKey:       rkey,
			CID:        "baftaxonomy",
			Record:     record,
		},
	}
}

func TestPostConsumer_RepoCommunityMismatch_IsPermanent(t *testing.T) {
	// The repo-DID/community-DID security check runs before any repository access,
	// so no DB is needed.
	c := NewPostEventConsumer(nil, nil, nil, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		"did:plc:evilrepo", "social.coves.community.post", "create", "p1",
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": "did:plc:victimcommunity",
			"author":    "did:plc:someauthor",
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent, "repo/community DID mismatch is a permanent security rejection")
}

func TestPostConsumer_MissingRequiredField_IsPermanent(t *testing.T) {
	c := NewPostEventConsumer(nil, nil, nil, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		"did:plc:somecommunity", "social.coves.community.post", "create", "p1",
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"author":    "did:plc:someauthor",
			"createdAt": "2026-01-01T00:00:00Z",
			// community field missing → structurally invalid
		},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent, "record missing required fields is permanently invalid")
}

func TestCommentConsumer_ValidationRejections_ArePermanent(t *testing.T) {
	// Validation runs before any repository/DB access.
	c := NewCommentEventConsumer(nil, nil)
	ctx := context.Background()

	validReply := map[string]interface{}{
		"root":   map[string]interface{}{"uri": "at://did:plc:x/social.coves.community.post/1", "cid": "bafroot"},
		"parent": map[string]interface{}{"uri": "at://did:plc:x/social.coves.community.post/1", "cid": "bafparent"},
	}

	t.Run("invalid commenter DID format", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("not-a-did", CommentCollection, "create", "c1",
			map[string]interface{}{
				"$type": CommentCollection, "content": "hi", "reply": validReply,
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})

	t.Run("missing content", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:commenter", CommentCollection, "create", "c1",
			map[string]interface{}{
				"$type": CommentCollection, "reply": validReply,
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})

	t.Run("invalid root reference", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:commenter", CommentCollection, "create", "c1",
			map[string]interface{}{
				"$type": CommentCollection, "content": "hi",
				"reply": map[string]interface{}{
					"root":   map[string]interface{}{"uri": "https://not-at-uri.example/1", "cid": "bafroot"},
					"parent": map[string]interface{}{"uri": "at://did:plc:x/social.coves.community.post/1", "cid": "bafparent"},
				},
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})
}

func TestVoteConsumer_ValidationRejections_ArePermanent(t *testing.T) {
	// Validation runs before any repository/DB access.
	c := NewVoteEventConsumer(nil, nil, nil)
	ctx := context.Background()

	t.Run("invalid direction", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:voter", "social.coves.feed.vote", "create", "v1",
			map[string]interface{}{
				"subject":   map[string]interface{}{"uri": "at://did:plc:x/social.coves.community.post/1", "cid": "baf1"},
				"direction": "sideways",
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})

	t.Run("missing subject", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:voter", "social.coves.feed.vote", "create", "v1",
			map[string]interface{}{
				"direction": "up",
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})
}

func TestCommunityConsumer_InvalidRKey_IsPermanent(t *testing.T) {
	// skipVerification=true and an explicit handle keep this test off the network
	// and away from the DB: the rkey check fires before any repository access.
	c := NewCommunityEventConsumer(nil, "did:web:test.local", true, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		"did:plc:community", "social.coves.community.profile", "create", "not-self",
		map[string]interface{}{
			"name":      "gaming",
			"handle":    "c-gaming.test.local",
			"hostedBy":  "did:web:test.local",
			"createdBy": "did:plc:creator",
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent, "non-'self' community profile rkey is permanently invalid")
}

func TestCommunityConsumer_SubscriptionMissingSubject_IsPermanent(t *testing.T) {
	c := NewCommunityEventConsumer(nil, "did:web:test.local", true, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		"did:plc:subscriber", "social.coves.community.subscription", "create", "s1",
		map[string]interface{}{"createdAt": "2026-01-01T00:00:00Z"},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent, "subscription record without subject is permanently invalid")
}

func TestAggregatorConsumer_ValidationRejections_ArePermanent(t *testing.T) {
	// Both checks fire before any repository access.
	c := NewAggregatorEventConsumer(nil)
	ctx := context.Background()

	t.Run("invalid service rkey", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:agg", "social.coves.aggregator.service", "create", "not-self",
			map[string]interface{}{"displayName": "Agg", "createdAt": "2026-01-01T00:00:00Z"}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})

	t.Run("authorization communityDid mismatch", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:realcommunity", "social.coves.aggregator.authorization", "create", "a1",
			map[string]interface{}{
				"aggregatorDid": "did:plc:agg",
				"communityDid":  "did:plc:othercommunity",
				"createdBy":     "did:plc:mod",
				"createdAt":     "2026-01-01T00:00:00Z",
				"enabled":       true,
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})
}

func TestUserConsumer_ValidationRejections_ArePermanent(t *testing.T) {
	consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{})
	ctx := context.Background()

	t.Run("identity event missing did", func(t *testing.T) {
		err := consumer.HandleEvent(ctx, &JetstreamEvent{
			Kind:     "identity",
			Did:      "did:plc:someuser",
			TimeUS:   time.Now().UnixMicro(),
			Identity: &IdentityEvent{Did: "", Handle: "some.handle"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})

	t.Run("identity event missing handle is a SKIP, not an error", func(t *testing.T) {
		// Jetstream emits handle-less identity events when a handle is
		// invalidated/tombstoned — valid events with nothing for us to apply.
		// Erroring here would dead-letter the entire network's handle
		// invalidations as permanent failures (junk DLQ rows at scale).
		err := consumer.HandleEvent(ctx, &JetstreamEvent{
			Kind:     "identity",
			Did:      "did:plc:someuser",
			TimeUS:   time.Now().UnixMicro(),
			Identity: &IdentityEvent{Did: "did:plc:someuser", Handle: ""},
		})
		require.NoError(t, err)
	})

	t.Run("user block with invalid blocked DID", func(t *testing.T) {
		blockConsumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
			WithUserBlockRepo(failingUserBlockRepoStub{}))
		err := blockConsumer.HandleEvent(ctx, taxonomyEvent(
			"did:plc:blocker", CovesActorBlockCollection, "create", "b1",
			map[string]interface{}{"subject": "not-a-did", "createdAt": "2026-01-01T00:00:00Z"},
		))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})
}

// failingUserBlockRepoStub satisfies userblocks.Repository for validation-path
// tests; validation rejects the event before any repository method is reached,
// so every method panics to prove it is never called.
type failingUserBlockRepoStub struct{}

func (failingUserBlockRepoStub) BlockUser(ctx context.Context, block *userblocks.UserBlock) (*userblocks.UserBlock, error) {
	panic("BlockUser must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) UnblockUser(ctx context.Context, blockerDID, blockedDID string) error {
	panic("UnblockUser must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) GetBlock(ctx context.Context, blockerDID, blockedDID string) (*userblocks.UserBlock, error) {
	panic("GetBlock must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) GetBlockByURI(ctx context.Context, recordURI string) (*userblocks.UserBlock, error) {
	panic("GetBlockByURI must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) ListBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*userblocks.UserBlock, error) {
	panic("ListBlockedUsers must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error) {
	panic("IsBlocked must not be reached for a permanently invalid event")
}

func (failingUserBlockRepoStub) AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error) {
	panic("AreBlocked must not be reached for a permanently invalid event")
}
