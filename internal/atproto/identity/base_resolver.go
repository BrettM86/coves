package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// baseResolver implements Resolver using Indigo's identity resolution
type baseResolver struct {
	directory indigoIdentity.Directory
}

// newBaseResolver creates a new base resolver using Indigo
func newBaseResolver(plcURL string, httpClient *http.Client) Resolver {
	return newBaseResolverWithWellKnownHosts(plcURL, httpClient, nil)
}

// newBaseResolverWithWellKnownHosts is newBaseResolver plus the dev/CI override
// that redirects the HTTP leg of handle verification for the suffixes it names.
//
// Both halves of the override are applied HERE rather than in the factory,
// because both belong to the same directory and applying them in two places is
// how one of them ends up missing: the client wrapping and
// SkipDNSDomainSuffixes have to describe the same set of suffixes or a mapped
// handle spends a DNS timeout before reaching the host we configured for it.
// This is also the one place the directory is built, so the wrapping cannot be
// applied twice.
//
// hosts is nil for every resolver but the dev and CI ones, and a nil map leaves
// the client and the directory exactly as they were.
func newBaseResolverWithWellKnownHosts(plcURL string, httpClient *http.Client, hosts map[string]string) Resolver {
	// Create Indigo's BaseDirectory which handles DNS and HTTPS resolution
	dir := &indigoIdentity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: *httpClient,
		// Indigo will use default DNS resolver if not specified
	}

	if len(hosts) > 0 {
		// WRAPPING, not replacing: the SSRF-safe transport still vets and dials
		// the rewritten address. It permits the loopback host this points at only
		// because WithPrivateHostsAllowed was required alongside.
		dir.HTTPClient.Transport = newWellKnownRewriteTransport(dir.HTTPClient.Transport, hosts)
		dir.SkipDNSDomainSuffixes = wellKnownSuffixes(hosts)
	}

	return &baseResolver{
		directory: dir,
	}
}

// Resolve resolves a handle or DID to complete identity information
func (r *baseResolver) Resolve(ctx context.Context, identifier string) (*Identity, error) {
	identifier = strings.TrimSpace(identifier)

	if identifier == "" {
		return nil, &ErrInvalidIdentifier{
			Identifier: identifier,
			Reason:     "identifier cannot be empty",
		}
	}

	// Parse the identifier (could be handle or DID)
	atID, err := syntax.ParseAtIdentifier(identifier)
	if err != nil {
		return nil, &ErrInvalidIdentifier{
			Identifier: identifier,
			Reason:     fmt.Sprintf("invalid identifier format: %v", err),
		}
	}

	// Resolve using Indigo's directory
	ident, err := r.directory.Lookup(ctx, atID)
	if err != nil {
		// Check if it's a "not found" error
		errStr := err.Error()
		if strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "NoRecordsFound") ||
			strings.Contains(errStr, "404") {
			return nil, &ErrNotFound{
				Identifier: identifier,
				Reason:     errStr,
			}
		}

		return nil, &ErrResolutionFailed{
			Identifier: identifier,
			Reason:     errStr,
		}
	}

	// Extract PDS URL from identity
	pdsURL := ident.PDSEndpoint()

	return &Identity{
		DID:        ident.DID.String(),
		Handle:     ident.Handle.String(),
		PDSURL:     pdsURL,
		ResolvedAt: time.Now().UTC(),
		Method:     MethodHTTPS, // Default - Indigo doesn't expose which method was used
	}, nil
}

// ResolveHandle specifically resolves a handle to DID and PDS URL
func (r *baseResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	ident, err := r.Resolve(ctx, handle)
	if err != nil {
		return "", "", err
	}

	return ident.DID, ident.PDSURL, nil
}

// ResolveDID retrieves a DID document and extracts the PDS endpoint
func (r *baseResolver) ResolveDID(ctx context.Context, didStr string) (*DIDDocument, error) {
	did, err := syntax.ParseDID(didStr)
	if err != nil {
		return nil, &ErrInvalidIdentifier{
			Identifier: didStr,
			Reason:     fmt.Sprintf("invalid DID format: %v", err),
		}
	}

	ident, err := r.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, &ErrResolutionFailed{
			Identifier: didStr,
			Reason:     err.Error(),
		}
	}

	// Construct our DID document from Indigo's identity
	doc := &DIDDocument{
		DID:     ident.DID.String(),
		Service: []Service{},
	}

	// Extract PDS service endpoint
	pdsURL := ident.PDSEndpoint()
	if pdsURL != "" {
		doc.Service = append(doc.Service, Service{
			ID:              "#atproto_pds",
			Type:            "AtprotoPersonalDataServer",
			ServiceEndpoint: pdsURL,
		})
	}

	return doc, nil
}

// Purge is a no-op for base resolver (no caching)
func (r *baseResolver) Purge(ctx context.Context, identifier string) error {
	// Base resolver doesn't cache, so nothing to purge
	return nil
}
