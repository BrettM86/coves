//go:build integration

package communities_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
)

// How an #identity event for an indexed community becomes its stored handle.
//
// The consumer resolves the DID fresh through the uncached resolver, under the
// per-DID refresh lock, and stores the verified handle; the event's own handle
// is never trusted. Every case runs on newCommunityOriginHarness with the
// refresh lock and the real postgres-backed caching resolver, so purges are
// observable as identity_cache rows. Uncached resolution is scripted.

const (
	renamedCommunityOldHandle = "gaming.lemmy-world.tdpl.io"
	renamedCommunityNewHandle = "gaming.lemmy.world.ap.tdpl.io"
	relocatedCommunityPDS     = "https://pds.relocated.test"
)

// identityEvent is an #identity event for the harness DID carrying handle; an
// empty handle leaves the event handle-less.
func (h *communityOriginHarness) identityEvent(handle string) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Kind:     "identity",
		Did:      h.did,
		TimeUS:   time.Now().UnixMicro(),
		Identity: &jetstream.IdentityEvent{Did: h.did, Handle: handle},
	}
}

// didKeyedCacheRows counts identity_cache rows keyed by the harness DID.
func (h *communityOriginHarness) didKeyedCacheRows(t *testing.T) int {
	t.Helper()
	return h.cacheRowsKeyedBy(t, h.did)
}

