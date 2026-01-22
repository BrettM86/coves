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
}

type blobService struct {
	pdsURL string
}

// NewBlobService creates a new blob service
func NewBlobService(pdsURL string) Service {
	return &blobService{
		pdsURL: pdsURL,
	}
}

// UploadBlobFromURL fetches an image from a URL and uploads it to PDS
// Flow:
// 1. Fetch image from URL with timeout
// 2. Validate size (max 6MB)
// 3. Validate MIME type (image/jpeg, image/png, image/webp)
// 4. Call UploadBlob to upload to PDS
func (s *blobService) UploadBlobFromURL(ctx context.Context, owner BlobOwner, imageURL string) (*BlobRef, error) {
	// Input validation
	if imageURL == "" {
		return nil, fmt.Errorf("image URL cannot be empty")
	}

	// Create HTTP client with timeout (30s to handle slow CDNs and large images)
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Fetch image from URL
	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for image URL: %w", err)
	}

	// Set User-Agent to avoid being blocked by CDNs that filter bot traffic
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CovesBot/1.0; +https://coves.social)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch image from URL: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Warning: failed to close image response body: %v", closeErr)
		}
	}()

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch image: HTTP %d", resp.StatusCode)
	}

	// Get MIME type from Content-Type header
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		return nil, fmt.Errorf("image URL response missing Content-Type header")
	}

	// Normalize MIME type (e.g., image/jpg → image/jpeg)
	mimeType = normalizeMimeType(mimeType)

	// Validate MIME type before reading data
	if !isValidMimeType(mimeType) {
		return nil, fmt.Errorf("unsupported MIME type: %s (allowed: image/jpeg, image/png, image/webp)", mimeType)
	}

	// Read image data
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	// Validate size (6MB = 6291456 bytes)
	const maxSize = 6291456
	if len(data) > maxSize {
		return nil, fmt.Errorf("image size %d bytes exceeds maximum of %d bytes (6MB)", len(data), maxSize)
	}

	// Upload to PDS
	return s.UploadBlob(ctx, owner, data, mimeType)
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
