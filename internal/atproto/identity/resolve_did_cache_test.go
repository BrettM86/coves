package identity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachingResolver_ResolveDIDCachesVerifiedIdentity(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()

	first, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	assert.Contains(t, cache.entries, fixtureDID, "the first DID lookup must populate the identity cache")

	second, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	assert.Equal(t, 1, base.totalCalls(), "the second DID lookup must reuse the cached identity")
	for _, doc := range []*DIDDocument{first, second} {
		assert.Equal(t, fixtureDID, doc.DID)
		assert.Equal(t, []Service{{
			ID:              "#atproto_pds",
			Type:            "AtprotoPersonalDataServer",
			ServiceEndpoint: fixturePDS,
		}}, doc.Service)
	}
}

func TestCachingResolver_ResolveDIDDoesNotPersistUnverifiedIdentity(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()
	base.setResolveResult(unverifiedIdentity(), nil)

	doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	require.Len(t, doc.Service, 1)
	assert.Equal(t, "#atproto_pds", doc.Service[0].ID)
	assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	assert.Empty(t, cache.sets, "an unverified handle must not enter the durable identity cache")
}

func TestCachingResolver_ResolveDIDCacheIsSharedWithResolve(t *testing.T) {
	t.Parallel()
	resolver, base, _ := newCachingFixture()

	_, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	identity, err := resolver.Resolve(context.Background(), fixtureDID)
	require.NoError(t, err)
	assert.Equal(t, fixtureHandle, identity.Handle)
	assert.Equal(t, MethodCache, identity.Method)
	assert.Equal(t, 1, base.totalCalls(), "Resolve must reuse the identity populated by ResolveDID")
}

func TestCachingResolver_ResolveDIDReusesUnverifiedNegativeEntry(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()
	base.setResolveResult(unverifiedIdentity(), nil)

	identity, err := resolver.Resolve(context.Background(), fixtureDID)
	require.NoError(t, err)
	require.Equal(t, InvalidHandle, identity.Handle)
	doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	require.Len(t, doc.Service, 1)
	assert.Equal(t, "#atproto_pds", doc.Service[0].ID)
	assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	assert.Equal(t, 1, base.totalCalls(), "a recent unverified resolution must not contact the base again")
	assert.Empty(t, cache.sets, "an unverified handle must not enter the durable identity cache")
}

func TestCachingResolver_ResolveDIDPurgesStaleUnverifiedRow(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()
	stale := unverifiedIdentity()
	stale.PDSURL = "https://stale.pds.example"
	cache.entries[fixtureDID] = stale

	doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	require.Len(t, doc.Service, 1)
	assert.Equal(t, "#atproto_pds", doc.Service[0].ID)
	assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	assert.Equal(t, 1, base.totalCalls(), "a stale unverified row must be re-resolved")
	assert.Contains(t, cache.purges, fixtureDID, "the stale row must be purged")
}

func TestCachingResolver_ResolveDIDNegativeCachesFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		baseError           error
		expiredFirstContext bool
		checkError          func(*testing.T, error)
	}{
		{
			name:      "resolution failed",
			baseError: &ErrResolutionFailed{Identifier: fixtureDID, Reason: "directory unavailable"},
			checkError: func(t *testing.T, err error) {
				var failed *ErrResolutionFailed
				require.ErrorAs(t, err, &failed)
			},
		},
		{
			name:      "not found",
			baseError: &ErrNotFound{Identifier: fixtureDID, Reason: "no directory entry"},
			checkError: func(t *testing.T, err error) {
				var notFound *ErrNotFound
				require.ErrorAs(t, err, &notFound)
			},
		},
		{
			name:                "expired caller deadline",
			baseError:           &ErrResolutionFailed{Identifier: fixtureDID, Reason: "directory deadline", Err: context.DeadlineExceeded},
			expiredFirstContext: true,
			checkError: func(t *testing.T, err error) {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, base, cache := newCachingFixture()
			base.setResolveResult(nil, tc.baseError)
			clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
			resolver := newCachingResolverWithClock(base, cache, time.Minute, func() time.Time {
				return clock
			})
			firstContext := context.Background()
			if tc.expiredFirstContext {
				var cancel context.CancelFunc
				firstContext, cancel = context.WithDeadline(firstContext, time.Time{})
				defer cancel()
				require.ErrorIs(t, firstContext.Err(), context.DeadlineExceeded)
			}

			_, firstErr := resolver.ResolveDID(firstContext, fixtureDID)
			tc.checkError(t, firstErr)
			_, secondErr := resolver.ResolveDID(context.Background(), fixtureDID)
			tc.checkError(t, secondErr)
			assert.Equal(t, 1, base.totalCalls(), "a second DID lookup before the negative TTL must not contact the base")
			assert.Empty(t, cache.sets, "a failed resolution must not enter the durable identity cache")
		})
	}
}

func TestCachingResolver_ResolveDIDPurgeClearsNegativeFailure(t *testing.T) {
	t.Parallel()
	resolver, base, _ := newCachingFixture()
	base.setResolveResult(nil, &ErrResolutionFailed{Identifier: fixtureDID, Reason: "directory unavailable"})

	for range 2 {
		_, err := resolver.ResolveDID(context.Background(), fixtureDID)
		var failed *ErrResolutionFailed
		require.ErrorAs(t, err, &failed)
	}
	assert.Equal(t, 1, base.totalCalls(), "the second lookup must use the negative cache")

	require.NoError(t, resolver.Purge(context.Background(), fixtureDID))
	_, err := resolver.ResolveDID(context.Background(), fixtureDID)
	var failed *ErrResolutionFailed
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, 2, base.totalCalls(), "purging must permit another base lookup")
}

func TestCachingResolver_ResolveDIDDoesNotCacheCancelledFailure(t *testing.T) {
	t.Parallel()
	resolver, base, _ := newCachingFixture()
	base.setResolveResult(nil, &ErrResolutionFailed{
		Identifier: fixtureDID, Reason: "caller cancelled", Err: context.Canceled,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolver.ResolveDID(ctx, fixtureDID)
	require.ErrorIs(t, err, context.Canceled)
	base.setResolveResult(freshIdentity(), nil)

	doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
	require.NoError(t, err)
	require.Len(t, doc.Service, 1)
	assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	assert.Equal(t, 2, base.totalCalls(), "caller cancellation must not hide a recovered identity")
}

func TestCachingResolver_ResolveDIDRejectsNonDIDsBeforeBase(t *testing.T) {
	t.Parallel()
	resolver, base, _ := newCachingFixture()
	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{name: "empty", identifier: ""},
		{name: "handle", identifier: fixtureHandle},
		{name: "method only", identifier: "did:"},
		{name: "missing did prefix", identifier: "plc:abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolver.ResolveDID(context.Background(), tc.identifier)
			var invalid *ErrInvalidIdentifier
			assert.ErrorAs(t, err, &invalid)
			assert.Zero(t, base.totalCalls(), "invalid input must not reach the base resolver")
		})
	}
}
