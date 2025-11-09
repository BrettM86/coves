package posts

import (
	"fmt"
)

// TransformBlobRefsToURLs transforms all blob references in a PostView to PDS URLs
// This modifies the Embed field in-place, converting blob refs to direct URLs
// The transformation only affects external embeds with thumbnail blobs
func TransformBlobRefsToURLs(postView *PostView) {
	if postView == nil || postView.Embed == nil {
		return
	}

	// Get community PDS URL from post view
	if postView.Community == nil || postView.Community.PDSURL == "" {
		return // Cannot transform without PDS URL
	}

	communityDID := postView.Community.DID
	pdsURL := postView.Community.PDSURL

	// Check if embed is a map (should be for external embeds)
	embedMap, ok := postView.Embed.(map[string]interface{})
	if !ok {
		return
	}

	// Check embed type
	embedType, ok := embedMap["$type"].(string)
	if !ok {
		return
	}

	// Only transform external embeds
	if embedType == "social.coves.embed.external" {
		if external, ok := embedMap["external"].(map[string]interface{}); ok {
			transformThumbToURL(external, communityDID, pdsURL)
		}
	}
}

// transformThumbToURL converts a thumb blob ref to a PDS URL
// This modifies the external map in-place
func transformThumbToURL(external map[string]interface{}, communityDID, pdsURL string) {
	// Check if thumb exists
	thumb, ok := external["thumb"]
	if !ok {
		return
	}

	// If thumb is already a string (URL), don't transform
	if _, isString := thumb.(string); isString {
		return
	}

	// Try to parse as blob ref
	thumbMap, ok := thumb.(map[string]interface{})
	if !ok {
		return
	}

	// Extract CID from blob ref
	ref, ok := thumbMap["ref"].(map[string]interface{})
	if !ok {
		return
	}

	cid, ok := ref["$link"].(string)
	if !ok || cid == "" {
		return
	}

	// Transform to PDS blob endpoint URL
	// Format: {pds_url}/xrpc/com.atproto.sync.getBlob?did={community_did}&cid={cid}
	blobURL := fmt.Sprintf("%s/xrpc/com.atproto.sync.getBlob?did=%s&cid=%s",
		pdsURL, communityDID, cid)

	// Replace blob ref with URL string
	external["thumb"] = blobURL
}
