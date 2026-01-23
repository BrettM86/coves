package blobs

import (
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
