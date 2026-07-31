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
