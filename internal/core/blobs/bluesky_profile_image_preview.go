package blobs

import (
	"fmt"
	"net/url"
	"strings"
)

// BlueskyProfileImagePreviewSource selects where the preview URL of a Bluesky
// profile image offered for import points. The Coves image proxy cannot serve
// these blobs: it serves only blobs a Coves record references, and the Bluesky
// profile is not indexed. Set by BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE.
type BlueskyProfileImagePreviewSource string

const (
	// BlueskyProfileImagePreviewSourceBlueskyCDN points previews at Bluesky's
	// public image CDN. It is the default, and the zero value behaves the same.
	BlueskyProfileImagePreviewSourceBlueskyCDN BlueskyProfileImagePreviewSource = "bluesky_cdn"

	// BlueskyProfileImagePreviewSourcePDS points previews at the user's own
	// PDS com.atproto.sync.getBlob endpoint, for local and CI stacks whose
	// accounts the Bluesky CDN cannot see.
	BlueskyProfileImagePreviewSourcePDS BlueskyProfileImagePreviewSource = "pds"
)

// blueskyCDNImageBaseURL is Bluesky's public image CDN.
const blueskyCDNImageBaseURL = "https://cdn.bsky.app/img"

// ParseBlueskyProfileImagePreviewSource parses a configured preview source.
// An empty value is the Bluesky CDN; any value other than "bluesky_cdn" or
// "pds" is an error.
func ParseBlueskyProfileImagePreviewSource(raw string) (BlueskyProfileImagePreviewSource, error) {
	switch source := BlueskyProfileImagePreviewSource(raw); source {
	case "", BlueskyProfileImagePreviewSourceBlueskyCDN:
		return BlueskyProfileImagePreviewSourceBlueskyCDN, nil
	case BlueskyProfileImagePreviewSourcePDS:
		return source, nil
	default:
		return "", fmt.Errorf("unknown Bluesky profile image preview source %q: must be %q or %q",
			raw, BlueskyProfileImagePreviewSourceBlueskyCDN, BlueskyProfileImagePreviewSourcePDS)
	}
}

// URL returns the preview URL of the blob cid owned by did. preset is the
// Bluesky CDN preset, "avatar" or "banner"; pdsHost is the user's PDS origin.
// The zero source is the Bluesky CDN.
func (s BlueskyProfileImagePreviewSource) URL(pdsHost, did, cid, preset string) string {
	if s == BlueskyProfileImagePreviewSourcePDS {
		query := url.Values{"did": {did}, "cid": {cid}}
		return strings.TrimRight(pdsHost, "/") + "/xrpc/com.atproto.sync.getBlob?" + query.Encode()
	}
	return blueskyCDNImageBaseURL + "/" + url.PathEscape(preset) + "/plain/" +
		url.PathEscape(did) + "/" + url.PathEscape(cid) + "@jpeg"
}
