package jetstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountActivityRepo is an originRepo that records the community and the
// create options passed to Create and, when createErr is set, refuses it with
// that error, as the Postgres repository refuses a DID with account activity.
type accountActivityRepo struct {
	*originRepo
	createErr     error
	created       []communities.Community
	createOptions [][]communities.CreateOption
}

func (r *accountActivityRepo) Create(ctx context.Context, c *communities.Community, options ...communities.CreateOption) (*communities.Community, error) {
	r.created = append(r.created, *c)
	r.createOptions = append(r.createOptions, options)
	if r.createErr != nil {
		return nil, r.createErr
	}
	return r.originRepo.Create(ctx, c)
}

// A community profile from a DID the repository refuses for account activity
// (a users row, or an authored post, comment or vote) is a permanent refusal:
// replaying the event cannot change the DID's history.
func TestCommunityProfile_AccountActivityRefusalIsPermanent(t *testing.T) {
	t.Parallel()
	const did = "did:plc:selfindexeduser"
	repo := &accountActivityRepo{
		originRepo: newOriginRepo(),
		createErr:  fmt.Errorf("create community %s: %w", did, communities.ErrDIDHasAccountActivity),
	}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil)

	err := consumer.HandleEvent(context.Background(), &JetstreamEvent{
		Kind: "commit", Did: did, TimeUS: time.Now().UnixMicro(),
		Commit: profileCommit("create", "evasion", "", nil),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent)
	assert.NotContains(t, repo.byDID, did)
}

// Only a trusted bridge's community may be indexed despite account activity
// (a bridged group posts as itself before its profile arrives): the verified
// resolved PDS is a trusted bridge host AND the verified handle is under that
// PDS's registrable domain. The DID controller writes the PDS endpoint, so a
// DID document naming the bridge PDS alone never exempts, and neither do
// record fields claiming bridge provenance.
func TestCreateCommunity_AccountActivityRuleFollowsVerifiedBridgePDS(t *testing.T) {
	t.Parallel()
	allowed := []communities.CreateOption{communities.AllowAccountActivityForTrustedBridge}
	for _, test := range []struct {
		name           string
		resolvedPDS    string
		resolvedHandle string
		bridgeTrust    *BridgeTrust
		origin         string
		extra          map[string]interface{}
		want           []communities.CreateOption
	}{
		{
			name: "trusted bridge PDS and verified handle under the bridge domain", resolvedPDS: trustedBridgePDS,
			resolvedHandle: bridgedHandle, bridgeTrust: trustingBridge(), origin: bridgedOrigin, want: allowed,
		},
		{
			name: "DID document names the bridge PDS but the verified handle is outside the bridge domain", resolvedPDS: trustedBridgePDS,
			resolvedHandle: "comicstrips.lemmy-world.example.org", bridgeTrust: trustingBridge(), origin: bridgedOrigin, want: nil,
		},
		{
			name: "untrusted PDS with a bridge-domain handle and self-asserted bridge origin and hostedBy", resolvedPDS: untrustedPDS,
			resolvedHandle: bridgedHandle, bridgeTrust: trustingBridge(), origin: bridgedOrigin,
			extra: map[string]interface{}{"hostedBy": "did:web:tdpl.io"}, want: nil,
		},
		{
			name: "untrusted PDS and verified handle under that PDS's own registrable domain", resolvedPDS: "https://pds.example.net",
			resolvedHandle: "group.example.net", bridgeTrust: trustingBridge(), want: nil,
		},
		{
			name: "no bridge trust configured", resolvedPDS: trustedBridgePDS,
			resolvedHandle: bridgedHandle, bridgeTrust: nil, origin: bridgedOrigin, want: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := &accountActivityRepo{originRepo: newOriginRepo()}
			resolver := fixedResolver{id: identity.Identity{Handle: test.resolvedHandle, PDSURL: test.resolvedPDS}}
			var options []CommunityConsumerOption
			if test.bridgeTrust != nil {
				options = append(options, WithCommunityBridgeTrust(test.bridgeTrust))
			}
			consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver, options...)

			const did = "did:plc:bridgedgroup"
			require.NoError(t, consumer.createCommunity(context.Background(), did, profileCommit("create", "comicstrips", test.origin, test.extra)))

			require.Len(t, repo.created, 1, "the community reaches the repository once")
			assert.Equal(t, test.resolvedHandle, repo.created[0].Handle, "fixture: the community is created under the verified handle")
			assert.Equal(t, test.want, repo.createOptions[0])
		})
	}
}
