package moderation

import (
	"context"
	"log/slog"

	"Coves/internal/core/imageproxy"
)

// CDNPurger invalidates CDN URLs for owner-scoped media blocks.
type CDNPurger interface {
	PurgeBlobs(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult
}

// MediaReconcilerOption configures optional post-commit media actions.
type MediaReconcilerOption func(*MediaReconciler)

// WithCDNPurger enables CDN invalidation after media reconciliation commits.
func WithCDNPurger(purger CDNPurger) MediaReconcilerOption {
	return func(reconciler *MediaReconciler) { reconciler.cdnPurger = purger }
}

// purgeCDNMediaBlocks invalidates only the URLs belonging to owner-scoped
// blocks. CDN failures are best effort; the committed block already prevents
// serving the blob at the origin.
func purgeCDNMediaBlocks(ctx context.Context, purger CDNPurger, blocks []MediaBlock) {
	if purger == nil {
		return
	}
	var owned []imageproxy.BlockedBlob
	for _, block := range blocks {
		if block.OwnerDID != "" {
			owned = append(owned, imageproxy.BlockedBlob{OwnerDID: block.OwnerDID, CID: block.BlobCID})
		}
	}
	if len(owned) == 0 {
		return
	}
	for _, failure := range purger.PurgeBlobs(ctx, owned).Failed {
		slog.Error("moderation CDN purge failed", "did", failure.Blob.OwnerDID, "cid", failure.Blob.CID, "code", failure.Code)
	}
}
