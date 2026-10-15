package jetstream

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
)

// TestCommunityConsumer_ProfileOriginTimeoutIsDebouncedAndDeferred drives a
// community profile carrying originUrl and originSoftware through
// identity.NewUncachedResolver against local servers whose handle domain hangs.
// Any repo can put those fields in its profile, so each such event forces a
// fresh resolution; without the per-DID debounce, every event and every redrive
// would cost the community lane the whole identityResolveTimeout.
func TestCommunityConsumer_ProfileOriginTimeoutIsDebouncedAndDeferred(t *testing.T) {
	t.Parallel()

	const communityDID = "did:plc:communityoriginhang"
	const handleSuffix = ".communityoriginhang.test"
	const handle = "c-hang" + handleSuffix
	// The handle sits under the bridge PDS host's registrable domain, so the
	// identity passes the document checks and reaches handle verification.
	const bridgePDS = "https://pds.communityoriginhang.test"
	deliveries := []struct {
		name      string
		known     bool
		operation string
	}{
		{name: "known bridged community", known: true, operation: "update"},
		{name: "unknown community create", known: false, operation: "create"},
	}
	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()

			handleDomain := newHangableServer(t, true, nil)
			plc := newHangableServer(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+communityDID {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
					`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
					communityDID, handle, bridgePDS)
			})
			config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
				handleSuffix: strings.TrimPrefix(handleDomain.server.URL, "http://"),
			}))
			config.PLCURL = plc.server.URL

			repo := newOriginRepo()
			seeded := communities.Community{
				DID: communityDID, Handle: handle, Name: "hangstored", DisplayName: "Stored Name",
				PDSURL: bridgePDS, Origin: "lemmy.ml",
				OriginURL: "https://lemmy.ml/c/hangstored", OriginSoftware: "lemmy",
			}
			if delivery.known {
				stored := seeded
				repo.byDID[communityDID] = &stored
			}
			consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil,
				WithCommunityBridgeTrust(NewBridgeTrust([]string{bridgePDS})),
				WithCommunityUncachedIdentityResolver(identity.NewUncachedResolver(config)),
				WithCommunityIdentityRefreshFailureTTL(time.Hour))
			var clockMu sync.Mutex
			current := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			consumer.identityRefreshFailures.now = func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return current
			}
			advance := func(elapsed time.Duration) {
				clockMu.Lock()
				defer clockMu.Unlock()
				current = current.Add(elapsed)
			}
			event := func() *JetstreamEvent {
				return &JetstreamEvent{
					Kind: "commit",
					Did:  communityDID,
					Commit: profileCommit(delivery.operation, "hangevent", "lemmy.world", map[string]interface{}{
						"displayName":    "Event Name",
						"originUrl":      "https://lemmy.world/c/hangevent",
						"originSoftware": "piefed",
					}),
				}
			}

			started := time.Now()
			err := consumer.HandleEvent(context.Background(), event())
			require.ErrorIs(t, err, ErrUnresolvedReference, "a timed-out fresh resolution on a trusted host must be redrivable")
			require.NotErrorIs(t, err, errRedriveDeferred,
				"the event that actually ran the resolution did real work and must spend its attempt")
			require.GreaterOrEqual(t, time.Since(started), identityResolveTimeout,
				"the first event must take the timeout path, not a fast failure")
			require.Equal(t, int64(2), plc.requests.Load(),
				"the document is read once for admission and once more by the verifying lookup")
			require.Equal(t, int64(1), handleDomain.requests.Load())

			advance(time.Second)
			err = consumer.HandleEvent(context.Background(), event())
			require.ErrorIs(t, err, ErrUnresolvedReference, "a debounced profile must still be redrivable rather than dropped")
			assert.ErrorIs(t, err, errRedriveDeferred,
				"the timeout must be recorded so the next event within the TTL defers without resolving")
			assert.Equal(t, int64(2), plc.requests.Load(), "a debounced profile must not reach the PLC directory")
			assert.Equal(t, int64(1), handleDomain.requests.Load(), "a debounced profile must not reach the handle domain")
			if delivery.known {
				require.Contains(t, repo.byDID, communityDID)
				assert.Equal(t, seeded, *repo.byDID[communityDID], "no profile write may happen without a verified resolution")
			} else {
				assert.NotContains(t, repo.byDID, communityDID, "an unverified identity must not be indexed")
			}
		})
	}
}

// TestCommunityConsumer_ProfileOriginOffBridgeUpdateSkipsHandleLookup is the
// hang setup above with the DID document naming a PDS outside the bridge list.
// Such a document can never admit the display fields, so the update is applied
// against the stored handle without verifying the handle: the hanging handle
// domain is never asked, and the community lane does not wait on it.
func TestCommunityConsumer_ProfileOriginOffBridgeUpdateSkipsHandleLookup(t *testing.T) {
	t.Parallel()

	const communityDID = "did:plc:communityoriginoffbridge"
	const handleSuffix = ".communityoriginoffbridge.test"
	const handle = "c-offbridge" + handleSuffix
	const bridgePDS = "https://pds.communityoriginoffbridge.test"
	const offBridgePDS = "https://pds.elsewhere.test"

	handleDomain := newHangableServer(t, true, nil)
	plc := newHangableServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+communityDID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
			`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
			communityDID, handle, offBridgePDS)
	})
	config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
		handleSuffix: strings.TrimPrefix(handleDomain.server.URL, "http://"),
	}))
	config.PLCURL = plc.server.URL

	repo := newOriginRepo()
	repo.byDID[communityDID] = &communities.Community{
		DID: communityDID, Handle: handle, Name: "offbridgestored", DisplayName: "Stored Name",
		PDSURL: bridgePDS, Origin: "lemmy.ml",
		OriginURL: "https://lemmy.ml/c/offbridgestored", OriginSoftware: "lemmy",
	}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil,
		WithCommunityBridgeTrust(NewBridgeTrust([]string{bridgePDS})),
		WithCommunityUncachedIdentityResolver(identity.NewUncachedResolver(config)),
		WithCommunityIdentityRefreshFailureTTL(time.Hour))

	started := time.Now()
	err := consumer.HandleEvent(context.Background(), &JetstreamEvent{
		Kind: "commit",
		Did:  communityDID,
		Commit: profileCommit("update", "offbridgeevent", "lemmy.world", map[string]interface{}{
			"displayName":    "Event Name",
			"originUrl":      "https://lemmy.world/c/offbridgeevent",
			"originSoftware": "piefed",
		}),
	})
	require.NoError(t, err, "an off-bridge document settles the display fields; the rest of the update applies")
	assert.Less(t, time.Since(started), identityResolveTimeout, "the update must not wait on the handle domain")
	assert.Equal(t, int64(1), plc.requests.Load(), "only the admission read of the document is made")
	assert.Equal(t, int64(0), handleDomain.requests.Load(), "an off-bridge document must not reach the handle domain")

	require.Contains(t, repo.byDID, communityDID)
	stored := repo.byDID[communityDID]
	assert.Equal(t, "Event Name", stored.DisplayName, "the rest of the record is applied")
	assert.Empty(t, stored.OriginURL, "a display field from an off-bridge repo is cleared")
	assert.Empty(t, stored.OriginSoftware, "a display field from an off-bridge repo is cleared")
	assert.Equal(t, handle, stored.Handle, "the stored handle is kept")
	assert.Equal(t, bridgePDS, stored.PDSURL, "an unverified document must not write pds_url")
}

