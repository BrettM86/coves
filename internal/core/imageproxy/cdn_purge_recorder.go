package imageproxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// CDNPurgeRecorder durably commits CDN purge targets before local cache
// entries are deleted.
type CDNPurgeRecorder interface {
	// RecordCDNPurgeTargets records each target pending, moving a completed
	// target back to pending: cached bytes are evidence of a fresh copy.
	RecordCDNPurgeTargets(ctx context.Context, blobs []BlockedBlob) error
	// RecordMissingCDNPurgeTargets records only the targets not yet recorded
	// and leaves every recorded target, pending or completed, untouched.
	RecordMissingCDNPurgeTargets(ctx context.Context, blobs []BlockedBlob) error
}

// ServiceOption configures an ImageProxyService.
type ServiceOption func(*ImageProxyService)

// WithCDNPurgeRecorder records purge targets before blocked cache bytes are removed.
func WithCDNPurgeRecorder(recorder CDNPurgeRecorder) ServiceOption {
	return func(service *ImageProxyService) { service.cdnPurgeRecorder = recorder }
}

// BlobOwnerIndex lists every owner the blob reference index holds for a CID.
type BlobOwnerIndex interface {
	ListBlobOwners(ctx context.Context, cid string) ([]string, error)
}

// WithBlobOwnerIndex includes indexed owners when recording every-owner CDN purges.
func WithBlobOwnerIndex(index BlobOwnerIndex) ServiceOption {
	return func(service *ImageProxyService) { service.blobOwnerIndex = index }
}

// ownerDIDFromCacheDirectory reverses only unambiguous cache directory names.
func ownerDIDFromCacheDirectory(name string) (string, bool) {
	var owner string
	switch {
	case strings.HasPrefix(name, "did_plc_"):
		owner = "did:plc:" + strings.TrimPrefix(name, "did_plc_")
	case strings.HasPrefix(name, "did_web_"):
		host := strings.TrimPrefix(name, "did_web_")
		if strings.Contains(host, "_") {
			return "", false
		}
		owner = "did:web:" + host
	default:
		return "", false
	}
	if ValidateOwnerDID(owner) != nil || makeDIDSafe(owner) != name {
		return "", false
	}
	return owner, true
}

// recordCachedOwners names every indexed and recoverable cached owner before an
// ownerless purge. A cached owner is recorded pending even when its target
// completed, because its cached bytes are evidence of a fresh copy. An owner
// only the index names is recorded only if it has no target yet: the
// every-owner block's activation re-pended the owners already recorded, so a
// repeated sweep does not purge them again. An indexed owner that is not a
// canonical owner DID is skipped, since the index holds whatever DID a record
// carried.
// The caller has already awaited publications for cid.
func (s *ImageProxyService) recordCachedOwners(ctx context.Context, cid string) error {
	if cid == "" {
		return ErrEmptyParameter
	}
	directories, err := s.cache.OwnerDirectories(cid)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	var targets []BlockedBlob
	for _, directory := range directories {
		owner, mapped := ownerDIDFromCacheDirectory(directory)
		if !mapped {
			slog.Warn("[IMAGE-PROXY] cannot name cached owner for CDN purge", "directory", directory, "cid", cid)
			continue
		}
		if seen[owner] {
			continue
		}
		seen[owner] = true
		targets = append(targets, BlockedBlob{OwnerDID: owner, CID: cid})
	}
	var indexTargets []BlockedBlob
	if s.blobOwnerIndex != nil {
		owners, err := s.blobOwnerIndex.ListBlobOwners(ctx, cid)
		if err != nil {
			return fmt.Errorf("listing blob owners for CDN purge: %w", err)
		}
		for _, owner := range owners {
			if seen[owner] {
				continue
			}
			seen[owner] = true
			if ValidateOwnerDID(owner) != nil {
				slog.Warn("[IMAGE-PROXY] cannot name indexed owner for CDN purge", "owner", owner, "cid", cid)
				continue
			}
			indexTargets = append(indexTargets, BlockedBlob{OwnerDID: owner, CID: cid})
		}
	}
	if len(targets) > 0 {
		if err := s.cdnPurgeRecorder.RecordCDNPurgeTargets(ctx, targets); err != nil {
			return err
		}
	}
	if len(indexTargets) > 0 {
		return s.cdnPurgeRecorder.RecordMissingCDNPurgeTargets(ctx, indexTargets)
	}
	return nil
}
