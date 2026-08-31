package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the error taxonomy consumed by the connector and the
// DeadLetterRedriver: PERMANENT rejections (structurally invalid records,
// security policy violations) are wrapped with ErrPermanentEvent so they are
// dead-lettered without retries and never redriven, while ORDERING-dependent
// failures ("community not found") are UNRESOLVED: off the serial lane but
// redrivable once the missing dependency arrives.

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

	// Every structural rejection on the block path. The blocker DID and the
	// subject both arrive from an untrusted repo, and a block row keyed on
	// something that is not a DID would be a row no lookup can ever match — so
	// these are rejected before the repository, which is what the panicking stub
	// below proves.
	t.Run("user block validation rejections", func(t *testing.T) {
		blockConsumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
			WithUserBlockRepo(failingUserBlockRepoStub{}))

		validRecord := map[string]interface{}{
			"subject": "did:plc:blocked", "createdAt": "2026-01-01T00:00:00Z",
		}
		unsupportedDID := "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"

		for _, tc := range []struct {
			name       string
			blockerDID string
			operation  string
			rkey       string
			record     map[string]interface{}
		}{
			{name: "create with no record data", blockerDID: "did:plc:blocker",
				operation: "create", rkey: "b1", record: nil},
			{name: "record without a subject", blockerDID: "did:plc:blocker",
				operation: "create", rkey: "b1",
				record: map[string]interface{}{"createdAt": "2026-01-01T00:00:00Z"}},
			{name: "invalid blocked DID", blockerDID: "did:plc:blocker",
				operation: "create", rkey: "b1",
				record: map[string]interface{}{"subject": "not-a-did", "createdAt": "2026-01-01T00:00:00Z"}},
			{name: "unsupported blocked DID method", blockerDID: "did:plc:blocker",
				operation: "create", rkey: "b1",
				record: map[string]interface{}{"subject": unsupportedDID, "createdAt": "2026-01-01T00:00:00Z"}},
			{name: "invalid blocker DID", blockerDID: "not-a-did",
				operation: "create", rkey: "b1", record: validRecord},
			{name: "unsupported blocker DID method", blockerDID: unsupportedDID,
				operation: "create", rkey: "b1", record: validRecord},
			// Without an rkey the consumer cannot build the record's AT-URI, and
			// the URI is the only key the unblock path has to find the row by.
			{name: "create with no rkey", blockerDID: "did:plc:blocker",
				operation: "create", rkey: "", record: validRecord},
			{name: "delete with no rkey", blockerDID: "did:plc:blocker",
				operation: "delete", rkey: "", record: nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := blockConsumer.HandleEvent(ctx, taxonomyEvent(
					tc.blockerDID, CovesActorBlockCollection, tc.operation, tc.rkey, tc.record))
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrPermanentEvent)
			})
		}
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

// The gates below exist because of docs/CONSUMER_TRUST_AUDIT.md §1.3: each of
// these values used to fall through to a lookup or an INSERT whose failure was
// classified transient, so a record carrying a non-DID where a DID belongs
// bought 4.2s of in-line retries plus ten redrives against a value no replay
// can repair. They must be refused BEFORE any repository access — which is
// also why every consumer here is built without one — and refused permanently.

func TestCommunityConsumer_NonDIDReferences_ArePermanent(t *testing.T) {
	c := NewCommunityEventConsumer(nil, "did:web:coves.social", true, nil)
	ctx := context.Background()

	t.Run("subscription subject", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:subscriber", "social.coves.community.subscription", "create", "s1",
			map[string]interface{}{"subject": "example.com", "createdAt": "2026-01-01T00:00:00Z"}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"a subscription subject that is not a DID can never resolve to a community; the FK-not-found "+
				"branch (unresolved, redriven) must never see it")
		assert.NotErrorIs(t, err, ErrUnresolvedReference)
	})

	t.Run("block subject", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:blocker", "social.coves.community.block", "create", "b1",
			map[string]interface{}{"subject": "example.com", "createdAt": "2026-01-01T00:00:00Z"}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"community_blocks.community_did carries a DID-shape CHECK (migration 009); a non-DID subject "+
				"used to reach the INSERT, fail the CHECK, and be retried as if the constraint might change")
	})

	t.Run("block subject with unsupported DID method", func(t *testing.T) {
		err := c.HandleEvent(ctx, taxonomyEvent("did:plc:blocker", "social.coves.community.block", "create", "b1",
			map[string]interface{}{
				"subject":   "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK",
				"createdAt": "2026-01-01T00:00:00Z",
			}))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"syntax.ParseDID accepts did:key, but migration 009 accepts only did:plc and did:web")
		assert.NotErrorIs(t, err, ErrUnresolvedReference)
	})
}

func TestAggregatorConsumer_NonDIDAggregator_IsPermanent(t *testing.T) {
	c := NewAggregatorEventConsumer(nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent("did:plc:realcommunity", "social.coves.aggregator.authorization", "create", "a1",
		map[string]interface{}{
			"aggregatorDid": "rss-bot",
			"communityDid":  "did:plc:realcommunity",
			"createdBy":     "did:plc:mod",
			"createdAt":     "2026-01-01T00:00:00Z",
			"enabled":       true,
		}))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"an aggregatorDid that is not a DID can never satisfy fk_aggregator; it must be retired on the "+
			"payload, not redriven ten times")
}

// The handle on an identity event is asserted by the relay, so its value is
// chosen upstream of this AppView. Both of its failure modes used to surface as
// plain transient errors: 4.2s of lane per event, forever, for a handle that
// either can never be accepted or is held by someone else.
func TestUserConsumer_IdentityHandleFailures_AreClassified(t *testing.T) {
	ctx := context.Background()
	identityEvent := func() *JetstreamEvent {
		return &JetstreamEvent{
			Kind:   "identity",
			Did:    "did:plc:knownuser",
			TimeUS: time.Now().UnixMicro(),
			Identity: &IdentityEvent{
				Did:    "did:plc:knownuser",
				Handle: "renamed.test",
			},
		}
	}

	t.Run("a handle this AppView rejects is permanent", func(t *testing.T) {
		svc := newMockUserService()
		svc.users["did:plc:knownuser"] = &users.User{DID: "did:plc:knownuser", Handle: "old.test"}
		svc.updateHandleError = &users.InvalidHandleError{Handle: "renamed.test", Reason: "reserved namespace"}
		consumer := NewUserEventConsumer(svc, &mockIdentityResolverForUser{})

		err := consumer.HandleEvent(ctx, identityEvent())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"the replay carries the same handle and fails the same rule; retrying it is pure lane time")
	})

	t.Run("a handle held by another row is an unresolved reference", func(t *testing.T) {
		svc := newMockUserService()
		svc.users["did:plc:knownuser"] = &users.User{DID: "did:plc:knownuser", Handle: "old.test"}
		svc.updateHandleError = users.ErrHandleAlreadyTaken
		consumer := NewUserEventConsumer(svc, &mockIdentityResolverForUser{})

		err := consumer.HandleEvent(ctx, identityEvent())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnresolvedReference,
			"the incumbent may release the handle (its own identity event is in flight), so the redrive "+
				"stays — but the lane must not wait 4.2s for it per event")
		assert.NotErrorIs(t, err, ErrPermanentEvent)
	})
}
