package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The caching resolver: which of two sources answers, and what a cache failure
// is allowed to cost.
//
// This wrapper is the resolver everything in the AppView actually holds —
// NewResolver returns it, never the base one — so its policy decisions are the
// system's. There are three, and each one has a cheap wrong version:
//
//   - A cache hit must be labelled as such. Callers that need a genuinely fresh
//     answer (a purge-then-resolve, a handle-change reconciliation) tell the two
//     apart by Method and by nothing else.
//   - A cache WRITE failure must not fail the request. The identity was
//     resolved; refusing to return it because a table was unavailable turns a
//     degraded cache into an outage.
//   - A cache READ failure is indistinguishable from a miss, on purpose. Both
//     mean "ask the network", and treating a broken cache as authoritative
//     would be worse.
//
// The sibling identity_cache_test.go covers the Postgres implementation of the
// cache against real SQL; this file covers the policy above against a fake, and
// the two do not overlap.

// recordingCache is an IdentityCache that answers from a map and remembers what
// it was asked.
type recordingCache struct {
	mu      sync.Mutex
	entries map[string]*Identity

	getErr   error
	setErr   error
	purgeErr error

	gets   []string
	sets   []*Identity
	purges []string
}

func newRecordingCache() *recordingCache {
	return &recordingCache{entries: map[string]*Identity{}}
}

func (c *recordingCache) Get(_ context.Context, identifier string) (*Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = append(c.gets, identifier)
	if c.getErr != nil {
		return nil, c.getErr
	}
	if ident, ok := c.entries[identifier]; ok {
		return ident, nil
	}
	return nil, &ErrCacheMiss{Identifier: identifier}
}

func (c *recordingCache) Set(_ context.Context, ident *Identity) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets = append(c.sets, ident)
	if c.setErr != nil {
		return c.setErr
	}
	c.entries[ident.DID] = ident
	if ident.Handle != "" {
		c.entries[ident.Handle] = ident
	}
	return nil
}

func (c *recordingCache) Delete(_ context.Context, identifier string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, identifier)
	return nil
}

func (c *recordingCache) Purge(_ context.Context, identifier string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purges = append(c.purges, identifier)
	if c.purgeErr != nil {
		return c.purgeErr
	}
	delete(c.entries, identifier)
	return nil
}

// countingResolver is the base a cachingResolver wraps: it answers from fixed
// values and counts how often it was consulted, which is the only way to say
// "the cache was used" rather than "the cache produced the same answer".
type countingResolver struct {
	identity *Identity
	doc      *DIDDocument
	err      error

	resolves    int
	resolveDIDs int
	purges      []string
}

func (r *countingResolver) Resolve(context.Context, string) (*Identity, error) {
	r.resolves++
	if r.err != nil {
		return nil, r.err
	}
	// A copy, because the caching resolver mutates what it is handed.
	clone := *r.identity
	return &clone, nil
}

func (r *countingResolver) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	ident, err := r.Resolve(ctx, handle)
	if err != nil {
		return "", "", err
	}
	return ident.DID, ident.PDSURL, nil
}

func (r *countingResolver) ResolveDID(context.Context, string) (*DIDDocument, error) {
	r.resolveDIDs++
	if r.err != nil {
		return nil, r.err
	}
	return r.doc, nil
}

func (r *countingResolver) Purge(_ context.Context, identifier string) error {
	r.purges = append(r.purges, identifier)
	return nil
}

func freshIdentity() *Identity {
	return &Identity{
		DID:        fixtureDID,
		Handle:     fixtureHandle,
		PDSURL:     fixturePDS,
		ResolvedAt: time.Now().UTC(),
		Method:     MethodHTTPS,
	}
}

func newCachingFixture() (Resolver, *countingResolver, *recordingCache) {
	base := &countingResolver{
		identity: freshIdentity(),
		doc: &DIDDocument{DID: fixtureDID, Service: []Service{
			{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: fixturePDS},
		}},
	}
	cache := newRecordingCache()
	return newCachingResolver(base, cache), base, cache
}

func TestCachingResolver_MissResolvesAndCaches(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()

	got, err := resolver.Resolve(context.Background(), fixtureHandle)
	require.NoError(t, err)

	assert.Equal(t, 1, base.resolves, "an uncached identifier must reach the network")
	assert.Equal(t, []string{fixtureHandle}, cache.gets,
		"the cache must be consulted with the identifier as given: normalising it here and not there "+
			"produces a cache that never hits")
	require.Len(t, cache.sets, 1, "a resolved identity that is not written back makes the cache useless")
	assert.Equal(t, MethodHTTPS, got.Method, "a fresh resolution is not a cache hit")
	assert.Equal(t, fixtureDID, got.DID)
}

