package main

import (
	"context"
	"errors"
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
	subjectReader       moderation.SubjectReader
	store               moderation.Store
	mediaBinder         moderation.TransactionBinder
	mediaPurger         moderation.MediaPurger
	communityResolver   moderation.CommunityResolver
	handleResolver      moderation.HandleResolver
	cdnPurgeTargetStore moderation.CDNPurgeTargetStore
}

// buildModeration shares the same durable queue between admin removals and
// consumer reconciliation when Cloudflare purge is configured.
func buildModeration(cfg *config.Config, apiBase string, dependencies moderationDependencies) (moderation.Service, *moderation.MediaReconciler, *moderation.CDNPurgeQueue, error) {
	cdnPurger, err := buildCDNPurger(cfg.Media.CDNPurge, apiBase)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating CDN purger: %w", err)
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
	var queue *moderation.CDNPurgeQueue
	if cdnPurger != nil {
		if dependencies.cdnPurgeTargetStore == nil {
			return nil, nil, nil, errors.New("CDN purge target store is required when CDN purge is configured")
		}
		queue = moderation.NewCDNPurgeQueue(dependencies.cdnPurgeTargetStore, cdnPurger,
			moderation.CDNPurgeQueueConfig{WriteTimeout: cfg.Server.WriteTimeout})
		moderationConfig.CDNPurgeTargets = queue
		reconcilerOptions = append(reconcilerOptions, moderation.WithCDNPurgeTargets(queue))
	}
	service := moderation.NewService(dependencies.subjectReader, dependencies.store, moderationConfig)
	reconciler := moderation.NewMediaReconciler(
		dependencies.mediaBinder, cfg.Instance.DID, dependencies.mediaPurger, reconcilerOptions...)
	return service, reconciler, queue, nil
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
