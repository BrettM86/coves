package identity

import (
	"context"
	"log"
)

// cachingResolver wraps a base resolver with caching
type cachingResolver struct {
	base  Resolver
	cache IdentityCache
}

// newCachingResolver creates a new caching resolver
func newCachingResolver(base Resolver, cache IdentityCache) Resolver {
	return &cachingResolver{
		base:  base,
		cache: cache,
	}
}

// Resolve resolves a handle or DID to complete identity information
// First checks cache, then falls back to base resolver
func (r *cachingResolver) Resolve(ctx context.Context, identifier string) (*Identity, error) {
	// Try cache first
	cached, err := r.cache.Get(ctx, identifier)
	if err == nil {
		// Cache hit - mark it as from cache
		cached.Method = MethodCache
		return cached, nil
	}

	// Cache miss - resolve using base resolver
	identity, err := r.base.Resolve(ctx, identifier)
	if err != nil {
		return nil, err
	}

	// Cache the resolved identity (ignore cache errors, just log them)
	if cacheErr := r.cache.Set(ctx, identity); cacheErr != nil {
		log.Printf("Warning: failed to cache identity for %s: %v", identifier, cacheErr)
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
	// Purge from cache
	if err := r.cache.Purge(ctx, identifier); err != nil {
		return err
	}

	// Propagate to base resolver (though it typically won't cache)
	return r.base.Purge(ctx, identifier)
}
