package main

import (
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
)

// cloudflareAPIBase is the Cloudflare API root production purges against.
const cloudflareAPIBase = "https://api.cloudflare.com/client/v4" // coves:allow-public-host: fixed Cloudflare cache purge endpoint

// moderationDependencies are the storage, media and resolution seams the
// moderation service and media reconciler are built from.
type moderationDependencies struct {
	subjectReader     moderation.SubjectReader
	store             moderation.Store
	mediaBinder       moderation.TransactionBinder
	mediaPurger       moderation.MediaPurger
	communityResolver moderation.CommunityResolver
	handleResolver    moderation.HandleResolver
}

// buildModeration builds the moderation service and the media reconciler, and
// gives both the CDN purger when Cloudflare purge is configured: the service
// purges after a removal commits, the reconciler after a consumer reconcile
// commits. apiBase is cloudflareAPIBase in production.
func buildModeration(cfg *config.Config, apiBase string, dependencies moderationDependencies) (moderation.Service, *moderation.MediaReconciler, error) {
	cdnPurger, err := buildCDNPurger(cfg.Media.CDNPurge, apiBase)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CDN purger: %w", err)
	}
	moderationConfig := moderation.Config{
		InstanceDID:            cfg.Instance.DID,
		IdempotencyRetention:   cfg.Moderation.IdempotencyRetention,
		MaxLiveIdempotencyKeys: cfg.Moderation.MaxLiveIdempotencyKeys,
		Purger:                 dependencies.mediaPurger,
		CursorSecret:           cfg.CursorSecret,
		CommunityResolver:      dependencies.communityResolver,
		HandleResolver:         dependencies.handleResolver,
	}
	var reconcilerOptions []moderation.MediaReconcilerOption
	if cdnPurger != nil {
		moderationConfig.CDNPurger = cdnPurger
		reconcilerOptions = append(reconcilerOptions, moderation.WithCDNPurger(cdnPurger))
	}
	service := moderation.NewService(dependencies.subjectReader, dependencies.store, moderationConfig)
	reconciler := moderation.NewMediaReconciler(
		dependencies.mediaBinder, cfg.Instance.DID, dependencies.mediaPurger, reconcilerOptions...)
	return service, reconciler, nil
}

// buildCDNPurger returns nil when no Cloudflare edge invalidation is configured.
func buildCDNPurger(purge config.CDNPurgeConfig, apiBase string) (moderation.CDNPurger, error) {
	if !purge.Enabled() {
		slog.Info("CDN purge is off; shared caches can keep an image for at most a day")
		return nil, nil
	}
	return imageproxy.NewCloudflarePurger(imageproxy.CloudflarePurgerConfig{
		APIBase:  apiBase,
		ZoneID:   purge.ZoneID,
		APIToken: purge.APIToken,
		BaseURLs: purge.BaseURLs,
		Timeout:  10 * time.Second,
	})
}
