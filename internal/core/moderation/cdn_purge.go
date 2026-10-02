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
