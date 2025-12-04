//go:build dev

package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DevHandleResolver resolves handles via local PDS for development
// This is needed because local handles (e.g., user.local.coves.dev) can't be
// resolved via standard DNS/HTTP well-known methods - they only exist on the local PDS.
type DevHandleResolver struct {
	pdsURL     string
	httpClient *http.Client
}

// NewDevHandleResolver creates a resolver that queries local PDS for handle resolution
func NewDevHandleResolver(pdsURL string, allowPrivateIPs bool) *DevHandleResolver {
	return &DevHandleResolver{
		pdsURL:     strings.TrimSuffix(pdsURL, "/"),
		httpClient: NewSSRFSafeHTTPClient(allowPrivateIPs),
	}
}

// ResolveHandle queries the local PDS to resolve a handle to a DID
// Returns the DID if successful, or empty string if not found
func (r *DevHandleResolver) ResolveHandle(ctx context.Context, handle string) (string, error) {
	if r.pdsURL == "" {
		return "", fmt.Errorf("PDS URL not configured")
	}

	// Build the resolve handle URL
	resolveURL := fmt.Sprintf("%s/xrpc/com.atproto.identity.resolveHandle?handle=%s",
		r.pdsURL, url.QueryEscape(handle))

	// Create request with context and timeout
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", resolveURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "Coves/1.0")

	// Execute request
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query PDS: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return "", nil // Handle not found
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PDS returned status %d", resp.StatusCode)
	}

	// Parse response
	var result struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	if result.DID == "" {
		return "", nil // No DID in response
	}

	slog.Debug("resolved handle via local PDS",
		"handle", handle,
		"did", result.DID,
		"pds_url", r.pdsURL)

	return result.DID, nil
}

// ResolveIdentifier attempts to resolve a handle to DID, or returns the DID if already provided
// This is the main entry point for the handlers
func (r *DevHandleResolver) ResolveIdentifier(ctx context.Context, identifier string) (string, error) {
	// If it's already a DID, return as-is
	if strings.HasPrefix(identifier, "did:") {
		return identifier, nil
	}

	// Try to resolve the handle via local PDS
	did, err := r.ResolveHandle(ctx, identifier)
	if err != nil {
		return "", fmt.Errorf("failed to resolve handle via PDS: %w", err)
	}
	if did == "" {
		return "", fmt.Errorf("handle not found on local PDS: %s", identifier)
	}

	return did, nil
}