// declaringCommunityResolver answers Resolve from the embedded mock and the DID
// document from declared, so a case can make the two disagree.
type declaringCommunityResolver struct {
	mockIdentityResolverForUser
	declared *identity.DeclaredIdentity
}

func (r *declaringCommunityResolver) ResolveDeclaredIdentity(context.Context, string) (*identity.DeclaredIdentity, error) {
	return r.declared, nil
}

// TestResolveFreshCommunityIdentity_ReportsDisplayVerdict pins the reason code
// a fresh resolution reports for the origin display fields, and that a
// document which can never admit them stops before any handle lookup.
func TestResolveFreshCommunityIdentity_ReportsDisplayVerdict(t *testing.T) {
	t.Parallel()

	const did = "did:plc:communityoriginverdict"
	const bridgePDS = "https://bridge.test"
	const bridgeHandle = "gaming.lemmy.world.ap.bridge.test"
	const untrustedPDS = "https://pds.example.com"
	for _, tc := range []struct {
		name         string
		declared     identity.DeclaredIdentity
		answer       identity.Identity
		want         originDisplayVerdict
		wantResolves int
	}{
		{
			name:     "document on an untrusted PDS",
			declared: identity.DeclaredIdentity{DID: did, DeclaredHandle: "gaming.example.com", PDSURL: untrustedPDS},
			answer:   identity.Identity{DID: did, Handle: "gaming.example.com", PDSURL: untrustedPDS},
			want:     originDisplayUntrustedDocument, wantResolves: 0,
		},
		{
			name:     "document declares a handle outside the bridge domain",
			declared: identity.DeclaredIdentity{DID: did, DeclaredHandle: "gaming.other.test", PDSURL: bridgePDS},
			answer:   identity.Identity{DID: did, Handle: "gaming.other.test", PDSURL: bridgePDS},
			want:     originDisplayUntrustedDocument, wantResolves: 0,
		},
		{
			name:     "verified answer on an untrusted PDS",
			declared: identity.DeclaredIdentity{DID: did, DeclaredHandle: bridgeHandle, PDSURL: bridgePDS},
			answer:   identity.Identity{DID: did, Handle: bridgeHandle, PDSURL: untrustedPDS},
			want:     originDisplayUntrustedResolution, wantResolves: 1,
		},
		{
			name:     "verified handle outside the bridge domain",
			declared: identity.DeclaredIdentity{DID: did, DeclaredHandle: bridgeHandle, PDSURL: bridgePDS},
			answer:   identity.Identity{DID: did, Handle: "gaming.other.test", PDSURL: bridgePDS},
			want:     originDisplayHandleOutsideBridgeDomain, wantResolves: 1,
		},
		{
			name:     "verified on the bridge",
			declared: identity.DeclaredIdentity{DID: did, DeclaredHandle: bridgeHandle, PDSURL: bridgePDS},
			answer:   identity.Identity{DID: did, Handle: bridgeHandle, PDSURL: bridgePDS},
			want:     originDisplayAdmitted, wantResolves: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			declared, answer := tc.declared, tc.answer
			resolver := &declaringCommunityResolver{
				mockIdentityResolverForUser: mockIdentityResolverForUser{identities: map[string]*identity.Identity{did: &answer}},
				declared:                    &declared,
			}
			consumer := NewCommunityEventConsumer(newOriginRepo(), originTestInstance, true, nil,
				WithCommunityBridgeTrust(NewBridgeTrust([]string{bridgePDS})),
				WithCommunityUncachedIdentityResolver(resolver))

			fresh, err := consumer.resolveFreshCommunityIdentity(context.Background(), did)
			require.NoError(t, err)
			assert.Equal(t, tc.want, fresh.verdict, "verdict %s, want %s", fresh.verdict, tc.want)
			assert.Equal(t, tc.wantResolves, resolver.calls, "Resolve calls")
			if tc.wantResolves == 0 {
				assert.Nil(t, fresh.resolved, "no handle was verified")
				assert.Empty(t, fresh.verifiedHandle)
			}
		})
	}
	assert.Equal(t, "untrusted-document", originDisplayUntrustedDocument.String())
	assert.Equal(t, "handle-outside-bridge-domain", originDisplayHandleOutsideBridgeDomain.String())
}
