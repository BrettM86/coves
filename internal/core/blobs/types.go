package blobs

import (
	"log/slog"
	"net/url"
	"strings"
)

// BlobRef represents a blob reference for atproto records
type BlobRef struct {
	Type     string            `json:"$type"`
	Ref      map[string]string `json:"ref"`
	MimeType string            `json:"mimeType"`
	Size     int               `json:"size"`
}

// HydrateBlobURL converts a blob CID to a full PDS blob URL.
// Returns empty string if any required parameter is empty.
// Format: {pdsURL}/xrpc/com.atproto.sync.getBlob?did={did}&cid={cid}
func HydrateBlobURL(pdsURL, did, cid string) string {
	if pdsURL == "" || did == "" || cid == "" {
		return ""
	}
	return strings.TrimSuffix(pdsURL, "/") + "/xrpc/com.atproto.sync.getBlob?did=" +
		url.QueryEscape(did) + "&cid=" + url.QueryEscape(cid)
}

// HydrateImageProxyURL generates a URL for the image proxy with the specified preset.
// Format: {proxyBaseURL}/img/{preset}/plain/{did}/{cid}
// If proxyBaseURL is empty, generates a relative URL: /img/{preset}/plain/{did}/{cid}
// Returns empty string if preset, did, or cid are empty.
// DID and CID are URL-escaped for safety in path segments.
func HydrateImageProxyURL(proxyBaseURL, preset, did, cid string) string {
	if preset == "" || did == "" || cid == "" {
		return ""
	}
	return strings.TrimSuffix(proxyBaseURL, "/") + "/img/" + preset + "/plain/" +
		url.PathEscape(did) + "/" + url.PathEscape(cid)
}

// ImageURLConfig holds configuration for image URL generation.
type ImageURLConfig struct {
	ProxyEnabled bool   // Whether the image proxy is enabled
	ProxyBaseURL string // Base URL for the image proxy (e.g., "https://coves.social")
	CDNURL       string // Optional CDN override URL
}

// HydrateImageURL generates the appropriate image URL based on config.
// If proxy is disabled, returns direct PDS URL via HydrateBlobURL.
// If CDN URL is set and proxy is enabled, uses CDN instead of ProxyBaseURL.
// Returns empty string if the generated URL would be invalid.
func HydrateImageURL(config ImageURLConfig, pdsURL, did, cid, preset string) string {
	if !config.ProxyEnabled {
		return HydrateBlobURL(pdsURL, did, cid)
	}

	// Determine which base URL to use
	baseURL := config.ProxyBaseURL
	if config.CDNURL != "" {
		baseURL = config.CDNURL
	}

	// Generate proxy URL
	proxyURL := HydrateImageProxyURL(baseURL, preset, did, cid)

	// If proxy URL generation failed (e.g., empty preset or base URL), fall back to direct URL
	// Log this as it indicates a configuration problem when proxy is enabled
	if proxyURL == "" {
		slog.Warn("[IMAGE-PROXY] proxy URL generation failed, falling back to direct PDS URL",
			"proxy_enabled", config.ProxyEnabled,
			"proxy_base_url", config.ProxyBaseURL,
			"cdn_url", config.CDNURL,
			"preset", preset,
			"did", did,
			"cid", cid,
		)
		return HydrateBlobURL(pdsURL, did, cid)
	}

	return proxyURL
}
