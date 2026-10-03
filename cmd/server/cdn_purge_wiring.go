package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
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
	cdnPurgeQueue     *moderation.CDNPurgeQueue
}

// buildModeration shares the durable purge queue with removal and reconciliation
// when CDN purge is configured.
func buildModeration(cfg *config.Config, dependencies moderationDependencies) (moderation.Service, *moderation.MediaReconciler, error) {
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
	if dependencies.cdnPurgeQueue != nil {
		moderationConfig.CDNPurgeTargets = dependencies.cdnPurgeQueue
		reconcilerOptions = append(reconcilerOptions, moderation.WithCDNPurgeTargets(dependencies.cdnPurgeQueue))
	}
	service := moderation.NewService(dependencies.subjectReader, dependencies.store, moderationConfig)
	reconciler := moderation.NewMediaReconciler(dependencies.mediaBinder, cfg.Instance.DID, dependencies.mediaPurger, reconcilerOptions...)
	return service, reconciler, nil
}

// buildCDNPurgeQueue constructs the single queue shared by the media proxy and
// moderation, or leaves it off when Cloudflare purge is unconfigured.
func buildCDNPurgeQueue(cfg *config.Config, apiBase string, store moderation.CDNPurgeTargetStore) (*moderation.CDNPurgeQueue, error) {
	purger, err := buildCDNPurger(cfg.Media.CDNPurge, apiBase)
	if err != nil {
		return nil, fmt.Errorf("creating CDN purger: %w", err)
	}
	if purger == nil {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("CDN purge enabled without a target store")
	}
	return moderation.NewCDNPurgeQueue(store, purger, moderation.CDNPurgeQueueConfig{
		WriteTimeout: cfg.Server.WriteTimeout,
	}), nil
}

// buildCDNPurger returns nil when no Cloudflare edge invalidation is configured.
func buildCDNPurger(purge config.CDNPurgeConfig, apiBase string) (moderation.CDNPurger, error) {
	if !purge.Enabled() {
		slog.Info("CDN purge is off; responses now allow shared caches one day (copies cached under the old one-year header need the one-time Purge Everything, PRD_CSAM_SCANNING.md rollout step 4)")
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

// cdnPurgeSweepInterval is how often the CDN retry sweep runs after boot.
const cdnPurgeSweepInterval = time.Minute

// cdnPurgeSweeper processes due durable CDN purge targets.
type cdnPurgeSweeper interface {
	Sweep(ctx context.Context) error
}

// startCDNPurgeSweepJob retries due targets at boot and each interval.
func startCDNPurgeSweepJob(ctx context.Context, waitGroup *sync.WaitGroup, sweeper cdnPurgeSweeper, interval time.Duration) {
	if sweeper == nil || (reflect.ValueOf(sweeper).Kind() == reflect.Ptr && reflect.ValueOf(sweeper).IsNil()) || interval <= 0 {
		return
	}
	runTicker(ctx, waitGroup, "cdn-purge-sweep", interval, func(ctx context.Context) {
		// An ended cycle context (shutdown or the cycle deadline) surfaces as
		// driver errors that need not wrap ctx.Err(); the next cycle resumes.
		if err := sweeper.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("moderation CDN purge sweep failed", "error", err)
		}
	})
}
