package identity

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachingResolver_NegativeCachesRetryableDIDErrors(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativeerrors"
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "not found", err: &ErrNotFound{Identifier: did, Reason: "no directory entry"}},
		{name: "resolution failed", err: &ErrResolutionFailed{Identifier: did, Reason: "directory unavailable"}},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := &countingResolver{err: tc.err}
			cache := newRecordingCache()
			clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
			resolver := newCachingResolverWithClock(base, cache, DefaultNegativeCacheTTL, func() time.Time {
				return clock
			})

			_, firstErr := resolver.Resolve(context.Background(), did)
			require.ErrorIs(t, firstErr, tc.err, "the first failure must preserve the base resolver's taxonomy")
			_, secondErr := resolver.Resolve(context.Background(), did)
			require.ErrorIs(t, secondErr, tc.err, "the negative cache must preserve the original error chain")
			assert.Equal(t, 1, base.resolveCount(),
				"a retryable failure for the same DID must be resolved once per negative TTL, not once per attacker-created event")
			assert.Empty(t, cache.sets, "negative results must never enter the 24h Postgres identity cache")
		})
	}
}

func TestCachingResolver_DoesNotNegativeCacheExcludedFailures(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativeexclusions"
	for _, tc := range []struct {
		name       string
		identifier string
		err        error
		why        string
	}{
		{name: "canceled request", identifier: did, err: context.Canceled,
			why: "caller cancellation is not a fact later callers should inherit"},
		{name: "invalid identifier", identifier: did, err: &ErrInvalidIdentifier{Identifier: did, Reason: "malformed"},
			why: "invalid input is rejected before a costly network lookup and needs no process-local entry"},
		{name: "handle not found", identifier: "alice.example.com", err: &ErrNotFound{Identifier: "alice.example.com", Reason: "DNS not propagated"},
			why: "a user's newly propagated DNS handle must not remain invisible for 90 seconds at login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := &countingResolver{err: tc.err}
			resolver := newCachingResolverWithClock(base, newRecordingCache(), DefaultNegativeCacheTTL, func() time.Time {
				return time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
			})

			_, firstErr := resolver.Resolve(context.Background(), tc.identifier)
			require.ErrorIs(t, firstErr, tc.err)
			_, secondErr := resolver.Resolve(context.Background(), tc.identifier)
			require.ErrorIs(t, secondErr, tc.err)
			assert.Equal(t, 2, base.resolveCount(), "%s", tc.why)
		})
	}
}

func TestCachingResolver_CallerCancellationIsNotNegativeCachedWhenCauseIsStringified(t *testing.T) {
	t.Parallel()

	const did = "did:plc:stringifiedcancellation"
	baseErr := &ErrResolutionFailed{Identifier: did, Reason: "resolution failed: context canceled"}
	base := &countingResolver{err: baseErr}
	clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	resolver := newCachingResolverWithClock(base, newRecordingCache(), DefaultNegativeCacheTTL, func() time.Time {
		return clock
	})

	canceledParent, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolver.Resolve(canceledParent, did)
	require.ErrorIs(t, err, baseErr)

	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, 2, base.resolveCount(),
		"a caller-canceled parent must not poison later callers even when the base resolver stringifies context cancellation instead of wrapping it")
}

func TestCachingResolver_ChildDeadlineFailureIsNegativeCachedWhileParentIsLive(t *testing.T) {
	t.Parallel()

	const did = "did:plc:stringifiedchilddeadline"
	baseErr := &ErrResolutionFailed{Identifier: did, Reason: "resolution failed: context canceled"}
	base := &countingResolver{err: baseErr}
	clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	resolver := newCachingResolverWithClock(base, newRecordingCache(), DefaultNegativeCacheTTL, func() time.Time {
		return clock
	})
	liveParent, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := resolver.Resolve(liveParent, did)
	require.ErrorIs(t, err, baseErr)
	_, err = resolver.Resolve(liveParent, did)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, 1, base.resolveCount(),
		"a resolver's expired child deadline is a reusable failure while the caller's parent remains live; repeated attacker-chosen DIDs must not redial immediately")
}

