package blobs

import (
	"strings"
	"testing"
)

func TestHydrateBlobURL(t *testing.T) {
	tests := []struct {
		name     string
		pdsURL   string
		did      string
		cid      string
		expected string
	}{
		{
			name:     "valid inputs",
			pdsURL:   "https://pds.example.com",
			did:      "did:plc:abc123",
			cid:      "bafyreiabc123",
			expected: "https://pds.example.com/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aabc123&cid=bafyreiabc123",
		},
		{
			name:     "trailing slash on PDS URL removed",
			pdsURL:   "https://pds.example.com/",
			did:      "did:plc:abc123",
			cid:      "bafyreiabc123",
			expected: "https://pds.example.com/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aabc123&cid=bafyreiabc123",
		},
		{
			name:     "empty pdsURL returns empty",
			pdsURL:   "",
			did:      "did:plc:abc123",
			cid:      "bafyreiabc123",
			expected: "",
		},
		{
			name:     "empty did returns empty",
			pdsURL:   "https://pds.example.com",
			did:      "",
			cid:      "bafyreiabc123",
			expected: "",
		},
		{
			name:     "empty cid returns empty",
			pdsURL:   "https://pds.example.com",
			did:      "did:plc:abc123",
			cid:      "",
			expected: "",
		},
		{
			name:     "all empty returns empty",
			pdsURL:   "",
			did:      "",
			cid:      "",
			expected: "",
		},
		{
			name:     "special characters in DID are URL encoded",
			pdsURL:   "https://pds.example.com",
			did:      "did:web:example.com:user",
			cid:      "bafyreiabc123",
			expected: "https://pds.example.com/xrpc/com.atproto.sync.getBlob?did=did%3Aweb%3Aexample.com%3Auser&cid=bafyreiabc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HydrateBlobURL(tt.pdsURL, tt.did, tt.cid)
			if result != tt.expected {
				t.Errorf("HydrateBlobURL(%q, %q, %q) = %q, want %q",
					tt.pdsURL, tt.did, tt.cid, result, tt.expected)
			}
		})
	}
}

func TestHydrateImageProxyURL(t *testing.T) {
	tests := []struct {
		name         string
		proxyBaseURL string
		preset       string
		did          string
		cid          string
		expected     string
	}{
		{
			name:         "generates correct format",
			proxyBaseURL: "https://coves.social",
			preset:       "avatar",
			did:          "did:plc:abc123",
			cid:          "bafyreiabc123",
			expected:     "https://coves.social/img/avatar/plain/did:plc:abc123/bafyreiabc123",
		},
		{
			name:         "trailing slash on proxy URL removed",
			proxyBaseURL: "https://coves.social/",
			preset:       "thumb",
			did:          "did:plc:abc123",
			cid:          "bafyreiabc123",
			expected:     "https://coves.social/img/thumb/plain/did:plc:abc123/bafyreiabc123",
		},
		{
			name:         "empty proxyBaseURL generates relative URL",
			proxyBaseURL: "",
			preset:       "avatar",
			did:          "did:plc:abc123",
			cid:          "bafyreiabc123",
			expected:     "/img/avatar/plain/did:plc:abc123/bafyreiabc123",
		},
		{
			name:         "empty preset returns empty",
			proxyBaseURL: "https://coves.social",
			preset:       "",
			did:          "did:plc:abc123",
			cid:          "bafyreiabc123",
			expected:     "",
		},
		{
			name:         "empty did returns empty",
			proxyBaseURL: "https://coves.social",
			preset:       "avatar",
			did:          "",
			cid:          "bafyreiabc123",
			expected:     "",
		},
		{
			name:         "empty cid returns empty",
			proxyBaseURL: "https://coves.social",
			preset:       "avatar",
			did:          "did:plc:abc123",
			cid:          "",
			expected:     "",
		},
		{
			name:         "all empty returns empty",
			proxyBaseURL: "",
			preset:       "",
			did:          "",
			cid:          "",
			expected:     "",
		},
		{
			name:         "DID with colons preserved in path",
			proxyBaseURL: "https://coves.social",
			preset:       "avatar",
			did:          "did:web:example.com:user",
			cid:          "bafyreiabc123",
			// Colons are allowed in path segments per RFC 3986
			expected: "https://coves.social/img/avatar/plain/did:web:example.com:user/bafyreiabc123",
		},
		{
			name:         "forward slashes escaped in CID",
			proxyBaseURL: "https://coves.social",
			preset:       "avatar",
			did:          "did:plc:abc123",
			cid:          "bafyrei+special/chars",
			// Forward slashes must be escaped; plus signs allowed per RFC 3986
			expected: "https://coves.social/img/avatar/plain/did:plc:abc123/bafyrei+special%2Fchars",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HydrateImageProxyURL(tt.proxyBaseURL, tt.preset, tt.did, tt.cid)
			if result != tt.expected {
				t.Errorf("HydrateImageProxyURL(%q, %q, %q, %q) = %q, want %q",
					tt.proxyBaseURL, tt.preset, tt.did, tt.cid, result, tt.expected)
			}
		})
	}
}

