package blobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
)

// BlobOwner represents any entity that can own blobs on a PDS.
// This interface breaks the import cycle between blobs and communities packages.
// communities.Community implements this interface.
type BlobOwner interface {
	// GetPDSURL returns the PDS URL for this entity
	GetPDSURL() string
	// GetPDSAccessToken returns the access token for authenticating with the PDS
	GetPDSAccessToken() string
}

// Service defines the interface for blob operations
type Service interface {
	// UploadBlobFromURL fetches an image from a URL and uploads it to the owner's PDS
	UploadBlobFromURL(ctx context.Context, owner BlobOwner, imageURL string) (*BlobRef, error)

	// UploadBlob uploads binary data to the owner's PDS
	UploadBlob(ctx context.Context, owner BlobOwner, data []byte, mimeType string) (*BlobRef, error)

	// FetchImageForURL is UploadBlobFromURL's FIRST half on its own: fetch the
	// remote image under a timeout, refuse a Content-Type outside the image
	// allowlist, and cap the body at 6MB. It touches no PDS.
	//
	// IT IS SPLIT OUT BECAUSE THE UPLOADER CHANGED, NOT THE GUARD. A post's
	// media now goes into the AUTHOR's repository, under the author's own OAuth
	// session — which is DPoP-signed, so it cannot travel through this
	// package's BlobOwner (a bearer token and a URL). The caller therefore does
	// the upload itself, through the author's PDS client, and this is what it
	// calls first so that the choke point is reached by both paths rather than
	// reimplemented beside one of them.
	//
	// The guard is the reason the split is a split and not a copy. The URL
	// being fetched is attacker-influenced twice over — a client picks the page
	// that gets unfurled, and the page picks the thumbnail — so a second
	// implementation that drifted would turn a link preview into an unbounded
	// fetch performed by the AppView with a user's credentials into that user's
	// own storage quota.
	FetchImageForURL(ctx context.Context, imageURL string) (data []byte, mimeType string, err error)
}

type blobService struct {
	pdsURL string

	// allowPrivateHosts disables the SSRF guard that refuses private, loopback
	// and link-local addresses on a remote image fetch. NEVER set in production:
	// the URL being fetched is chosen by whoever controls the page being
	// unfurled, and the AppView shares a network with its database, its PDS and
	// a cloud metadata endpoint.
	//
	// It is construction state rather than an environment read inside the fetch,
	// for the reason blueskypost's blueskyAPI.allowPrivateHost documents: every
	// honest test of a remote fetch serves it from httptest, which listens on
	// loopback, and Go's testing package refuses t.Setenv alongside t.Parallel —
	// so an env read would make the guarded branch untestable in parallel and
	// force the whole package serial.
	allowPrivateHosts bool

	// fetchClient is the SSRF-guarded client every remote image fetch goes
	// through. Built ONCE at construction, because the guard's whole property is
	// that it resolves the host and dials only the address it vetted — a client
	// assembled per call would be the same code with a per-call chance of being
	// assembled wrongly.
	fetchClient *http.Client
}

// BlobServiceOption configures optional blob service behaviour.
type BlobServiceOption func(*blobService)

// WithPrivateHostsAllowed disables the remote-fetch SSRF guard.
//
// THE NAME IS THE CONTRACT: production must not call this. cmd/server derives
// the value from config once (the IS_DEV_ENV gate); tests that serve their
// fixtures from httptest pass it because loopback is exactly what the guard
// refuses.
func WithPrivateHostsAllowed() BlobServiceOption {
	return func(s *blobService) { s.allowPrivateHosts = true }
}

// NewBlobService creates a new blob service
func NewBlobService(pdsURL string, opts ...BlobServiceOption) Service {
	s := &blobService{
		pdsURL: pdsURL,
	}
	for _, opt := range opts {
		opt(s)
	}

	// The SSRF-safe transport of internal/atproto/oauth, which blueskypost's
	// attacker-influenced fetch already uses: it resolves the host, refuses
	// private, loopback and link-local addresses, and then dials only the
	// address it vetted — closing the check-then-dial window a naive guard
	// leaves open.
	//
	// Its own 15s ceiling is raised back to the 30s this fetch has always
	// allowed: thumbnails come from CDNs that are slow rather than hostile, and
	// tightening an unrelated timeout while fixing an SSRF hole would be a
	// second change wearing the first one's clothes.
	s.fetchClient = covesoauth.NewSSRFSafeHTTPClient(s.allowPrivateHosts)
	s.fetchClient.Timeout = 30 * time.Second
	return s
}

// UploadBlobFromURL fetches an image from a URL and uploads it to PDS
// Flow:
// 1. Fetch image from URL with timeout
// 2. Validate size (max 6MB)
// 3. Validate MIME type (image/jpeg, image/png, image/webp)
// 4. Call UploadBlob to upload to PDS
func (s *blobService) UploadBlobFromURL(ctx context.Context, owner BlobOwner, imageURL string) (*BlobRef, error) {
	data, mimeType, err := s.FetchImageForURL(ctx, imageURL)
	if err != nil {
		return nil, err
	}

	// Upload to PDS
	return s.UploadBlob(ctx, owner, data, mimeType)
}

