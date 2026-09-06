package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
)

type twoPhaseOriginResolver struct {
	calls       int
	capturedCtx context.Context
}

func (r *twoPhaseOriginResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	r.calls++
	if r.calls == 1 {
		return nil, &identity.ErrResolutionFailed{Identifier: did, Reason: "directory unavailable"}
	}
	r.capturedCtx = ctx
	return &identity.Identity{DID: did, Handle: bridgedHandle, PDSURL: trustedBridgePDS}, nil
}

func requireIdentityResolveDeadline(t *testing.T, captured context.Context, callStart time.Time) {
	t.Helper()
	require.NotNil(t, captured, "the consumer must resolve the unknown DID before classifying the event")
	deadline, ok := captured.Deadline()
	require.True(t, ok,
		"identity resolution needs its own deadline: the HTTP client's 10-second timeout is the only bound today, so a tarpit did:web costs four 10-second waits of serial lane time per fabricated event")
	assert.False(t, deadline.Before(callStart),
		"the resolution deadline must be derived from this call rather than an already-expired context")
	assert.False(t, deadline.After(callStart.Add(5*time.Second+time.Second)),
		"the resolution deadline must stay within the literal five-second product bound plus scheduling slack; raising the production constant must not silently restore attacker-controlled lane starvation")
}

func TestUserConsumer_UnknownProfileResolutionHasDeadline(t *testing.T) {
	t.Parallel()

	const did = "did:plc:deadlinedunknownuser"
	resolver := &mockIdentityResolverForUser{resolveErr: errors.New("identity directory unavailable")}
	consumer := NewUserEventConsumer(newMockUserService(), resolver)
	event := &JetstreamEvent{
		Did:    did,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        "3ldeadlineduser",
			Operation:  "create",
			Collection: CovesProfileCollection,
			RKey:       "self",
			CID:        "bafydeadlineduser",
			Record:     map[string]interface{}{"displayName": "Deadline"},
		},
	}

	callStart := time.Now()
	err := consumer.HandleEvent(context.Background(), event)
	require.ErrorIs(t, err, ErrUnresolvedReference,
		"a failed unknown-DID resolution must remain bounded and redrivable")
	require.Equal(t, 1, resolver.calls, "one event must trigger exactly one bounded identity resolution")
	requireIdentityResolveDeadline(t, resolver.capturedCtx, callStart)
}

func TestCreateCommunity_ResolutionHasDeadline(t *testing.T) {
	t.Parallel()

	resolver := &countingResolver{handle: "c-deadline.example.net"}
	consumer := NewCommunityEventConsumer(newOriginRepo(), originTestInstance, true, resolver)
	event := &JetstreamEvent{
		Did:    "did:plc:deadlinedcommunity",
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: profileCommit("create", "deadline", "", nil),
	}

	callStart := time.Now()
	require.NoError(t, consumer.HandleEvent(context.Background(), event),
		"a valid resolved community must reach the create path so its resolver context is observable")
	require.Equal(t, 1, resolver.calls, "one profile create must trigger exactly one bounded identity resolution")
	requireIdentityResolveDeadline(t, resolver.capturedCtx, callStart)
}

func TestUpdateCommunity_OriginFallbackResolutionHasDeadline(t *testing.T) {
	t.Parallel()

	const did = "did:plc:deadlineoriginfallback"
	repo := newOriginRepo()
	seedCommunity(repo, did, bridgedHandle, trustedBridgePDS)
	resolver := &twoPhaseOriginResolver{}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver,
		WithCommunityBridgeTrust(trustingBridge()))
	event := &JetstreamEvent{
		Did:    did,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: profileCommit("update", "comicstrips", bridgedOrigin, nil),
	}

	callStart := time.Now()
	require.NoError(t, consumer.HandleEvent(context.Background(), event),
		"the update must tolerate its first resolution failure and reach the origin fallback")
	require.Equal(t, 2, resolver.calls,
		"an asserted origin must trigger the fallback after the update path's first resolution established nothing")
	requireIdentityResolveDeadline(t, resolver.capturedCtx, callStart)
}
