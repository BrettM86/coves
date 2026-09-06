package identity

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// cachingResolver wraps a base resolver with caching
type cachingResolver struct {
	base  Resolver
	cache IdentityCache

	negativeTTL     time.Duration
	now             func() time.Time
	negativeMu      sync.Mutex
	negativeEntries map[string]negativeCacheEntry
	negativeOrder   []string
}

// newCachingResolver creates a new caching resolver
func newCachingResolver(base Resolver, cache IdentityCache) Resolver {
	return newCachingResolverWithClock(base, cache, DefaultNegativeCacheTTL, time.Now)
}

// Resolve resolves a handle or DID to complete identity information
// First checks cache, then falls back to base resolver
func (r *cachingResolver) Resolve(ctx context.Context, identifier string) (*Identity, error) {
	// Try cache first
	cached, err := r.cache.Get(ctx, identifier)
	if err == nil {
		// A row carrying no usable handle is NOT AN ANSWER, and is treated
		// exactly as an empty cache would be.
		//
		// Declining to write these rows (below) does nothing for the ones
		// already stored: every resolution that ran while DNS could not answer
		// left one behind, and a hit never reaches the base resolver — so those
		// rows keep being served for the rest of their 24h TTL, and the
		// TRANSIENT classification every caller gives ErrHandleUnverified stays
		// a lie until they expire. Reading past them is what actually drains
		// them.
		//
		// PURGED rather than merely ignored. Leaving the row in place means the
		// next resolution pays for the same query and discards the same value,
		// and anything reading identity_cache directly — a person debugging,
		// above all — still finds the placeholder recorded against this DID as
		// though it were a fact about the identity. The purge failing is not
		// fatal: the caller still gets a correct fresh answer, and the write
		// below replaces the row anyway.
		if cached != nil && cached.Handle != "" && cached.Handle != InvalidHandle {
			// Cache hit - mark it as from cache
			cached.Method = MethodCache
			return cached, nil
		}
		if purgeErr := r.cache.Purge(ctx, identifier); purgeErr != nil {
			log.Printf("Warning: failed to purge unverified cached identity for %s: %v", identifier, purgeErr)
		}
	}

	if identity, found, err := r.getNegative(identifier); found {
		return identity, err
	}

	// Cache miss - resolve using base resolver
	identity, err := r.base.Resolve(ctx, identifier)
	if err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) {
			r.storeNegative(identifier, nil, err)
		}
		return nil, err
	}

	// Cache only a VERIFIED identity. An InvalidHandle (or empty) handle is not
	// a fact about the identity — it is a fact about the network at the instant
	// of the lookup: DNS was slow, the TXT record had not propagated, this
	// resolver had no egress. Writing it back converts that moment into a stored
	// one, and IDENTITY_CACHE_TTL is 24h, so a single transient DNS failure pins
	// the DID as unverifiable for a day, for every caller, with nothing to
	// notice it and nothing to purge unless someone thinks to.
	//
	// That also makes the TRANSIENT classification callers give
	// ErrHandleUnverified a lie: every redrive inside the TTL is served the
	// stored placeholder without reaching the network, so the event can never
	// recover and just exhausts its redrive budget. Same rule as an outright
	// resolution error above, which is likewise not cached — this is that
	// failure wearing a success's clothes.
	//
	// The caller still gets the answer; only the write is skipped.
	if identity != nil && identity.Handle != "" && identity.Handle != InvalidHandle {
		// Cache the resolved identity (ignore cache errors, just log them)
		if cacheErr := r.cache.Set(ctx, identity); cacheErr != nil {
			log.Printf("Warning: failed to cache identity for %s: %v", identifier, cacheErr)
		}
	} else {
		r.storeNegative(identifier, identity, nil)
	}

	return identity, nil
}

// ResolveHandle specifically resolves a handle to DID and PDS URL
func (r *cachingResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	identity, err := r.Resolve(ctx, handle)
	if err != nil {
		return "", "", err
	}

	return identity.DID, identity.PDSURL, nil
}

// ResolveDID retrieves a DID document and extracts the PDS endpoint
func (r *cachingResolver) ResolveDID(ctx context.Context, did string) (*DIDDocument, error) {
	// Try to get from cache first
	cached, err := r.cache.Get(ctx, did)
	if err == nil {
		// We have cached identity, construct a simple DID document
		return &DIDDocument{
			DID: cached.DID,
			Service: []Service{
				{
					ID:              "#atproto_pds",
					Type:            "AtprotoPersonalDataServer",
					ServiceEndpoint: cached.PDSURL,
				},
			},
		}, nil
	}

	// Cache miss - use base resolver
	return r.base.ResolveDID(ctx, did)
}

// Purge removes an identifier from the cache and propagates to base
func (r *cachingResolver) Purge(ctx context.Context, identifier string) error {
	r.deleteNegative(identifier)

	// Purge from cache
	if err := r.cache.Purge(ctx, identifier); err != nil {
		return err
	}

	// Propagate to base resolver (though it typically won't cache)
	return r.base.Purge(ctx, identifier)
}