// FetchImageForURL fetches and validates a remote image without uploading it.
// See Service.FetchImageForURL for why this half stands on its own.
func (s *blobService) FetchImageForURL(ctx context.Context, imageURL string) ([]byte, string, error) {
	// Input validation
	if imageURL == "" {
		return nil, "", fmt.Errorf("image URL cannot be empty")
	}

	// Fetch image from URL through the SSRF-guarded client (see NewBlobService).
	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request for image URL: %w", err)
	}

	// Set User-Agent to avoid being blocked by CDNs that filter bot traffic
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CovesBot/1.0; +https://coves.social)")

	resp, err := s.fetchClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch image from URL: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Warning: failed to close image response body: %v", closeErr)
		}
	}()

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("failed to fetch image: HTTP %d", resp.StatusCode)
	}

	// Get MIME type from Content-Type header
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		return nil, "", fmt.Errorf("image URL response missing Content-Type header")
	}

	// Normalize MIME type (e.g., image/jpg → image/jpeg)
	mimeType = normalizeMimeType(mimeType)

	// Validate MIME type before reading data
	if !isValidMimeType(mimeType) {
		return nil, "", fmt.Errorf("unsupported MIME type: %s (allowed: image/jpeg, image/png, image/webp)", mimeType)
	}

	// READ THE CAP, do not measure it afterwards.
	//
	// `len(data) > maxSize` after an io.ReadAll is a correct test of a slice
	// that is already in memory, which is the same as having no cap at all for
	// the failure a cap exists to prevent: an origin advertising image/png and
	// streaming without a Content-Length gets the whole body buffered, and the
	// limit is only where the result is thrown away. A LimitReader of max+1 lets
	// at most one byte past the cap enter memory, and that byte is what
	// separates "exactly at the limit" from "over it".
	const maxSize = 6291456 // 6MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image data: %w", err)
	}
	if len(data) > maxSize {
		return nil, "", fmt.Errorf("image size exceeds maximum of %d bytes (6MB)", maxSize)
	}

	return data, mimeType, nil
}

// UploadBlob uploads binary data to the owner's PDS
// Flow:
// 1. Validate inputs
// 2. POST to {PDSURL}/xrpc/com.atproto.repo.uploadBlob
// 3. Use owner's PDSAccessToken for auth
// 4. Set Content-Type header to mimeType
// 5. Parse response and extract blob reference
func (s *blobService) UploadBlob(ctx context.Context, owner BlobOwner, data []byte, mimeType string) (*BlobRef, error) {
	// Input validation
	if owner == nil {
		return nil, fmt.Errorf("owner cannot be nil")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("data cannot be empty")
	}
	if mimeType == "" {
		return nil, fmt.Errorf("mimeType cannot be empty")
	}

	// Validate MIME type
	if !isValidMimeType(mimeType) {
		return nil, fmt.Errorf("unsupported MIME type: %s (allowed: image/jpeg, image/png, image/webp)", mimeType)
	}

	// Validate size (6MB = 6291456 bytes)
	const maxSize = 6291456
	if len(data) > maxSize {
		return nil, fmt.Errorf("data size %d bytes exceeds maximum of %d bytes (6MB)", len(data), maxSize)
	}

	// Use owner's PDS URL (for federated communities)
	pdsURL := owner.GetPDSURL()
	if pdsURL == "" {
		return nil, fmt.Errorf("owner has no PDS URL configured")
	}

	// Validate access token before making request
	accessToken := owner.GetPDSAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("owner has no PDS access token")
	}

	// Build PDS endpoint URL
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.uploadBlob", pdsURL)

	// Create HTTP request with blob data
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create PDS request: %w", err)
	}

	// Set headers (auth + content type)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PDS request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Warning: failed to close PDS response body: %v", closeErr)
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read PDS response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		// Sanitize error body for logging (prevent sensitive data leakage)
		bodyPreview := string(body)
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200] + "... (truncated)"
		}
		log.Printf("[BLOB-UPLOAD-ERROR] PDS Status: %d, Body: %s", resp.StatusCode, bodyPreview)

		// Return truncated error (defense in depth - handler will mask this further)
		return nil, fmt.Errorf("PDS returned error %d: %s", resp.StatusCode, bodyPreview)
	}

	// Parse response
	// The response from com.atproto.repo.uploadBlob is a BlobRef object
	var result struct {
		Blob BlobRef `json:"blob"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse PDS response: %w", err)
	}

	// Validate required fields in PDS response
	if result.Blob.Type == "" {
		return nil, fmt.Errorf("PDS response missing required field: $type")
	}
	if result.Blob.Ref == nil || result.Blob.Ref["$link"] == "" {
		return nil, fmt.Errorf("PDS response missing required field: ref.$link (CID)")
	}
	if result.Blob.MimeType == "" {
		return nil, fmt.Errorf("PDS response missing required field: mimeType")
	}
	if result.Blob.Size == 0 {
		return nil, fmt.Errorf("PDS response missing required field: size")
	}

	return &result.Blob, nil
}

// normalizeMimeType converts non-standard MIME types to their standard equivalents
// Common case: Many CDNs return image/jpg instead of the standard image/jpeg
func normalizeMimeType(mimeType string) string {
	switch mimeType {
	case "image/jpg":
		return "image/jpeg"
	default:
		return mimeType
	}
}

// isValidMimeType checks if the MIME type is allowed for blob uploads
func isValidMimeType(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}
