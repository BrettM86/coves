package identity

import "context"

// Resolver provides methods for resolving atProto identities
type Resolver interface {
	// Resolve resolves a handle or DID to complete identity information
	// The identifier can be either:
	// - A handle (e.g., "alice.bsky.social")
	// - A DID (e.g., "did:plc:abc123")
	Resolve(ctx context.Context, identifier string) (*Identity, error)

	// ResolveHandle specifically resolves a handle to DID and PDS URL
	// This is a convenience method for handle-only resolution
	ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error)

	// ResolveDID retrieves a DID document and extracts the PDS endpoint
	ResolveDID(ctx context.Context, did string) (*DIDDocument, error)

	// Purge removes an identifier from the cache
	// The identifier can be either a handle or DID
	Purge(ctx context.Context, identifier string) error
}

// IdentityCache provides caching for resolved identities
type IdentityCache interface {
	// Get retrieves a cached identity by handle or DID
	Get(ctx context.Context, identifier string) (*Identity, error)

	// Set caches an identity with the given TTL
	// This should cache bidirectionally (both handle and DID as keys)
	Set(ctx context.Context, identity *Identity) error

	// Delete removes a cached identity by identifier
	Delete(ctx context.Context, identifier string) error

	// Purge removes all cache entries associated with an identifier
	// (both handle and DID if applicable)
	Purge(ctx context.Context, identifier string) error
}
