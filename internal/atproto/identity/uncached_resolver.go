package identity

import (
	"context"
	"errors"
	"fmt"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// DeclaredIdentity is what a DID document asserts about itself, read without
// any handle resolution. DeclaredHandle is NOT verified: the document's owner
// writes it, so it is only good for deciding whether verifying it is worth a
// network call. It is empty when the document declares no valid handle.
type DeclaredIdentity struct {
	DID            string
	DeclaredHandle string
	PDSURL         string
}

// UncachedResolver is the resolver NewUncachedResolver returns: a Resolver that
// never caches, plus a DID document read with no handle I/O.
type UncachedResolver interface {
	Resolver

	// ResolveDeclaredIdentity fetches did's document through the same PLC
	// directory and SSRF-guarded client as Resolve, and makes no DNS or
	// well-known request for the handle it declares. A caller that would reject
	// the identity on its document alone can do so before Resolve spends time
	// on a handle domain the identity's owner controls.
	ResolveDeclaredIdentity(ctx context.Context, did string) (*DeclaredIdentity, error)
}

// NewUncachedResolver resolves identities without a cache so identity-event
// refresh does not trust an answer cached before the event arrived.
func NewUncachedResolver(config Config) UncachedResolver {
	requireWellKnownHostsGate(config)
	if config.PLCURL == "" {
		config.PLCURL = "https://plc.directory"
	}
	if config.httpClient == nil {
		config.httpClient = resolverHTTPClient(config.allowPrivateHosts)
	}
	return newBaseResolverWithWellKnownHosts(config.PLCURL, config.httpClient, config.wellKnownHosts)
}

// ResolveDeclaredIdentity implements UncachedResolver. Errors are classified
// like Resolve's: ErrInvalidIdentifier, ErrNotFound, or ErrResolutionFailed.
func (r *baseResolver) ResolveDeclaredIdentity(ctx context.Context, didStr string) (*DeclaredIdentity, error) {
	did, err := syntax.ParseDID(didStr)
	if err != nil {
		return nil, &ErrInvalidIdentifier{
			Identifier: didStr,
			Reason:     fmt.Sprintf("invalid DID format: %v", err),
		}
	}
	doc, err := r.directory.ResolveDID(ctx, did)
	if err != nil {
		if errors.Is(err, indigoIdentity.ErrDIDNotFound) {
			return nil, &ErrNotFound{Identifier: didStr, Reason: err.Error()}
		}
		return nil, &ErrResolutionFailed{Identifier: didStr, Reason: err.Error(), Err: err}
	}
	ident := indigoIdentity.ParseIdentity(doc)
	declared := &DeclaredIdentity{DID: ident.DID.String(), PDSURL: ident.PDSEndpoint()}
	if handle, handleErr := ident.DeclaredHandle(); handleErr == nil {
		declared.DeclaredHandle = handle.String()
	}
	return declared, nil
}
