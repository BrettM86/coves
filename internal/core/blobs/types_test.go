package blobs

import "testing"

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
