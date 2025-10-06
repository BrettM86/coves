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
	// Create Indigo's BaseDirectory which handles DNS and HTTPS resolution
	dir := &indigoIdentity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: *httpClient,
		// Indigo will use default DNS resolver if not specified
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
	ident, err := r.directory.Lookup(ctx, *atID)

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
