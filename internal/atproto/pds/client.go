// Package pds provides an abstraction layer for authenticated interactions with AT Protocol PDSs.
// It wraps indigo's atclient.APIClient to provide a consistent interface regardless of
// authentication method (OAuth with DPoP or password-based Bearer tokens).
package pds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"Coves/internal/core/blobs"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
	lexutil "github.com/bluesky-social/indigo/lex/util"
)

// Client provides authenticated access to a user's PDS repository.
// It abstracts the underlying authentication mechanism (OAuth/DPoP or password/Bearer)
// so services can make PDS calls without knowing how auth works.
type Client interface {
	// CreateRecord creates a record in the user's repository.
	// If rkey is empty, a TID will be generated.
	// Returns the record URI and CID.
	CreateRecord(ctx context.Context, collection, rkey string, record any) (uri, cid string, err error)

	// DeleteRecord deletes a record from the user's repository.
	DeleteRecord(ctx context.Context, collection, rkey string) error

	// ListRecords lists records in a collection with pagination.
	// Returns records, next cursor (empty if no more), and error.
	ListRecords(ctx context.Context, collection string, limit int, cursor string) (*ListRecordsResponse, error)

	// GetRecord retrieves a single record by collection and rkey.
	GetRecord(ctx context.Context, collection, rkey string) (*RecordResponse, error)

	// PutRecord creates or updates a record with optional optimistic locking.
	// If swapRecord CID is provided, the operation fails if the current CID doesn't match.
	PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (uri, cid string, err error)

	// UploadBlob uploads binary data to the user's PDS repository.
	// Returns a BlobRef that can be used in records.
	// mimeType is required and is sent as the request Content-Type: a PDS
	// enforcing the granular blob:*/* OAuth scope matches the granted accept
	// patterns against it and rejects wildcard content types, so it must be
	// the blob's concrete MIME type (e.g. "image/png").
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
		// THE NAME CHECKS RUN FIRST, BEFORE EVERY STATUS BRANCH — including the
		// 5xx one. The name says what happened; the status only says how the
		// server framed it, and PDS implementations and the proxies in front of
		// them disagree on the framing. A lost swap comes back from a live PDS
		// as HTTP 400 with "error": "InvalidSwap", not the 409 the lexicon
		// documents; a 500 carrying InvalidSwap is STILL a lost swap, and a
		// caller that saw only ErrServerError would resend the same shape
		// instead of re-reading — the exact behaviour the sentinel exists to
		// prevent.
		//
		// A 409 InvalidSwap — what the lexicon says, and what some
		// implementation may yet send — is BOTH sentinels at once, so callers
		// written against either one behave correctly whichever status arrives.
		if apiErr.Name == "InvalidSwap" {
			if apiErr.StatusCode == 409 {
				return fmt.Errorf("%s: %w: %w: %s", operation, ErrConflict, ErrSwapConflict, apiErr.Message)
			}
			return fmt.Errorf("%s: %w: %s", operation, ErrSwapConflict, apiErr.Message)
		}

		// "No such record" is the other name that outranks its status. The
		// reference PDS answers getRecord for a missing record with HTTP 400 and
		// "error": "RecordNotFound" (internal/core/users/profile_backfill.go
		// documents the same observation), so the status alone calls an absent
		// record a malformed request. A writer that shapes create-vs-update from
		// a pre-read cannot tell those apart, and every caller already testing
		// errors.Is(err, ErrNotFound) after a GetRecord is silently never true.
		//
		// THE NAME IS THE ONLY THING TRUSTED HERE — never the message. The
		// reference PDS also spells some misses as InvalidRequest with "could
		// not locate record" in the MESSAGE (the getProfile shape;
		// internal/core/users/profile_backfill.go matches it deliberately, at
		// its own call site, against that one operation). That spelling is NOT
		// mapped at this layer: a transport-wide substring match would turn any
		// error that merely mentions those words into ErrNotFound for every
		// caller of every method. Our PDS answers the record operations this
		// client wraps with the RecordNotFound name — pinned by the idempotent
		// re-delete in service_writeforward_test.go, which fails if that ever
		// stops being true.
		if apiErr.Name == "RecordNotFound" || apiErr.Name == "NotFound" {
			return fmt.Errorf("%s: %w: %s", operation, ErrNotFound, apiErr.Message)
		}

		// A 5xx is its own class, not the generic wrap. applyWrites answers a
		// delete of a missing record — and a create of an existing one — with a
		// 500, and a state-shaped writer meeting that has to know its pre-read
		// went stale so it can re-shape the batch.
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("%s: %w: %s", operation, ErrServerError, apiErr.Message)
		}

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
func (c *client) CreateRecord(ctx context.Context, collection, rkey string, record any) (string, string, error) {
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

	// A 200 with an empty uri/cid means the PDS (or a proxy in front of it)
	// returned a malformed body; callers must never mistake it for success
	if result.URI == "" || result.CID == "" {
		return "", "", fmt.Errorf("createRecord: PDS returned success without uri/cid (collection %s)", collection)
	}

	return result.URI, result.CID, nil
}