func TestCachingResolver_NegativeEntryExpiresIntoVerifiedResolution(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativeexpires"
	baseErr := &ErrNotFound{Identifier: did, Reason: "DID not propagated"}
	base := &countingResolver{err: baseErr}
	cache := newRecordingCache()
	clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	resolver := newCachingResolverWithClock(base, cache, DefaultNegativeCacheTTL, func() time.Time {
		return clock
	})

	_, err := resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	require.Equal(t, 1, base.resolveCount(),
		"the negative entry must suppress repeated network work until its short TTL expires")

	clock = clock.Add(DefaultNegativeCacheTTL + time.Nanosecond)
	verified := freshIdentity()
	verified.DID = did
	base.setResolveResult(verified, nil)
	got, err := resolver.Resolve(context.Background(), did)
	require.NoError(t, err)
	assert.Equal(t, did, got.DID)
	assert.Equal(t, MethodHTTPS, got.Method, "an answer after negative expiry must be visibly fresh")
	assert.Equal(t, 2, base.resolveCount(), "expiry must permit exactly one fresh base resolution")
	assert.Len(t, cache.sets, 1,
		"once the DID verifies, the positive identity belongs in the durable 24h cache")
}

func TestCachingResolver_PurgeClearsNegativeEntry(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativepurge"
	baseErr := &ErrResolutionFailed{Identifier: did, Reason: "directory unavailable"}
	base := &countingResolver{err: baseErr}
	resolver := newCachingResolverWithClock(base, newRecordingCache(), DefaultNegativeCacheTTL, func() time.Time {
		return time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	})

	_, err := resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	require.Equal(t, 1, base.resolveCount(), "the fixture must hold a negative entry before purge")

	require.NoError(t, resolver.Purge(context.Background(), did))
	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, 2, base.resolveCount(),
		"purging a DID must clear its process-local failure so the next request can observe recovery immediately")
}

func TestCachingResolver_PurgeFailureStillClearsNegativeEntry(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativepurgefailure"
	baseErr := &ErrResolutionFailed{Identifier: did, Reason: "directory unavailable"}
	base := &countingResolver{err: baseErr}
	cache := newRecordingCache()
	resolver := newCachingResolverWithClock(base, cache, DefaultNegativeCacheTTL, func() time.Time {
		return time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	})

	_, err := resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	require.Equal(t, 1, base.resolveCount(), "the fixture must hold a negative entry before purge")

	purgeErr := fmt.Errorf("postgres identity cache unavailable")
	cache.purgeErr = purgeErr
	require.ErrorIs(t, resolver.Purge(context.Background(), did), purgeErr,
		"the durable cache failure must still be reported to the caller")

	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, 2, base.resolveCount(),
		"a failed Postgres purge must still clear process-local failures; otherwise recovery stays hidden behind an entry the caller explicitly purged")
}

func TestCachingResolver_NegativeTTLUsesDefault(t *testing.T) {
	t.Parallel()

	const did = "did:plc:negativettldefault"
	baseErr := &ErrNotFound{Identifier: did, Reason: "not propagated"}
	base := &countingResolver{err: baseErr}
	clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	resolver := newCachingResolverWithClock(base, newRecordingCache(), -time.Second, func() time.Time {
		return clock
	})

	_, err := resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	_, err = resolver.Resolve(context.Background(), did)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, 1, base.resolveCount(),
		"a negative TTL must fall back to DefaultNegativeCacheTTL rather than expiring every attacker-controlled failure immediately")
}

