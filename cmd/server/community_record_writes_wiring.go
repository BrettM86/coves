package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/moderation"
)

// communityRecordWriteSweepInterval is how often the community record write
// sweep runs after boot. runTicker also bounds each cycle by it, so it exceeds
// one attempt's timeout plus its outcome write (90 s + 5 s): a sweep that
// claims a row near the end of its cycle still sees the attempt through
// before the next cycle starts.
const communityRecordWriteSweepInterval = 2 * time.Minute

// startCommunityRecordWriteSweepJob reconciles due community record write rows
// at boot and each interval.
func startCommunityRecordWriteSweepJob(ctx context.Context, waitGroup *sync.WaitGroup, queue *moderation.CommunityRecordWriteQueue, interval time.Duration) {
	if queue == nil || interval <= 0 {
		return
	}
	runTicker(ctx, waitGroup, "community-record-write-sweep", interval, func(ctx context.Context) {
		// Sweep logs each failed row itself. An ended cycle context surfaces
		// as driver errors that need not wrap ctx.Err(); the next cycle
		// resumes.
		if err := queue.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("community record write sweep failed", "error", err)
		}
	})
}
