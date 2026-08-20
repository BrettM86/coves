package imageproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
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

	// allowPrivateHosts disables the SSRF guard that refuses private, loopback
	// and link-local addresses on the PDS fetch. NEVER set in production: the
	// address this fetcher dials is the serviceEndpoint of a DID document,
	// which anyone can mint for free, and the route in front of it carries no
	// credential at all — so the destination is chosen by a stranger, and the
	// AppView shares a network with its Postgres, its PDS, its Jetstream and a
	// cloud metadata endpoint.
	//
	// It is construction state rather than an environment read inside Fetch,
	// for the reason blobs.blobService.allowPrivateHosts documents: every honest
	// test of this fetch serves its PDS from httptest, which listens on loopback, and
	// Go's testing package refuses t.Setenv alongside t.Parallel — so an env
	// read would make the guarded branch untestable in parallel and force the
	// whole package serial.
	allowPrivateHosts bool
}

// DefaultMaxSourceSizeMB is the default maximum source image size if not configured.
const DefaultMaxSourceSizeMB = 10

// PDSFetcherOption configures optional PDSFetcher behaviour.
type PDSFetcherOption func(*PDSFetcher)

// WithPrivateHostsAllowed disables the SSRF address guard on the PDS fetch.
//
// THE NAME IS THE CONTRACT: production must not call this. cmd/server derives
// the value from config once (the IS_DEV_ENV gate); tests that serve their
// fixtures from httptest pass it because loopback is exactly what the guard
// refuses, and a local dev stack runs its PDS on the developer's own machine.
func WithPrivateHostsAllowed() PDSFetcherOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(f *PDSFetcher) { f.allowPrivateHosts = true }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to NewPDSFetcher: the hatch when it is set, and NOTHING
// when it is not.
//
// It mirrors oauth.PrivateAddressOptions, and it is a function rather than an
// `if` in cmd/server/wiring.go for the reason documented there: `.env.ci:140`
// sets IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at every call
// site holding such a boolean. A unit test against this function is the only
// place in the repository where the branch production actually runs is ever
// evaluated. Do not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's
// own defaults.
func PrivateHostOptions(allowPrivate bool) []PDSFetcherOption {
	if !allowPrivate {
		return nil
	}
	return []PDSFetcherOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// NewPDSFetcher creates a new PDSFetcher with the specified timeout.
// maxSizeMB specifies the maximum allowed image size in megabytes (0 uses default of 10MB).
func NewPDSFetcher(timeout time.Duration, maxSizeMB int, opts ...PDSFetcherOption) *PDSFetcher {
	if maxSizeMB <= 0 {
		maxSizeMB = DefaultMaxSourceSizeMB
	}
	f := &PDSFetcher{
		timeout:      timeout,
		maxSizeBytes: int64(maxSizeMB) * 1024 * 1024,
	}
	for _, opt := range opts {
		opt(f)
	}

	// The SSRF-safe transport of internal/atproto/oauth: it resolves the host,
	// refuses private, loopback and link-local addresses, and then dials only
	// the address it vetted — closing the check-then-dial window a naive guard
	// leaves open.
	//
	// THE BYTE CAP IS RAISED FROM THIS FETCHER'S OWN LIMIT rather than left at
	// the transport's 32 MiB default, because IMAGE_PROXY_MAX_SOURCE_SIZE_MB is
	// operator-configurable: an operator who set it to 64 would otherwise be
	// silently clamped by a constant in another package, and would see it as an
	// image that will not load and an error blaming the remote host. A config
	// value that quietly does not take effect is worse than one that is
	// rejected. oauth.DefaultMaxResponseBytes documents this call site by name.
	//
	// ONE BYTE ABOVE, deliberately. Fetch reads maxSizeBytes+1 through an
	// io.LimitReader so that an oversized body is DETECTED rather than
	// truncated; a transport cap set to exactly maxSizeBytes would fail that
	// probing read and turn every ErrImageTooLarge into a generic fetch
	// failure.
	clientOpts := append(
		covesoauth.PrivateAddressOptions(f.allowPrivateHosts),
		covesoauth.WithMaxResponseBytes(f.maxSizeBytes+1),
	)
	f.client = covesoauth.NewSSRFSafeHTTPClient(clientOpts...)

	// The shared client ships a 15s ceiling of its own, and this one is
	// operator-configured (IMAGE_PROXY_FETCH_TIMEOUT_SECONDS, default 30). It
	// is restored the way blobs.NewBlobService restores its own: silently
	// re-timing every image fetch would be a second change wearing an SSRF
	// fix's clothes.
	f.client.Timeout = timeout
	return f
}

// Fetch retrieves a blob from the specified PDS using the com.atproto.sync.getBlob endpoint.
// Returns:
//   - ErrPDSBlocked if the SSRF guard refuses the endpoint's shape or its address
//   - ErrPDSNotFound if the blob does not exist (404 response)
//   - ErrPDSTimeout if the request times out or context is cancelled
//   - ErrPDSFetchFailed for any other error
func (f *PDSFetcher) Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error) {
	// Construct the request URL
	endpoint, err := parsePDSEndpoint(pdsURL)
	if err != nil {
		return nil, err
	}

	// RawPath goes with Path. url.URL keeps the escaped spelling separately and
	// EscapedPath() prefers it when it is a valid encoding of Path — it is not,
	// once Path has been replaced, so this is belt-and-braces rather than a
	// live hole. It costs one line and removes the need to re-derive that.
	endpoint.Path = "/xrpc/com.atproto.sync.getBlob"
	endpoint.RawPath = ""

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
		// THE GUARD'S REFUSAL IS CLASSIFIED FIRST AND ON ITS OWN IDENTITY.
		// Falling through to the branches below would map a refused internal
		// address onto ErrPDSTimeout (504) or ErrPDSFetchFailed (502)
		// depending on how the dial failed — which is the port-scan oracle
		// this guard exists to remove, rebuilt one layer up.
		if errors.Is(err, covesoauth.ErrBlockedAddress) {
			return nil, fmt.Errorf("%w: %v", ErrPDSBlocked, err)
		}

		// The transport refuses an ANNOUNCED Content-Length over its cap
		// before the body is read, so this is the same condition the
		// Content-Length branch below reports — just detected one layer down,
		// for a declared length above maxSizeBytes+1. Classifying it the same
		// way keeps a 2 MB declaration and a 1 MB+1 declaration from arriving
		// as two different errors.
		if errors.Is(err, covesoauth.ErrResponseTooLarge) {
			return nil, fmt.Errorf("%w: %v", ErrImageTooLarge, err)
		}

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

// parsePDSEndpoint parses a PDS URL and refuses every shape this fetcher will
// not dial, before a packet leaves the process.
//
// # WHY SHAPE IS CHECKED AT ALL, GIVEN THE ADDRESS GUARD
//
// Fetch overwrites .Path, .RawPath and .RawQuery and nothing else, so every
// OTHER component of the caller-supplied URL survives into the request — and the
// caller here is a DID document's serviceEndpoint, which a stranger mints. The
// address guard has no opinion on any of them:
//
//   - .User: `https://evil@internal-host` dials internal-host and carries
//     userinfo to it, which becomes an Authorization header on the wire.
//   - .Fragment: a component of the attacker's string survives a rewrite that
//     is supposed to replace the whole request target.
//   - .Opaque: url.URL.String() emits Opaque INSTEAD of Path, so the rewrite
//     above is discarded entirely and the attacker's own path is requested.
//   - .Scheme: file:// and gopher:// are refused today only by accident of what
//     http.Transport happens to register, which a future transport option can
//     quietly change. A positive allowlist is the control; the stdlib's
//     registration table is not.
//
// # REFUSED, NOT SANITISED
//
// Clearing these components would also close the holes, and it would do it
// silently: a serviceEndpoint carrying userinfo is not a URL with one field too
// many, it is evidence that the endpoint is not what this fetcher is for.
// Refusing says so, and says it in one place a reader can check, rather than
// leaving a reader to prove that the list of fields cleared is exhaustive.
func parsePDSEndpoint(pdsURL string) (*url.URL, error) {
	endpoint, err := url.Parse(pdsURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid PDS URL: %v", ErrPDSFetchFailed, err)
	}

	// url.Parse lower-cases the scheme, so this comparison is already
	// case-insensitive over the input.
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("%w: PDS URL scheme %q is not http or https", ErrPDSBlocked, endpoint.Scheme)
	}
	if endpoint.Opaque != "" {
		return nil, fmt.Errorf("%w: PDS URL is opaque and has no authority to vet", ErrPDSBlocked)
	}
	if endpoint.User != nil {
		return nil, fmt.Errorf("%w: PDS URL carries userinfo", ErrPDSBlocked)
	}
	if endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: PDS URL carries a fragment", ErrPDSBlocked)
	}
	if endpoint.Host == "" {
		return nil, fmt.Errorf("%w: PDS URL has no host", ErrPDSBlocked)
	}

	return endpoint, nil
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
