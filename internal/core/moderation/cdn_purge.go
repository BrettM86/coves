package moderation

import (
	"context"

	"Coves/internal/core/imageproxy"
)

// CDNPurger invalidates CDN URLs for owner-scoped media blocks.
type CDNPurger interface {
	PurgeBlobs(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult
}

// MediaReconcilerOption configures optional post-commit media actions.
type MediaReconcilerOption func(*MediaReconciler)

// ownerCDNPurgeBlobs excludes every-owner blocks: only pairs with an origin
// repository can be turned into CDN image URLs.
func ownerCDNPurgeBlobs(blocks []MediaBlock) []imageproxy.BlockedBlob {
	var owned []imageproxy.BlockedBlob
	for _, block := range blocks {
		if block.OwnerDID != "" {
			owned = append(owned, imageproxy.BlockedBlob{OwnerDID: block.OwnerDID, CID: block.BlobCID})
		}
	}
	return owned
}

// everyOwnerBlockCIDs returns the CID of each every-owner block, once.
func everyOwnerBlockCIDs(blocks []MediaBlock) []string {
	var cids []string
	seen := make(map[string]bool)
	for _, block := range blocks {
		if block.OwnerDID == "" && !seen[block.BlobCID] {
			seen[block.BlobCID] = true
			cids = append(cids, block.BlobCID)
		}
	}
	return cids
}

// appendMissingCDNPurgeBlobs appends each of additional not already in blobs.
func appendMissingCDNPurgeBlobs(blobs, additional []imageproxy.BlockedBlob) []imageproxy.BlockedBlob {
	present := make(map[imageproxy.BlockedBlob]bool, len(blobs))
	for _, blob := range blobs {
		present[blob] = true
	}
	for _, blob := range additional {
		if !present[blob] {
			present[blob] = true
			blobs = append(blobs, blob)
		}
	}
	return blobs
}