// unverifiedIdentity is what resolution returns when the DID document was read
// but the handle could not be confirmed against DNS: the reserved placeholder in
// Handle, and a perfectly good PDS alongside it, since the PDS comes from the
// document and needs no DNS at all.
func unverifiedIdentity() *Identity {
	return &Identity{
		DID:        fixtureDID,
		Handle:     InvalidHandle,
		PDSURL:     fixturePDS,
		ResolvedAt: time.Now().UTC(),
		Method:     MethodHTTPS,
	}
}

// TestCachingResolver_DoesNotCacheAnUnverifiedIdentity is a bug that predates
// the handle-binding work and is worth stating on its own terms.
//
// InvalidHandle is not a fact about an identity. It is a fact about the NETWORK
// at the instant of the lookup: DNS was slow, the TXT record had not propagated,
// the resolver had no egress. Caching it converts a momentary condition into a
// stored one, and IDENTITY_CACHE_TTL is 24h in .env.ci — so a single transient
// DNS failure pins a DID as unverifiable for a day, for every caller, with no
// way to notice and nothing to purge unless someone thinks to.
//
// The consumer already treats an unverified handle as TRANSIENT and expects the
// redrive to succeed (identity.ErrHandleUnverified is classified that way at
// every call site). A cache that memoises the failure makes that expectation
// false: every retry inside the TTL gets the same stored placeholder without a
// single network call, so the event can never recover and eventually exhausts
// its redrive budget.
//
// This is the same rule TestCachingResolver_PropagatesAResolutionFailure states
// for an outright error — a failed resolution must not be cached, or an account
// stays invisible for a full TTL after the reason went away. A handle.invalid
// result is that failure wearing a success's clothes.
func TestCachingResolver_DoesNotCacheAnUnverifiedIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		handle string
		why    string
	}{
		{
			name:   "the reserved placeholder",
			handle: InvalidHandle,
			why:    "resolution reports an unverifiable handle as a SUCCESS carrying this string",
		},
		{
			name:   "an empty handle",
			handle: "",
			why:    "an identity with no handle establishes nothing worth remembering, and the cache is keyed by handle too",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver, base, cache := newCachingFixture()
			unverified := unverifiedIdentity()
			unverified.Handle = tc.handle
			base.identity = unverified

			got, err := resolver.Resolve(context.Background(), fixtureDID)
			require.NoError(t, err, "an unverifiable handle is not a resolution error: %s", tc.why)
			assert.Equal(t, tc.handle, got.Handle, "the caller still gets the answer, unchanged")

			assert.Empty(t, cache.sets,
				"the unverified result was written to a cache with a 24h TTL: one transient DNS failure "+
					"now pins this DID as unverifiable for a day, and the transient classification every "+
					"caller applies to it becomes a lie")

			_, err = resolver.Resolve(context.Background(), fixtureDID)
			require.NoError(t, err)
			assert.Equal(t, 2, base.resolves,
				"the second resolution was served from cache. Nothing about the identity changed — only "+
					"the network did — so the next attempt must be allowed to reach it")
		})
	}
}

// TestCachingResolver_TreatsAnUnverifiedCachedRowAsAMiss closes the other half
// of the unverified-identity rule, and it is the half that governs the rows that
// already exist.
//
// Not writing them is only forward-looking. identity_cache is a live Postgres
// table with a 24h TTL, so every placeholder written before that fix is still
// there and still being served — and a cache HIT never reaches the base
// resolver, so nothing about the fix makes those rows go away. For up to a day
// after the deploy, the DIDs that happened to be resolved during a DNS wobble
// keep answering handle.invalid to every caller, from a cache that will not ask
// again. The bug looks fixed and behaves exactly as before for the identities it
// already bit.
//
// So a hit that carries no usable handle has to be treated as what it actually
// is: not an answer. Consult the base, return the verified result, and PURGE the
// row rather than leaving it to expire — a stale entry that is read, ignored,
// and left in place is read again by the next caller and ignored again, which
// costs a query per resolution and keeps the wrong value discoverable by
// anything that reads the table directly.
//
// This is the same shape as TestCachingResolver_PropagatesAResolutionFailure's
// rule for an outright error — a failure must not be cached, or an account stays
// invisible for a full TTL after the reason went away — applied to the failure
// that arrives dressed as a success.
func TestCachingResolver_TreatsAnUnverifiedCachedRowAsAMiss(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		handle string
		why    string
	}{
		{
			name:   "the reserved placeholder",
			handle: InvalidHandle,
			why:    "written by every resolution that happened while DNS could not answer",
		},
		{
			name:   "an empty handle",
			handle: "",
			why:    "a row that establishes no handle at all answers nothing a caller can use",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver, base, cache := newCachingFixture()
			stale := unverifiedIdentity()
			stale.Handle = tc.handle
			cache.entries[fixtureDID] = stale

			got, err := resolver.Resolve(context.Background(), fixtureDID)
			require.NoError(t, err)

			assert.Equal(t, fixtureHandle, got.Handle,
				"the caller was handed %q from cache: %s. A hit never reaches the base resolver, so "+
					"these rows keep answering for the rest of their 24h TTL — the fix that stopped "+
					"WRITING them does nothing for the ones already there", tc.handle, tc.why)
			assert.Equal(t, 1, base.resolves,
				"a cached row carrying no usable handle is not an answer, and must send the caller to "+
					"the network exactly as an empty cache would")

			assert.Contains(t, cache.purges, fixtureDID,
				"the stale row was read, ignored, and left in place — so the next resolution pays for "+
					"the same query, ignores the same value, and anything reading identity_cache "+
					"directly still finds %q against this DID", tc.handle)

			assert.Len(t, cache.sets, 1,
				"treated as a miss means treated as a miss: the verified answer must be written back, "+
					"or every resolution of this DID re-resolves until something else caches it")
		})
	}
}

