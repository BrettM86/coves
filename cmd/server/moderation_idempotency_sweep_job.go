package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const moderationIdempotencySweepInterval = time.Hour

// moderationIdempotencySweeper deletes expired moderation idempotency keys.
type moderationIdempotencySweeper interface {
	DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int64, error)
}

// startModerationIdempotencySweepJob periodically deletes expired keys.
func startModerationIdempotencySweepJob(ctx context.Context, waitGroup *sync.WaitGroup, sweeper moderationIdempotencySweeper, interval time.Duration) {
	if sweeper == nil || interval <= 0 {
		slog.Warn("moderation idempotency sweep job not started", "sweeper_present", sweeper != nil, "interval", interval)
		return
	}
	runTicker(ctx, waitGroup, "moderation-idempotency-sweep", interval, func(ctx context.Context) {
		deleted, err := sweeper.DeleteExpiredIdempotencyKeys(ctx, time.Now())
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Error("moderation idempotency sweep failed", "error", err)
			}
			return
		}
		if deleted > 0 {
			slog.Info("moderation idempotency sweep completed", "expired_keys_removed", deleted)
		}
	})
}
