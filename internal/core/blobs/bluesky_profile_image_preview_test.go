package blobs

import (
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBlueskyProfileImagePreviewSource(t *testing.T) {
	tests := []struct {
		raw       string
		want      BlueskyProfileImagePreviewSource
		wantError bool
	}{
		{raw: "", want: BlueskyProfileImagePreviewSourceBlueskyCDN},
		{raw: "bluesky_cdn", want: BlueskyProfileImagePreviewSourceBlueskyCDN},
		{raw: "pds", want: BlueskyProfileImagePreviewSourcePDS},
		{raw: "PDS", wantError: true},
		{raw: "proxy", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseBlueskyProfileImagePreviewSource(tt.raw)
			if tt.wantError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "bluesky_cdn")
				assert.Contains(t, err.Error(), "pds")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBlueskyProfileImagePreviewSource_URL(t *testing.T) {
	const (
		did = "did:plc:abc123"
		cid = "bafkreidhqb6afzkiias6nwij3ckl6cv2zdbyes7hhljww56uuusre4jaiq"
	)
	localPDS := testkit.Endpoints().PDS.BaseURL
	tests := []struct {
		name    string
		source  BlueskyProfileImagePreviewSource
		pdsHost string
		did     string
		preset  string
		want    string
	}{
		{
			name:   "bluesky cdn avatar",
			source: BlueskyProfileImagePreviewSourceBlueskyCDN,
			did:    did, preset: "avatar",
			pdsHost: "https://pds.example",
			want:    "https://cdn.bsky.app/img/avatar/plain/did:plc:abc123/" + cid + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
		},
		{
			name:   "bluesky cdn banner",
			source: BlueskyProfileImagePreviewSourceBlueskyCDN,
			did:    did, preset: "banner",
			pdsHost: "https://pds.example",
			want:    "https://cdn.bsky.app/img/banner/plain/did:plc:abc123/" + cid + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
		},
		{
			name:   "unset source is the bluesky cdn",
			source: "",
			did:    did, preset: "avatar",
			pdsHost: "https://pds.example",
			want:    "https://cdn.bsky.app/img/avatar/plain/did:plc:abc123/" + cid + "@jpeg", // coves:allow-public-host: expected output of the production Bluesky CDN URL builder, matched as a string and never dialled
		},
		{
			name:   "pds getBlob",
			source: BlueskyProfileImagePreviewSourcePDS,
			did:    did, preset: "avatar",
			pdsHost: localPDS,
			want:    localPDS + "/xrpc/com.atproto.sync.getBlob?cid=" + cid + "&did=did%3Aplc%3Aabc123",
		},
		{
			name:   "pds getBlob trims a trailing slash on the host",
			source: BlueskyProfileImagePreviewSourcePDS,
			did:    did, preset: "banner",
			pdsHost: "https://pds.example/",
			want:    "https://pds.example/xrpc/com.atproto.sync.getBlob?cid=" + cid + "&did=did%3Aplc%3Aabc123",
		},
		{
			name:   "pds getBlob query-escapes the did",
			source: BlueskyProfileImagePreviewSourcePDS,
			did:    "did:web:a.example&cid=other", preset: "avatar",
			pdsHost: "https://pds.example",
			want:    "https://pds.example/xrpc/com.atproto.sync.getBlob?cid=" + cid + "&did=did%3Aweb%3Aa.example%26cid%3Dother",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.source.URL(tt.pdsHost, tt.did, cid, tt.preset))
		})
	}
}