func TestCachingResolver_HitIsServedWithoutTheNetworkAndSaysSo(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()
	cache.entries[fixtureHandle] = freshIdentity()

	got, err := resolver.Resolve(context.Background(), fixtureHandle)
	require.NoError(t, err)

	assert.Zero(t, base.resolves,
		"a cached identity was re-resolved. Handle resolution costs a DNS lookup or an HTTPS fetch "+
			"against somebody else's server, on a path that runs for every record ingested")
	assert.Empty(t, cache.sets, "a hit must not rewrite what it just read")
	assert.Equal(t, MethodCache, got.Method,
		"a hit must be labelled as one: tests/live's purge check, and anything else that needs a "+
			"provably fresh answer, has nothing else to look at")
	assert.Equal(t, fixtureDID, got.DID)
}

// TestCachingResolver_HitMutatesTheCachedValue pins a sharp edge rather than a
// behaviour worth keeping.
//
// The hit path sets Method on the object the cache handed back, in place. With
// postgresCache that is harmless — every Get scans a fresh struct — but the
// interface says nothing about ownership, and an implementation that returned a
// shared pointer (an in-memory cache, the obvious next one to write) would have
// its stored entry relabelled MethodCache by the first reader. Every later hit
// would still say MethodCache, so nothing breaks visibly; what breaks is the
// ability to ever distinguish a stored fresh resolution again.
func TestCachingResolver_HitMutatesTheCachedValue(t *testing.T) {
	t.Parallel()
	resolver, _, cache := newCachingFixture()
	stored := freshIdentity()
	cache.entries[fixtureHandle] = stored
	require.Equal(t, MethodHTTPS, stored.Method)

	_, err := resolver.Resolve(context.Background(), fixtureHandle)
	require.NoError(t, err)

	assert.Equal(t, MethodCache, stored.Method,
		"IF THIS FAILED, the caching resolver stopped writing through to the object the cache owns — "+
			"which is the safer design. Update this test to assert the copy instead of reverting")
}

func TestCachingResolver_ADegradedCacheIsNotAnOutage(t *testing.T) {
	t.Parallel()

	t.Run("a cache that cannot be read falls through to the network", func(t *testing.T) {
		t.Parallel()
		resolver, base, cache := newCachingFixture()
		cache.getErr = errors.New("connection pool exhausted")

		got, err := resolver.Resolve(context.Background(), fixtureHandle)
		require.NoError(t, err, "an unreadable cache must not fail resolution: the network still knows "+
			"the answer, and the whole AppView stops ingesting if it does not get one")
		assert.Equal(t, 1, base.resolves)
		assert.Equal(t, fixtureDID, got.DID)
	})

	t.Run("a cache that cannot be written still returns the identity", func(t *testing.T) {
		t.Parallel()
		resolver, _, cache := newCachingFixture()
		cache.setErr = errors.New("disk full")

		got, err := resolver.Resolve(context.Background(), fixtureHandle)
		require.NoError(t, err, "the identity was resolved; refusing to hand it over because it could "+
			"not be memoised turns a slow cache into a broken product")
		assert.Equal(t, fixtureDID, got.DID)
	})
}

func TestCachingResolver_PropagatesAResolutionFailure(t *testing.T) {
	t.Parallel()
	resolver, base, cache := newCachingFixture()
	base.err = &ErrNotFound{Identifier: fixtureHandle, Reason: "no such handle"}

	_, err := resolver.Resolve(context.Background(), fixtureHandle)
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound,
		"the base resolver's taxonomy must survive the wrapper: a caller that routes 404 versus 502 "+
			"reads the error it gets from THIS resolver")
	assert.Empty(t, cache.sets,
		"a failed resolution must not be cached. A negative entry would keep an account invisible for "+
			"a full TTL after the reason it failed went away")
}

