package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"
)

// imageProxyDependencies are the seams the image proxy service and handler
// are built from.
type imageProxyDependencies struct {
	cache     imageproxy.Cache
	processor imageproxy.Processor
	fetcher   imageproxy.Fetcher
	blocks    imageproxy.BlockChecker
	lister    imageproxy.BlockedBlobLister
	resolver  identity.Resolver
}

// buildImageProxyService installs the recorder before the startup sweep runs.
func buildImageProxyService(cfg imageproxy.Config, purge config.CDNPurgeConfig, queue *moderation.CDNPurgeQueue, dependencies imageProxyDependencies) (*imageproxy.ImageProxyService, *imageproxyhandlers.Handler, context.CancelFunc, error) {
	var serviceOptions []imageproxy.ServiceOption
	var handlerOptions []imageproxyhandlers.HandlerOption
	if queue != nil {
		serviceOptions = append(serviceOptions, imageproxy.WithCDNPurgeRecorder(queue))
		handlerOptions = append(handlerOptions, imageproxyhandlers.WithCDNPurgeBaseURLs(purge.BaseURLs))
	}
	service, err := imageproxy.NewService(dependencies.cache, dependencies.processor, dependencies.fetcher, dependencies.blocks, cfg, serviceOptions...)
	if err != nil {
		return nil, nil, nil, err
	}
	stopPurge := startBlockedMediaPurgeJob(service, dependencies.lister, cfg.CleanupInterval)
	handler := imageproxyhandlers.NewHandler(service, dependencies.resolver, handlerOptions...)
	return service, handler, stopPurge, nil
}

// blockedMediaStartupPurgeTimeout bounds the single startup sweep that runs
// when the purge interval is zero, matching the default cleanup interval.
const blockedMediaStartupPurgeTimeout = time.Hour

// blockedMediaPurger is the sweep startBlockedMediaPurgeJob drives.
type blockedMediaPurger interface {
	PurgeActiveBlocks(ctx context.Context, lister imageproxy.BlockedBlobLister) error
}

// startBlockedMediaPurgeJob sweeps straight away, which completes any purge a
// restart interrupted, and then every interval. An interval of zero or less
// runs only the startup sweep. Each cycle recovers its own panic and runs under
// a deadline, so one bad cycle costs one cycle rather than the job. The
// returned function stops the job and waits for an in-flight sweep to end.
func startBlockedMediaPurgeJob(purger blockedMediaPurger, lister imageproxy.BlockedBlobLister, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	work := func(ctx context.Context) {
		if err := purger.PurgeActiveBlocks(ctx, lister); err != nil && ctx.Err() == nil {
			slog.Error("[IMAGE-PROXY] blocked media purge failed", "error", err)
		}
	}
	if interval > 0 {
		runTicker(ctx, &waitGroup, "blocked-media-purge", interval, work)
	} else {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			runGuarded(ctx, "blocked-media-purge", blockedMediaStartupPurgeTimeout, work)
		}()
	}
	return func() {
		cancel()
		waitGroup.Wait()
	}
}
