package imageproxy

import (
	"context"
	"log/slog"
	"strings"
)

// CDNPurgeRecorder durably commits CDN purge targets before local cache
// entries are deleted.
type CDNPurgeRecorder interface {
	RecordCDNPurgeTargets(ctx context.Context, blobs []BlockedBlob) error
}

// ServiceOption configures an ImageProxyService.
type ServiceOption func(*ImageProxyService)

// WithCDNPurgeRecorder records purge targets before blocked cache bytes are removed.
func WithCDNPurgeRecorder(recorder CDNPurgeRecorder) ServiceOption {
	return func(service *ImageProxyService) { service.cdnPurgeRecorder = recorder }
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

// recordCachedOwners names every recoverable owner before an ownerless purge.
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
		if seen[directory] {
			continue
		}
		seen[directory] = true
		owner, mapped := ownerDIDFromCacheDirectory(directory)
		if !mapped {
			slog.Warn("[IMAGE-PROXY] cannot name cached owner for CDN purge", "directory", directory, "cid", cid)
			continue
		}
		targets = append(targets, BlockedBlob{OwnerDID: owner, CID: cid})
	}
	if len(targets) == 0 {
		return nil
	}
	return s.cdnPurgeRecorder.RecordCDNPurgeTargets(ctx, targets)
}