func TestCachingResolver_ResolveHandle(t *testing.T) {
	t.Parallel()

	t.Run("goes through the cache", func(t *testing.T) {
		t.Parallel()
		resolver, base, cache := newCachingFixture()
		cache.entries[fixtureHandle] = freshIdentity()

		did, pdsURL, err := resolver.ResolveHandle(context.Background(), fixtureHandle)
		require.NoError(t, err)
		assert.Equal(t, fixtureDID, did)
		assert.Equal(t, fixturePDS, pdsURL)
		assert.Zero(t, base.resolves, "the convenience method must not bypass the cache the wrapper exists for")
	})

	t.Run("propagates the failure as an empty pair plus an error", func(t *testing.T) {
		t.Parallel()
		resolver, base, _ := newCachingFixture()
		base.err = errors.New("plc unreachable")

		did, pdsURL, err := resolver.ResolveHandle(context.Background(), fixtureHandle)
		require.Error(t, err)
		assert.Empty(t, did)
		assert.Empty(t, pdsURL)
	})
}

func TestCachingResolver_ResolveDID(t *testing.T) {
	t.Parallel()

	t.Run("reconstructs a document from a cache hit", func(t *testing.T) {
		t.Parallel()
		resolver, base, cache := newCachingFixture()
		cache.entries[fixtureDID] = freshIdentity()

		doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
		require.NoError(t, err)
		assert.Zero(t, base.resolveDIDs)
		assert.Equal(t, fixtureDID, doc.DID)
		require.Len(t, doc.Service, 1)
		assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	})

	t.Run("falls through to the base resolver on a miss", func(t *testing.T) {
		t.Parallel()
		resolver, base, _ := newCachingFixture()

		doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
		require.NoError(t, err)
		assert.Equal(t, 1, base.resolveDIDs)
		assert.Equal(t, fixtureDID, doc.DID)
	})

	// The two paths do not agree on what "no PDS" looks like. The base resolver
	// omits the service entry entirely (asserted in base_resolver_test.go); the
	// cache-hit path always emits one, so a cached identity with an empty PDS
	// URL produces a document that claims a PDS at "". A caller checking
	// len(doc.Service) gets a different answer depending on cache state, which
	// is the kind of difference that only shows up under load.
	t.Run("emits an empty service entry for a cached identity with no PDS", func(t *testing.T) {
		t.Parallel()
		resolver, _, cache := newCachingFixture()
		noPDS := freshIdentity()
		noPDS.PDSURL = ""
		cache.entries[fixtureDID] = noPDS

		doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
		require.NoError(t, err)
		require.Lenf(t, doc.Service, 1,
			"IF THIS FAILED, the cache-hit path learned to omit an empty PDS entry like the base path "+
				"already does. That closes the asymmetry; assert the new agreement instead of "+
				"reverting")
		assert.Empty(t, doc.Service[0].ServiceEndpoint,
			"pinned, not endorsed: a caller that reads Service[0] gets \"\" as a host to dial")
	})

	t.Run("propagates the base resolver's failure on a miss", func(t *testing.T) {
		t.Parallel()
		resolver, base, _ := newCachingFixture()
		base.err = &ErrResolutionFailed{Identifier: fixtureDID, Reason: "plc timeout"}

		_, err := resolver.ResolveDID(context.Background(), fixtureDID)
		var failed *ErrResolutionFailed
		require.ErrorAs(t, err, &failed)
	})
}

func TestCachingResolver_Purge(t *testing.T) {
	t.Parallel()

	t.Run("clears the cache and tells the base resolver", func(t *testing.T) {
		t.Parallel()
		resolver, base, cache := newCachingFixture()
		cache.entries[fixtureHandle] = freshIdentity()

		require.NoError(t, resolver.Purge(context.Background(), fixtureHandle))
		assert.Equal(t, []string{fixtureHandle}, cache.purges)
		assert.Equal(t, []string{fixtureHandle}, base.purges,
			"the base resolver holds no cache today, but Indigo's directory may; a purge that stops "+
				"there is a purge that does not purge")

		// The point of a purge is the next read.
		got, err := resolver.Resolve(context.Background(), fixtureHandle)
		require.NoError(t, err)
		assert.Equal(t, MethodHTTPS, got.Method,
			"after a purge the next resolution must come from the network. This is the property "+
				"tests/live's purgeIdentityCache relies on to prove a handle change was picked up")
	})

	t.Run("stops at a cache that could not be purged", func(t *testing.T) {
		t.Parallel()
		resolver, base, cache := newCachingFixture()
		cache.purgeErr = errors.New("statement timeout")

		require.Error(t, resolver.Purge(context.Background(), fixtureHandle))
		assert.Empty(t, base.purges,
			"reporting a purge as failed while having partially performed it is worse than not "+
				"performing it: the caller retries, and the retry is the only thing that can fix the "+
				"entry that is still there")
	})
}