func TestCachingResolver_NegativeCacheEvictsOldestEntryAtBound(t *testing.T) {
	t.Parallel()

	baseErr := context.DeadlineExceeded
	base := &countingResolver{err: baseErr}
	clock := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	resolver := newCachingResolverWithClock(base, newRecordingCache(), time.Minute, func() time.Time {
		return clock
	})

	firstDID := "did:plc:negative0000"
	mostRecentDID := ""
	for i := 0; i < negativeCacheMaxEntries+1; i++ {
		mostRecentDID = fmt.Sprintf("did:plc:negative%04d", i)
		_, err := resolver.Resolve(context.Background(), mostRecentDID)
		require.ErrorIs(t, err, baseErr)
	}
	require.Equal(t, negativeCacheMaxEntries+1, base.resolveCount())

	_, err := resolver.Resolve(context.Background(), firstDID)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, negativeCacheMaxEntries+2, base.resolveCount(),
		"the first DID must be evicted FIFO once attacker-controlled failures reach the hard bound")

	_, err = resolver.Resolve(context.Background(), mostRecentDID)
	require.ErrorIs(t, err, baseErr)
	assert.Equal(t, negativeCacheMaxEntries+2, base.resolveCount(),
		"the newest DID must remain negatively cached; exceeding the bound must not disable caching entirely")
}

func TestCachingResolver_NegativeCacheSweepPreservesUnexpiredEntries(t *testing.T) {
	t.Parallel()

	const ttl = time.Minute
	baseErr := context.DeadlineExceeded
	base := &countingResolver{err: baseErr}
	start := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	clock := start
	resolver := newCachingResolverWithClock(base, newRecordingCache(), ttl, func() time.Time {
		return clock
	}).(*cachingResolver)

	half := negativeCacheMaxEntries / 2
	for i := 0; i < half; i++ {
		_, err := resolver.Resolve(context.Background(), fmt.Sprintf("did:plc:sweepexpired%04d", i))
		require.ErrorIs(t, err, baseErr)
	}
	clock = start.Add(ttl / 2)
	secondHalfDIDs := make([]string, 0, negativeCacheMaxEntries-half)
	for i := half; i < negativeCacheMaxEntries; i++ {
		did := fmt.Sprintf("did:plc:sweepunexpired%04d", i)
		secondHalfDIDs = append(secondHalfDIDs, did)
		_, err := resolver.Resolve(context.Background(), did)
		require.ErrorIs(t, err, baseErr)
	}
	require.Equal(t, negativeCacheMaxEntries, base.resolveCount())

	clock = start.Add(ttl + time.Nanosecond)
	_, err := resolver.Resolve(context.Background(), "did:plc:sweeptrigger")
	require.ErrorIs(t, err, baseErr)
	resolvesAfterSweep := base.resolveCount()

	for _, did := range secondHalfDIDs {
		_, err := resolver.Resolve(context.Background(), did)
		require.ErrorIs(t, err, baseErr)
	}
	assert.Equal(t, resolvesAfterSweep, base.resolveCount(),
		"capacity cleanup must remove only expired entries; evicting the unexpired half repeats thousands of attacker-controlled resolutions")
	resolver.negativeMu.Lock()
	remaining := len(resolver.negativeEntries)
	resolver.negativeMu.Unlock()
	assert.Equal(t, len(secondHalfDIDs)+1, remaining,
		"reaching the bound must reap every expired entry, not just the oldest one; a cache that keeps expired "+
			"attacker-controlled DIDs at the bound evicts live entries on every later store")
}

func TestCachingResolver_NegativeCacheConcurrentResolveIsRaceSafe(t *testing.T) {
	t.Parallel()

	const (
		did     = "did:plc:negativeconcurrent"
		workers = 16
	)
	baseErr := &ErrResolutionFailed{Identifier: did, Reason: "directory unavailable"}
	base := &countingResolver{err: baseErr}
	resolver := newCachingResolverWithClock(base, newRecordingCache(), DefaultNegativeCacheTTL, func() time.Time {
		return time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	})

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := resolver.Resolve(context.Background(), did)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.ErrorIs(t, err, baseErr, "every concurrent caller must receive the cached failure taxonomy")
	}
	assert.GreaterOrEqual(t, base.resolveCount(), 1, "at least one caller must populate the negative entry")
	assert.LessOrEqual(t, base.resolveCount(), workers,
		"concurrent misses may race to populate the entry, but must remain bounded and race-free")
}
