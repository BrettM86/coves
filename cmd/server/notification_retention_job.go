package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/notifications"
)

const notificationRetentionInterval = time.Hour

func startNotificationRetentionJob(ctx context.Context, waitGroup *sync.WaitGroup, sweeper notifications.RetentionSweeper, interval time.Duration) {
	runTicker(ctx, waitGroup, "notification-retention", interval, func(ctx context.Context) {
		for _, sweep := range []struct {
			name string
			run  func(context.Context) (int64, error)
		}{
			{"read", sweeper.SweepReadNotifications},
			{"unread_cap", sweeper.SweepUnreadOverflow},
			{"empty_groups", sweeper.SweepEmptyUpvoteGroups},
			{"hidden_references", sweeper.SweepHiddenReferenceNotifications},
		} {
			if ctx.Err() != nil {
				slog.Info("notification retention cycle stopped", "before_sweep", sweep.name, "reason", ctx.Err())
				return
			}
			var totalRemoved int64
			for {
				removed, err := sweep.run(ctx)
				if err != nil {
					if ctx.Err() == nil {
						slog.Error("notification retention sweep failed", "sweep", sweep.name, "error", err)
					}
					break
				}
				totalRemoved += removed
				if removed != notifications.RetentionBatchSize || ctx.Err() != nil {
					break
				}
			}
			if totalRemoved > 0 {
				slog.Info("notification retention sweep completed", "sweep", sweep.name, "removed", totalRemoved)
			}
		}
	})
}
