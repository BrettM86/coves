package imageproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Fetcher defines the interface for fetching blobs from a PDS.
type Fetcher interface {
	// Fetch retrieves a blob from the specified PDS.
	// Returns the blob bytes or an error if the fetch fails.
	Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error)
}

// PDSFetcher implements the Fetcher interface for fetching blobs from atproto PDS servers.
type PDSFetcher struct {
	client       *http.Client
	timeout      time.Duration
	maxSizeBytes int64
}

// DefaultMaxSourceSizeMB is the default maximum source image size if not configured.
const DefaultMaxSourceSizeMB = 10

// NewPDSFetcher creates a new PDSFetcher with the specified timeout.
// maxSizeMB specifies the maximum allowed image size in megabytes (0 uses default of 10MB).
func NewPDSFetcher(timeout time.Duration, maxSizeMB int) *PDSFetcher {
	if maxSizeMB <= 0 {
		maxSizeMB = DefaultMaxSourceSizeMB
	}
	return &PDSFetcher{
		client: &http.Client{
			Timeout: timeout,
		},
		timeout:      timeout,
		maxSizeBytes: int64(maxSizeMB) * 1024 * 1024,
	}
}

// Fetch retrieves a blob from the specified PDS using the com.atproto.sync.getBlob endpoint.
// Returns:
//   - ErrPDSNotFound if the blob does not exist (404 response)
//   - ErrPDSTimeout if the request times out or context is cancelled
//   - ErrPDSFetchFailed for any other error
func (f *PDSFetcher) Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error) {
	// Construct the request URL
	endpoint, err := url.Parse(pdsURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid PDS URL: %v", ErrPDSFetchFailed, err)
	}
	endpoint.Path = "/xrpc/com.atproto.sync.getBlob"

	query := url.Values{}
	query.Set("did", did)
	query.Set("cid", cid)
	endpoint.RawQuery = query.Encode()

	// Create the request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to create request: %v", ErrPDSFetchFailed, err)
	}

	// Set User-Agent header for identification
	req.Header.Set("User-Agent", "Coves-ImageProxy/1.0")

	// Execute the request
	resp, err := f.client.Do(req)
	if err != nil {
		// Check if the error is due to context cancellation or timeout
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrPDSTimeout, ctx.Err())
		}
		// Check if it's a timeout error from the http client
		if isTimeoutError(err) {
			return nil, fmt.Errorf("%w: request timed out", ErrPDSTimeout)
		}
		return nil, fmt.Errorf("%w: %v", ErrPDSFetchFailed, err)
	}
	defer resp.Body.Close()

	// Handle response status codes
	switch resp.StatusCode {
	case http.StatusOK:
		// Check Content-Length header if available
		if resp.ContentLength > 0 && resp.ContentLength > f.maxSizeBytes {
			return nil, fmt.Errorf("%w: content length %d exceeds maximum %d bytes",
				ErrImageTooLarge, resp.ContentLength, f.maxSizeBytes)
		}

		// Use a limited reader to prevent memory exhaustion even if Content-Length is missing or wrong.
		// We read maxSizeBytes + 1 to detect if the response exceeds the limit.
		limitedReader := io.LimitReader(resp.Body, f.maxSizeBytes+1)
		data, err := io.ReadAll(limitedReader)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to read response body: %v", ErrPDSFetchFailed, err)
		}

		// Check if we hit the limit (meaning there was more data)
		if int64(len(data)) > f.maxSizeBytes {
			return nil, fmt.Errorf("%w: response body exceeds maximum %d bytes",
				ErrImageTooLarge, f.maxSizeBytes)
		}

		return data, nil

	case http.StatusNotFound:
		return nil, ErrPDSNotFound

	case http.StatusBadRequest:
		// AT Protocol PDS may return 400 with "Blob not found" for missing blobs
		// We need to check the error message to distinguish from actual bad requests
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if readErr == nil && isBlobNotFoundError(body) {
			return nil, ErrPDSNotFound
		}
		return nil, fmt.Errorf("%w: bad request (status 400)", ErrPDSFetchFailed)

	default:
		return nil, fmt.Errorf("%w: unexpected status code %d", ErrPDSFetchFailed, resp.StatusCode)
	}
}

// pdsErrorResponse represents the error response structure from AT Protocol PDS
type pdsErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// isBlobNotFoundError checks if the error response indicates a blob was not found.
// AT Protocol PDS returns 400 with {"error":"InvalidRequest","message":"Blob not found"}
// for missing blobs instead of a proper 404.
func isBlobNotFoundError(body []byte) bool {
	var errResp pdsErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		return false
	}
	// Check for "Blob not found" message (case-insensitive)
	return strings.Contains(strings.ToLower(errResp.Message), "blob not found")
}

// isTimeoutError checks if the error is a timeout-related error.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	// Check for timeout interface
	if te, ok := err.(interface{ Timeout() bool }); ok {
		return te.Timeout()
	}
	return false
}