// DeleteRecord deletes a record from the user's repository.
func (c *client) DeleteRecord(ctx context.Context, collection, rkey string) error {
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
func (c *client) GetRecord(ctx context.Context, collection, rkey string) (*RecordResponse, error) {
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

// maxPDSErrorDetailBytes caps each text a PDS error body contributes to a
// returned error, so a hostile PDS cannot flood our logs through the wrapped
// message.
const maxPDSErrorDetailBytes = 256

// BoundedRecordGetter reads a single record while refusing a response body
// larger than maxBytes.
type BoundedRecordGetter interface {
	GetRecordBounded(ctx context.Context, collection, rkey string, maxBytes int64) (*RecordResponse, error)
}

// Ensure the concrete client provides the bounded read.
var _ BoundedRecordGetter = (*client)(nil)

// GetRecordBounded retrieves a single record by collection and rkey, reading
// at most maxBytes of the response body.
//
// The body is read through a limit of maxBytes+1: the extra probe byte tells a
// body of exactly maxBytes from a larger one, so an over-cap or endless body
// fails without being consumed further and never yields a partial record.
// Error responses are read under the same cap and mapped like GetRecord's, with
// two differences: the PDS's error texts are capped at maxPDSErrorDetailBytes,
// and ErrNotFound follows IsRecordNotFoundResponse, so a 404 without an XRPC
// error name is not ErrNotFound. With no envelope the request most likely never
// reached a PDS (a stale pds_url pointing at a generic web server answers 404
// for everything), so it says nothing about the record. A 2xx without a record
// value is an error, never an empty record a caller could merge over the real
// one. A response that cannot be used (over the cap, undecodable, no value, a
// bare 404) wraps ErrMalformedResponse.
func (c *client) GetRecordBounded(ctx context.Context, collection, rkey string, maxBytes int64) (*RecordResponse, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("getRecord: maxBytes must be positive, got %d", maxBytes)
	}

	request := atclient.NewAPIRequest(http.MethodGet, syntax.NSID("com.atproto.repo.getRecord"), nil)
	request.Headers.Set("Accept", "application/json")
	request.QueryParams.Set("repo", c.did)
	request.QueryParams.Set("collection", collection)
	request.QueryParams.Set("rkey", rkey)

	response, err := c.apiClient.Do(ctx, request)
	if err != nil {
		return nil, wrapAPIError(err, "getRecord")
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("getRecord: reading response body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("getRecord: %w: response body exceeds %d bytes (collection %s)", ErrMalformedResponse, maxBytes, collection)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var errorBody atclient.ErrorBody
		if json.Unmarshal(body, &errorBody) != nil {
			errorBody = atclient.ErrorBody{}
		}
		if IsRecordNotFoundResponse(response.StatusCode, body) {
			return nil, fmt.Errorf("getRecord: %w: %s (collection %s)", ErrNotFound, capPDSErrorDetail(errorBody.Name), collection)
		}
		if response.StatusCode == http.StatusNotFound && errorBody.Name == "" {
			return nil, fmt.Errorf("getRecord: %w: bare 404 without an XRPC error from %s (collection %s)", ErrMalformedResponse, c.host, collection)
		}
		errorBody.Name = capPDSErrorDetail(errorBody.Name)
		errorBody.Message = capPDSErrorDetail(errorBody.Message)
		return nil, wrapAPIError(errorBody.APIError(response.StatusCode), "getRecord")
	}

	var result struct {
		URI   string         `json:"uri"`
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// Not wrapped: decoder errors can quote PDS-controlled text uncapped.
		return nil, fmt.Errorf("getRecord: %w: decoding response body: %s", ErrMalformedResponse, capPDSErrorDetail(err.Error()))
	}
	if result.Value == nil {
		return nil, fmt.Errorf("getRecord: %w: PDS returned success without a record value (collection %s)", ErrMalformedResponse, collection)
	}

	return &RecordResponse{
		URI:   result.URI,
		CID:   result.CID,
		Value: result.Value,
	}, nil
}

// capPDSErrorDetail truncates a text from a PDS error body to
// maxPDSErrorDetailBytes.
func capPDSErrorDetail(detail string) string {
	if len(detail) > maxPDSErrorDetailBytes {
		return detail[:maxPDSErrorDetailBytes]
	}
	return detail
}

// PutRecord creates or updates a record with optional optimistic locking.
func (c *client) PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (string, string, error) {
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
//
// The blob's real MIME type MUST be sent as the Content-Type. Indigo's
// generated RepoUploadBlob helper hardcodes "*/*" (the lexicon's accepted
// encoding), but a PDS enforcing the granular blob:*/* OAuth scope matches the
// granted accept patterns against the request's Content-Type and requires a
// concrete type — "*/*" never matches, so every upload through the generated
// helper fails with ScopeMissingError even when blob:*/* was granted.
// Empty or wildcard MIME types are therefore rejected locally with
// ErrBadRequest before any request is made.
func (c *client) UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	if mimeType == "" || strings.Contains(mimeType, "*") {
		return nil, fmt.Errorf("uploadBlob: %w: mimeType must be a concrete MIME type such as \"image/png\" (the PDS blob scope check rejects empty or wildcard content types)", ErrBadRequest)
	}

	var result comatproto.RepoUploadBlob_Output
	err := c.apiClient.LexDo(ctx, lexutil.Procedure, mimeType, "com.atproto.repo.uploadBlob", nil, bytes.NewReader(data), &result)
	if err != nil {
		return nil, wrapAPIError(err, "uploadBlob")
	}

	if result.Blob == nil || !result.Blob.Ref.Defined() {
		return nil, fmt.Errorf("uploadBlob: PDS returned success without a valid blob ref")
	}

	return &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": result.Blob.Ref.String()},
		MimeType: result.Blob.MimeType,
		Size:     int(result.Blob.Size),
	}, nil
}