// cacheRowsKeyedBy counts identity_cache rows whose identifier is identifier.
func (h *communityOriginHarness) cacheRowsKeyedBy(t *testing.T, identifier string) int {
	t.Helper()
	var count int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM identity_cache WHERE identifier = $1`, identifier).Scan(&count); err != nil {
		t.Fatalf("count identity_cache rows for %s: %v", identifier, err)
	}
	return count
}

// assertStoredHandle checks the handle column of the harness community.
func (h *communityOriginHarness) assertStoredHandle(t *testing.T, want string) {
	t.Helper()
	if got := h.read(t).Handle; got != want {
		t.Errorf("stored handle = %q, want %q", got, want)
	}
}

// assertNoResolverCalls checks neither resolver was asked anything.
func (h *communityOriginHarness) assertNoResolverCalls(t *testing.T) {
	t.Helper()
	if h.realCaching.resolves != 0 || h.realCaching.purges != 0 {
		t.Errorf("caching resolver Resolve, Purge calls = %d, %d, want 0, 0",
			h.realCaching.resolves, h.realCaching.purges)
	}
	if h.uncached.resolves != 0 || h.uncached.documentReads != 0 || h.uncached.purges != 0 {
		t.Errorf("uncached resolver Resolve, ResolveDeclaredIdentity, Purge calls = %d, %d, %d, want 0, 0, 0",
			h.uncached.resolves, h.uncached.documentReads, h.uncached.purges)
	}
}

// failingGetByDIDRepository is the real repository except that GetByDID fails.
type failingGetByDIDRepository struct {
	communities.Repository
	err error
}

func (r failingGetByDIDRepository) GetByDID(context.Context, string) (*communities.Community, error) {
	return nil, r.err
}

var errCommunityReadUnavailable = errors.New("community read unavailable")

// TestCommunityIdentityEvent_StoresTheFreshVerifiedHandle is the acceptance
// case: the caching resolver still answers the old handle, the uncached one
// answers the new handle, and only the handle column changes.
func TestCommunityIdentityEvent_StoresTheFreshVerifiedHandle(t *testing.T) {
	t.Parallel()
	h := newCommunityOriginHarness(t, "did:plc:communityidentityb1",
		renamedCommunityNewHandle, relocatedCommunityPDS, withRealCachingResolver(), withIdentityRefreshLock())
	h.seed(t, seededCommunity{
		name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS,
		origin: gamingOriginHost, originURL: gamingOriginURL, originSoftware: gamingSoftwareKept,
	})
	h.seedCachedIdentity(t, renamedCommunityOldHandle, trustedBridgePDS)

	if err := h.deliverEvent(h.identityEvent(renamedCommunityNewHandle)); err != nil {
		t.Fatalf("identity event: %v", err)
	}

	stored := h.read(t)
	if stored.Handle != renamedCommunityNewHandle {
		t.Errorf("stored handle = %q, want %q", stored.Handle, renamedCommunityNewHandle)
	}
	if stored.Name != "gaming" || stored.Origin != "lemmy.world" || stored.PDSURL != "https://bridge.test" {
		t.Errorf("stored name, origin, pds_url = %q, %q, %q, want %q, %q, %q unchanged",
			stored.Name, stored.Origin, stored.PDSURL, "gaming", "lemmy.world", "https://bridge.test")
	}
	byHandle, err := h.repo.GetByHandle(context.Background(), renamedCommunityNewHandle)
	if err != nil {
		t.Fatalf("GetByHandle(%s): %v", renamedCommunityNewHandle, err)
	}
	if byHandle.DID != "did:plc:communityidentityb1" {
		t.Errorf("GetByHandle(%s) DID = %q, want %q", renamedCommunityNewHandle, byHandle.DID, "did:plc:communityidentityb1")
	}
}

// TestCommunityIdentityEvent_UnchangedHandleStillPurgesTheCachedDID: the
// stored handle already equals the fresh verified one, but the DID's cached
// answer carries an older handle; the refresh drops it and leaves the row.
func TestCommunityIdentityEvent_UnchangedHandleStillPurgesTheCachedDID(t *testing.T) {
	t.Parallel()
	h := newCommunityOriginHarness(t, "did:plc:communityidentityb2",
		renamedCommunityNewHandle, trustedBridgePDS, withRealCachingResolver(), withIdentityRefreshLock())
	h.seed(t, seededCommunity{name: "gaming", handle: renamedCommunityNewHandle, pdsURL: trustedBridgePDS})
	h.seedCachedIdentity(t, renamedCommunityOldHandle, trustedBridgePDS)
	if got := h.didKeyedCacheRows(t); got != 1 {
		t.Fatalf("identity_cache rows keyed by the DID before delivery = %d, want 1", got)
	}

	if err := h.deliverEvent(h.identityEvent(renamedCommunityOldHandle)); err != nil {
		t.Fatalf("identity event: %v", err)
	}

	h.assertStoredHandle(t, renamedCommunityNewHandle)
	if got := h.didKeyedCacheRows(t); got != 0 {
		t.Errorf("identity_cache rows keyed by the DID after the refresh = %d, want 0", got)
	}
}

// TestCommunityIdentityEvent_UnindexedOrUnreadableDIDResolvesNothing: an
// identity event for a DID this instance does not index is acknowledged
// without any resolver call; a failed community read is an error, not an
// acknowledgement, and also resolves nothing.
func TestCommunityIdentityEvent_UnindexedOrUnreadableDIDResolvesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		did         string
		eventHandle string
		readFails   bool
	}{
		{name: "not indexed, event carries a handle", did: "did:plc:communityidentityb3a", eventHandle: renamedCommunityNewHandle},
		{name: "not indexed, handle-less event", did: "did:plc:communityidentityb3b"},
		{name: "community read fails", did: "did:plc:communityidentityb3c", eventHandle: renamedCommunityNewHandle, readFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := []communityOriginHarnessOption{withRealCachingResolver(), withIdentityRefreshLock()}
			if tc.readFails {
				opts = append(opts, withConsumerRepository(func(repo communities.Repository) communities.Repository {
					return failingGetByDIDRepository{Repository: repo, err: errCommunityReadUnavailable}
				}))
			}
			h := newCommunityOriginHarness(t, tc.did, renamedCommunityNewHandle, trustedBridgePDS, opts...)
			if tc.readFails {
				h.seed(t, seededCommunity{name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS})
			}

			err := h.deliverEvent(h.identityEvent(tc.eventHandle))

			if tc.readFails {
				if err == nil {
					t.Errorf("identity event with a failing community read returned nil, want an error")
				} else if errors.Is(err, jetstream.ErrPermanentEvent) {
					t.Errorf("identity event with a failing community read = %v, want an error that is not ErrPermanentEvent", err)
				}
				h.assertStoredHandle(t, renamedCommunityOldHandle)
			} else {
				if err != nil {
					t.Errorf("identity event for an unindexed DID = %v, want nil", err)
				}
				h.assertNoCommunity(t)
			}
			h.assertNoResolverCalls(t)
		})
	}
}

// TestCommunityIdentityEvent_ResolvesWhateverHandleTheEventCarries: neither a
// handle-less event nor one repeating the stored handle short-circuits the
// fresh resolution.
func TestCommunityIdentityEvent_ResolvesWhateverHandleTheEventCarries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		did         string
		eventHandle string
	}{
		{name: "handle-less event", did: "did:plc:communityidentityb4a"},
		{name: "event carries the stored old handle", did: "did:plc:communityidentityb4b", eventHandle: renamedCommunityOldHandle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCommunityOriginHarness(t, tc.did, renamedCommunityNewHandle, trustedBridgePDS,
				withRealCachingResolver(), withIdentityRefreshLock())
			h.seed(t, seededCommunity{name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS})
			h.seedCachedIdentity(t, renamedCommunityOldHandle, trustedBridgePDS)

			if err := h.deliverEvent(h.identityEvent(tc.eventHandle)); err != nil {
				t.Fatalf("identity event: %v", err)
			}

			h.assertStoredHandle(t, renamedCommunityNewHandle)
		})
	}
}

// TestCommunityIdentityEvent_RejectedResolutionKeepsTheStoredHandle: a fresh
// resolution that fails, does not verify, or verifies a handle the community
// may not hold leaves the row alone; the error class says whether a redrive
// can help.
func TestCommunityIdentityEvent_RejectedResolutionKeepsTheStoredHandle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		did  string
		// verifiedHandle is what the uncached resolver answers unless script
		// overrides it.
		verifiedHandle string
		script         func(r *scriptedCommunityResolver, did string)
		// verifyAs turns hostedBy verification on with this consumer instance
		// DID; hostedByDID is the stored hosted_by_did.
		verifyAs    string
		hostedByDID string
		wantErr     error
	}{
		{
			name: "uncached resolution fails", did: "did:plc:communityidentityb5a",
			verifiedHandle: renamedCommunityNewHandle,
			script: func(r *scriptedCommunityResolver, _ string) {
				r.answer, r.resolveErr = nil, errDirectoryUnavailable
			},
			wantErr: jetstream.ErrUnresolvedReference,
		},
		{
			name: "resolution answers handle.invalid", did: "did:plc:communityidentityb5b",
			verifiedHandle: renamedCommunityNewHandle,
			script: func(r *scriptedCommunityResolver, did string) {
				r.answer = identityOn(did, identity.InvalidHandle, trustedBridgePDS)
			},
			wantErr: jetstream.ErrUnresolvedReference,
		},
		{
			name: "resolution answers another DID", did: "did:plc:communityidentityb5c",
			verifiedHandle: renamedCommunityNewHandle,
			script: func(r *scriptedCommunityResolver, _ string) {
				r.answer = identityOn("did:plc:communityidentityb5other", renamedCommunityNewHandle, trustedBridgePDS)
			},
			wantErr: jetstream.ErrPermanentEvent,
		},
		{
			// The stored hosted_by_did (tdpl.io) does not match the handle's
			// domain (coves.test); the consumer's own instance DID does, so a
			// check against the instance DID would go on to fetch
			// did:web:coves.test and fail with a non-permanent error instead.
			name: "verified handle outside the stored hostedBy domain", did: "did:plc:communityidentityb5d",
			verifiedHandle: "gaming.lemmy.world.ap.coves.test",
			verifyAs:       "did:web:coves.test", hostedByDID: "did:web:tdpl.io",
			wantErr: jetstream.ErrPermanentEvent,
		},
		{
			name: "verified handle breaks community handle rules", did: "did:plc:communityidentityb5e",
			verifiedHandle: "under_score.example.com",
			wantErr:        jetstream.ErrPermanentEvent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := []communityOriginHarnessOption{withRealCachingResolver(), withIdentityRefreshLock()}
			if tc.verifyAs != "" {
				opts = append(opts, withHostedByVerification(tc.verifyAs))
			}
			h := newCommunityOriginHarness(t, tc.did, tc.verifiedHandle, trustedBridgePDS, opts...)
			h.seed(t, seededCommunity{
				name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS, hostedByDID: tc.hostedByDID,
			})
			if tc.script != nil {
				tc.script(h.uncached, tc.did)
			}

			err := h.deliverEvent(h.identityEvent(tc.verifiedHandle))

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("identity event = %v, want errors.Is %v", err, tc.wantErr)
			}
			if tc.wantErr == jetstream.ErrUnresolvedReference && errors.Is(err, jetstream.ErrPermanentEvent) {
				t.Errorf("identity event = %v, want it not ErrPermanentEvent", err)
			}
			h.assertStoredHandle(t, renamedCommunityOldHandle)
		})
	}
}

// TestCommunityIdentityEvent_HandleHeldByAnotherCommunity: the verified
// handle is stored for a different community; the rename waits for that row
// to move instead of failing for good, and neither row changes.
func TestCommunityIdentityEvent_HandleHeldByAnotherCommunity(t *testing.T) {
	t.Parallel()
	const holderDID = "did:plc:communityidentityb6holder"
	h := newCommunityOriginHarness(t, "did:plc:communityidentityb6",
		renamedCommunityNewHandle, trustedBridgePDS, withRealCachingResolver(), withIdentityRefreshLock())
	h.seed(t, seededCommunity{name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS})
	h.seedDID(t, holderDID, seededCommunity{name: "gamingholder", handle: renamedCommunityNewHandle, pdsURL: trustedBridgePDS})

	err := h.deliverEvent(h.identityEvent(renamedCommunityNewHandle))

	if !errors.Is(err, jetstream.ErrUnresolvedReference) || errors.Is(err, jetstream.ErrPermanentEvent) {
		t.Errorf("identity event = %v, want errors.Is ErrUnresolvedReference and not ErrPermanentEvent", err)
	}
	h.assertStoredHandle(t, renamedCommunityOldHandle)
	holder, readErr := h.repo.GetByDID(context.Background(), holderDID)
	if readErr != nil {
		t.Fatalf("read holder community %s: %v", holderDID, readErr)
	}
	if holder.Handle != renamedCommunityNewHandle {
		t.Errorf("holder community handle = %q, want %q", holder.Handle, renamedCommunityNewHandle)
	}
}

// failOncePurgeResolver forwards everything to the real caching resolver
// except the first Purge of failHandle, which fails.
type failOncePurgeResolver struct {
	identity.Resolver
	failHandle string
	failed     bool
}

var errCachePurgeUnavailable = errors.New("identity cache purge unavailable")

func (r *failOncePurgeResolver) Purge(ctx context.Context, identifier string) error {
	if identifier == r.failHandle && !r.failed {
		r.failed = true
		return errCachePurgeUnavailable
	}
	return r.Resolver.Purge(ctx, identifier)
}

// TestCommunityIdentityEvent_FailedOldHandlePurgeKeepsTheOldHandle: the old
// handle's cache entry must be gone before the new handle is stored. A failed
// purge leaves the row (and the entry) for a redrive, and the replay finishes
// both.
func TestCommunityIdentityEvent_FailedOldHandlePurgeKeepsTheOldHandle(t *testing.T) {
	t.Parallel()
	const did = "did:plc:communityidentityb7"
	h := newCommunityOriginHarness(t, did, renamedCommunityNewHandle, trustedBridgePDS, withIdentityRefreshLock(),
		withCachingResolver(func(caching identity.Resolver) identity.Resolver {
			return &failOncePurgeResolver{Resolver: caching, failHandle: renamedCommunityOldHandle}
		}))
	h.seed(t, seededCommunity{name: "gaming", handle: renamedCommunityOldHandle, pdsURL: trustedBridgePDS})
	// An orphan keyed by the old handle only: purging the DID cannot reach it.
	if _, err := h.db.ExecContext(context.Background(), `INSERT INTO identity_cache
		(identifier, did, handle, pds_url, resolved_at, resolution_method, expires_at)
		VALUES ($1, $2, $1, $3, NOW(), 'dns', NOW() + INTERVAL '1 hour')`,
		renamedCommunityOldHandle, did, trustedBridgePDS); err != nil {
		t.Fatalf("seed identity_cache row keyed by %s: %v", renamedCommunityOldHandle, err)
	}
	event := h.identityEvent(renamedCommunityNewHandle)

	err := h.deliverEvent(event)

	if err == nil {
		t.Errorf("identity event with a failing old-handle purge returned nil, want an error")
	} else if errors.Is(err, jetstream.ErrPermanentEvent) {
		t.Errorf("identity event with a failing old-handle purge = %v, want an error that is not ErrPermanentEvent", err)
	}
	h.assertStoredHandle(t, renamedCommunityOldHandle)
	if got := h.cacheRowsKeyedBy(t, renamedCommunityOldHandle); got != 1 {
		t.Errorf("identity_cache rows keyed by %s after the failed purge = %d, want 1", renamedCommunityOldHandle, got)
	}

	if err := h.deliverEvent(event); err != nil {
		t.Errorf("replayed identity event = %v, want nil", err)
	}
	h.assertStoredHandle(t, renamedCommunityNewHandle)
	if got := h.cacheRowsKeyedBy(t, renamedCommunityOldHandle); got != 0 {
		t.Errorf("identity_cache rows keyed by %s after the replay = %d, want 0", renamedCommunityOldHandle, got)
	}
}