func TestHydrateImageURL_ProxyDisabled(t *testing.T) {
	config := ImageURLConfig{
		ProxyEnabled: false,
		ProxyBaseURL: "https://coves.social",
	}
	pdsURL := "https://pds.example.com"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"
	preset := "avatar"

	result := HydrateImageURL(config, pdsURL, did, cid, preset)

	// Should return direct PDS URL when proxy is disabled
	expected := HydrateBlobURL(pdsURL, did, cid)
	if result != expected {
		t.Errorf("HydrateImageURL with proxy disabled = %q, want %q", result, expected)
	}
}

func TestHydrateImageURL_ProxyEnabled(t *testing.T) {
	config := ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://coves.social",
	}
	pdsURL := "https://pds.example.com"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"
	preset := "avatar"

	result := HydrateImageURL(config, pdsURL, did, cid, preset)

	// Should return proxy URL when proxy is enabled
	expected := HydrateImageProxyURL(config.ProxyBaseURL, preset, did, cid)
	if result != expected {
		t.Errorf("HydrateImageURL with proxy enabled = %q, want %q", result, expected)
	}
}

func TestHydrateImageURL_CDNOverride(t *testing.T) {
	config := ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://coves.social",
		CDNURL:       "https://cdn.coves.social",
	}
	pdsURL := "https://pds.example.com"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"
	preset := "avatar"

	result := HydrateImageURL(config, pdsURL, did, cid, preset)

	// Should use CDN URL instead of proxy base URL
	expected := HydrateImageProxyURL(config.CDNURL, preset, did, cid)
	if result != expected {
		t.Errorf("HydrateImageURL with CDN URL = %q, want %q", result, expected)
	}

	// Verify CDN URL is actually in the result
	if !strings.HasPrefix(result, "https://cdn.coves.social/") {
		t.Errorf("Expected CDN URL prefix, got %q", result)
	}
}

func TestHydrateImageURL_EmptyPresetUsesDirectURL(t *testing.T) {
	config := ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://coves.social",
	}
	pdsURL := "https://pds.example.com"
	did := "did:plc:abc123"
	cid := "bafyreiabc123"
	preset := "" // empty preset

	result := HydrateImageURL(config, pdsURL, did, cid, preset)

	// With empty preset, proxy URL will return empty, so fall back to direct URL
	// This tests the behavior when preset is not specified
	expected := HydrateBlobURL(pdsURL, did, cid)
	if result != expected {
		t.Errorf("HydrateImageURL with empty preset = %q, want %q", result, expected)
	}
}

func TestImageURLConfig(t *testing.T) {
	// Test that ImageURLConfig holds correct fields
	config := ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: "https://coves.social",
		CDNURL:       "https://cdn.coves.social",
	}

	if !config.ProxyEnabled {
		t.Error("ProxyEnabled should be true")
	}
	if config.ProxyBaseURL != "https://coves.social" {
		t.Errorf("ProxyBaseURL = %q, want %q", config.ProxyBaseURL, "https://coves.social")
	}
	if config.CDNURL != "https://cdn.coves.social" {
		t.Errorf("CDNURL = %q, want %q", config.CDNURL, "https://cdn.coves.social")
	}
}
