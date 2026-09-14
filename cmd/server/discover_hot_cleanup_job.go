package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/discover"
)

const (
	discoverHotCleanupInterval            = time.Minute
	discoverHotCleanupDerivedRowBatchSize = 10_000
)

func startDiscoverHotCleanupJob(
	ctx context.Context,
	waitGroup *sync.WaitGroup,
	cleaner discover.DiscoverHotStateCleaner,
	interval time.Duration,
	derivedRowBatchSize int,
) {
	runTicker(ctx, waitGroup, "discover-hot-cleanup", interval, func(ctx context.Context) {
		var totalRemoved int64
		for {
			removed, err := cleaner.CleanupExpiredDiscoverHotState(ctx, derivedRowBatchSize)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Error("Discover Hot cleanup failed", "error", err)
				}
				return
			}
			totalRemoved += removed
			if removed != int64(derivedRowBatchSize) {
				break
			}
		}
		if totalRemoved > 0 {
			slog.Info("Discover Hot cleanup completed", "derived_rows_removed", totalRemoved)
		}
	})
}
