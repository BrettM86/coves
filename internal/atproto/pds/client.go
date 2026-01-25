// Package pds provides an abstraction layer for authenticated interactions with AT Protocol PDSs.
// It wraps indigo's atclient.APIClient to provide a consistent interface regardless of
// authentication method (OAuth with DPoP or password-based Bearer tokens).
package pds

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"Coves/internal/core/blobs"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Client provides authenticated access to a user's PDS repository.
// It abstracts the underlying authentication mechanism (OAuth/DPoP or password/Bearer)
// so services can make PDS calls without knowing how auth works.
type Client interface {
	// CreateRecord creates a record in the user's repository.
	// If rkey is empty, a TID will be generated.
	// Returns the record URI and CID.
	CreateRecord(ctx context.Context, collection string, rkey string, record any) (uri string, cid string, err error)

	// DeleteRecord deletes a record from the user's repository.
	DeleteRecord(ctx context.Context, collection string, rkey string) error

	// ListRecords lists records in a collection with pagination.
	// Returns records, next cursor (empty if no more), and error.
	ListRecords(ctx context.Context, collection string, limit int, cursor string) (*ListRecordsResponse, error)

	// GetRecord retrieves a single record by collection and rkey.
	GetRecord(ctx context.Context, collection string, rkey string) (*RecordResponse, error)

	// PutRecord creates or updates a record with optional optimistic locking.
	// If swapRecord CID is provided, the operation fails if the current CID doesn't match.
	PutRecord(ctx context.Context, collection string, rkey string, record any, swapRecord string) (uri string, cid string, err error)

	// UploadBlob uploads binary data to the user's PDS repository.
	// Returns a BlobRef that can be used in records.
	// Note: The mimeType parameter is accepted for interface compatibility, but the PDS
	// performs its own MIME type detection from the blob content. The returned BlobRef
	// will contain the PDS-detected MIME type.
	UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error)

	// DID returns the authenticated user's DID.
	DID() string

	// HostURL returns the PDS host URL.
	HostURL() string
}

// ListRecordsResponse contains the result of a ListRecords call.
type ListRecordsResponse struct {
	Records []RecordEntry
	Cursor  string
}

// RecordEntry represents a single record from a list operation.
type RecordEntry struct {
	URI   string
	CID   string
	Value map[string]any
}

// RecordResponse contains a single record retrieved from the PDS.
type RecordResponse struct {
	URI   string
	CID   string
	Value map[string]any
}

// client implements the Client interface using indigo's APIClient.
// This single implementation works for both OAuth (DPoP) and password (Bearer) auth
// because APIClient handles the authentication details internally.
type client struct {
	apiClient *atclient.APIClient
	did       string
	host      string
}

// Ensure client implements Client interface.
var _ Client = (*client)(nil)

// wrapAPIError inspects an error from atclient and wraps it with our typed errors.
// This allows callers to use errors.Is() for reliable error detection.
func wrapAPIError(err error, operation string) error {
	if err == nil {
		return nil
	}

	// Check if it's an APIError from atclient
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 400:
			return fmt.Errorf("%s: %w: %s", operation, ErrBadRequest, apiErr.Message)
		case 401:
			return fmt.Errorf("%s: %w: %s", operation, ErrUnauthorized, apiErr.Message)
		case 403:
			return fmt.Errorf("%s: %w: %s", operation, ErrForbidden, apiErr.Message)
		case 404:
			return fmt.Errorf("%s: %w: %s", operation, ErrNotFound, apiErr.Message)
		case 409:
			return fmt.Errorf("%s: %w: %s", operation, ErrConflict, apiErr.Message)
		case 413:
			return fmt.Errorf("%s: %w: %s", operation, ErrPayloadTooLarge, apiErr.Message)
		case 429:
			return fmt.Errorf("%s: %w: %s", operation, ErrRateLimited, apiErr.Message)
		}
	}

	// For other errors, wrap with operation context
	return fmt.Errorf("%s failed: %w", operation, err)
}

// DID returns the authenticated user's DID.
func (c *client) DID() string {
	return c.did
}

// HostURL returns the PDS host URL.
func (c *client) HostURL() string {
	return c.host
}

// CreateRecord creates a record in the user's repository.
func (c *client) CreateRecord(ctx context.Context, collection string, rkey string, record any) (string, string, error) {
	// Build request payload per com.atproto.repo.createRecord
	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"record":     record,
	}

	// Only include rkey if provided (PDS will generate TID if not)
	if rkey != "" {
		payload["rkey"] = rkey
	}

	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.createRecord"), payload, &result)
	if err != nil {
		return "", "", wrapAPIError(err, "createRecord")
	}

	return result.URI, result.CID, nil
}

// DeleteRecord deletes a record from the user's repository.
func (c *client) DeleteRecord(ctx context.Context, collection string, rkey string) error {
	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
	}

	// deleteRecord returns empty response on success
	err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.deleteRecord"), payload, nil)
	if err != nil {
		return wrapAPIError(err, "deleteRecord")
	}

	return nil
}

// ListRecords lists records in a collection with pagination.
func (c *client) ListRecords(ctx context.Context, collection string, limit int, cursor string) (*ListRecordsResponse, error) {
	params := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"limit":      limit,
	}

	if cursor != "" {
		params["cursor"] = cursor
	}

	var result struct {
		Cursor  string `json:"cursor"`
		Records []struct {
			URI   string         `json:"uri"`
			CID   string         `json:"cid"`
			Value map[string]any `json:"value"`
		} `json:"records"`
	}

	err := c.apiClient.Get(ctx, syntax.NSID("com.atproto.repo.listRecords"), params, &result)
	if err != nil {
		return nil, wrapAPIError(err, "listRecords")
	}

	// Convert to our response type
	response := &ListRecordsResponse{
		Cursor:  result.Cursor,
		Records: make([]RecordEntry, len(result.Records)),
	}

	for i, rec := range result.Records {
		response.Records[i] = RecordEntry{
			URI:   rec.URI,
			CID:   rec.CID,
			Value: rec.Value,
		}
	}

	return response, nil
}

// GetRecord retrieves a single record by collection and rkey.
func (c *client) GetRecord(ctx context.Context, collection string, rkey string) (*RecordResponse, error) {
	params := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
	}

	var result struct {
		URI   string         `json:"uri"`
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}

	err := c.apiClient.Get(ctx, syntax.NSID("com.atproto.repo.getRecord"), params, &result)
	if err != nil {
		return nil, wrapAPIError(err, "getRecord")
	}

	return &RecordResponse{
		URI:   result.URI,
		CID:   result.CID,
		Value: result.Value,
	}, nil
}

// PutRecord creates or updates a record with optional optimistic locking.
func (c *client) PutRecord(ctx context.Context, collection string, rkey string, record any, swapRecord string) (string, string, error) {
	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}

	// Optional: optimistic locking via CID swap check
	if swapRecord != "" {
		payload["swapRecord"] = swapRecord
	}

	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.putRecord"), payload, &result)
	if err != nil {
		return "", "", wrapAPIError(err, "putRecord")
	}

	return result.URI, result.CID, nil
}

// UploadBlob uploads binary data to the user's PDS repository.
func (c *client) UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	result, err := comatproto.RepoUploadBlob(ctx, c.apiClient, bytes.NewReader(data))
	if err != nil {
		return nil, wrapAPIError(err, "uploadBlob")
	}

	return &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": result.Blob.Ref.String()},
		MimeType: result.Blob.MimeType,
		Size:     int(result.Blob.Size),
	}, nil
}
