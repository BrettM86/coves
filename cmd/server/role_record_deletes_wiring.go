package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
)

// roleRecordDeleteSweepInterval is how often the role record delete sweep runs
// after boot. runTicker also bounds each cycle by it: the sweep starts no
// attempt after the cycle's deadline, so a cycle ends at most one attempt's
// timeout plus its outcome write (90 s + 5 s) after it.
const roleRecordDeleteSweepInterval = 2 * time.Minute

// newRoleRecordDeleterFactory opens a hosted community's repo through repos and
// returns its guarded delete surface. repos is called per attempt, so it may
// resolve a dependency built after the queue.
func newRoleRecordDeleterFactory(repos func() posts.CommunityRepoFactory) moderation.RoleRecordDeleterFactory {
	return func(ctx context.Context, communityDID string) (pds.GuardedDeleter, error) {
		repo, err := repos()(ctx, communityDID)
		if err != nil {
			return nil, err
		}
		deleter, ok := repo.(pds.GuardedDeleter)
		if !ok {
			return nil, fmt.Errorf("building a PDS client for %s: the client does not implement the guarded delete", communityDID)
		}
		return deleter, nil
	}
}

// startRoleRecordDeleteSweepJob attempts due role record deletes at boot and
// each interval.
func startRoleRecordDeleteSweepJob(ctx context.Context, waitGroup *sync.WaitGroup, queue *moderation.RoleRecordDeleteQueue, interval time.Duration) {
	if queue == nil || interval <= 0 {
		return
	}
	runTicker(ctx, waitGroup, "role-record-delete-sweep", interval, func(ctx context.Context) {
		// Sweep logs each failed row itself; the next cycle resumes.
		if err := queue.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("role record delete sweep failed", "error", err)
		}
	})
}
