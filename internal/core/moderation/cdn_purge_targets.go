package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/imageproxy"
)

// CDNPurgeTargets receives the owner-scoped pairs a committed transaction recorded.
type CDNPurgeTargets interface {
	PurgeAfterCommit(ctx context.Context, blobs []imageproxy.BlockedBlob)
}

// CDNPurgeTarget is one claimed durable edge-purge target.
type CDNPurgeTarget struct {
	Blob                 imageproxy.BlockedBlob
	Attempts             int
	EarliestCompletionAt time.Time
	Generation           int64
	Claim                int64
}

// CDNPurgeClaim selects due targets for one attempt.
type CDNPurgeClaim struct {
	// DueAt is the cutoff: only targets whose next attempt is at or before it
	// are claimed. A sweep freezes it at the start of its pass.
	DueAt time.Time
	// Now is sampled once after the claimed rows are locked. That sample
	// anchors each claim's lease and, on a target's first claim, its earliest
	// completion, so the anchor is never earlier than the commit that made
	// the target visible. It must not be DueAt: a target re-pended after a
	// sweep began would then complete before its commit plus WriteTimeout.
	Now          func() time.Time
	Lease        time.Duration
	WriteTimeout time.Duration
	Limit        int
	Blobs        []imageproxy.BlockedBlob
	excluded     []imageproxy.BlockedBlob
}

// ExcludedBlobs lists targets already attempted by this sweep. A target
// re-pended during the pass is due at -infinity, so without this list the same
// pass would claim it again whatever DueAt is. The queue sets this only for
// unrestricted claims; it does not change the restriction made by Blobs on
// immediate post-commit claims.
func (claim CDNPurgeClaim) ExcludedBlobs() []imageproxy.BlockedBlob {
	return claim.excluded
}

// CDNPurgeTargetStore persists durable edge-purge targets.
type CDNPurgeTargetStore interface {
	ClaimDueCDNPurgeTargets(ctx context.Context, claim CDNPurgeClaim) ([]CDNPurgeTarget, error)
	CompleteCDNPurgeTarget(ctx context.Context, target CDNPurgeTarget) error
	RescheduleCDNPurgeTarget(ctx context.Context, target CDNPurgeTarget, nextAttemptAt time.Time, failureCode string) error
}

const (
	// cdnPurgeLease is how long a claim holds a target before another worker
	// may claim it.
	cdnPurgeLease = 2 * time.Minute
	// cdnPurgeImmediateAttemptTimeout bounds an immediate post-commit attempt.
	// With cdnPurgeOutcomeWriteTimeout it stays below cdnPurgeLease, so the
	// attempt's outcome is written before its lease can pass to a sweep.
	cdnPurgeImmediateAttemptTimeout = 90 * time.Second
	// cdnPurgeOutcomeWriteTimeout bounds writing an attempt's outcomes, which
	// runs after the attempt's own context may have ended.
	cdnPurgeOutcomeWriteTimeout = 5 * time.Second
	// cdnPurgeImmediateAttemptLimit caps immediate attempts in flight. Beyond
	// it, recorded targets wait for the next sweep.
	cdnPurgeImmediateAttemptLimit = 4
	// cdnPurgeInterruptedCode records an attempt cut off by its context.
	cdnPurgeInterruptedCode = "interrupted"
)

// CDNPurgeQueueConfig configures a CDNPurgeQueue.
type CDNPurgeQueueConfig struct {
	WriteTimeout time.Duration
	Now          func() time.Time
	BatchSize    int
}

// CDNPurgeQueue purges durable targets after commit and on sweeps.
type CDNPurgeQueue struct {
	store    CDNPurgeTargetStore
	purger   CDNPurger
	config   CDNPurgeQueueConfig
	slots    chan struct{}
	mu       sync.Mutex
	closed   bool
	inFlight int
	idle     chan struct{}
}

// NewCDNPurgeQueue builds a durable purge queue.
func NewCDNPurgeQueue(store CDNPurgeTargetStore, purger CDNPurger, config CDNPurgeQueueConfig) *CDNPurgeQueue {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	idle := make(chan struct{})
	close(idle)
	return &CDNPurgeQueue{
		store: store, purger: purger, config: config, idle: idle,
		slots: make(chan struct{}, cdnPurgeImmediateAttemptLimit),
	}
}

// PurgeAfterCommit schedules a bounded, cancellation-independent attempt for
// just the pairs recorded by the committing transaction. After Close, or while
// cdnPurgeImmediateAttemptLimit attempts are in flight, it returns without
// attempting: the targets are durable and the next sweep purges them.
func (q *CDNPurgeQueue) PurgeAfterCommit(ctx context.Context, blobs []imageproxy.BlockedBlob) {
	if len(blobs) == 0 {
		return
	}
	owned := append([]imageproxy.BlockedBlob(nil), blobs...)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	select {
	case q.slots <- struct{}{}:
	default:
		q.mu.Unlock()
		slog.Info("moderation CDN purge immediate attempts saturated; the sweep will purge these targets", "count", len(owned))
		return
	}
	if q.inFlight == 0 {
		q.idle = make(chan struct{})
	}
	q.inFlight++
	q.mu.Unlock()
	go func() {
		defer func() {
			<-q.slots
			q.mu.Lock()
			q.inFlight--
			if q.inFlight == 0 {
				close(q.idle)
			}
			q.mu.Unlock()
		}()
		attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cdnPurgeImmediateAttemptTimeout)
		defer cancel()
		for start := 0; start < len(owned); start += q.config.BatchSize {
			if attemptCtx.Err() != nil {
				slog.Warn("moderation CDN purge immediate attempt ran out of time; the sweep will purge the rest",
					"count", len(owned)-start)
				return
			}
			end := min(start+q.config.BatchSize, len(owned))
			targets, err := q.store.ClaimDueCDNPurgeTargets(attemptCtx, q.claim(owned[start:end], q.config.Now()))
			if err != nil {
				slog.Error("moderation CDN purge claim failed", "error", err)
				return
			}
			if err := q.attempt(attemptCtx, targets); err != nil {
				slog.Error("moderation CDN purge target update failed", "error", err)
				return
			}
		}
	}()
}

func (q *CDNPurgeQueue) claim(blobs []imageproxy.BlockedBlob, dueAt time.Time) CDNPurgeClaim {
	return CDNPurgeClaim{
		DueAt: dueAt, Now: q.config.Now, Lease: cdnPurgeLease, WriteTimeout: q.config.WriteTimeout,
		Limit: q.config.BatchSize, Blobs: blobs,
	}
}

// Sweep drains targets due at the start of this pass. Freezing the due cutoff
// keeps a target rescheduled while a request is held from being retried by the
// same sweep if the clock advances during that request. The store still samples
// the live clock after locking rows for each target's completion anchor.
func (q *CDNPurgeQueue) Sweep(ctx context.Context) error {
	dueAt := q.config.Now()
	var attempted []imageproxy.BlockedBlob
	for {
		claim := q.claim(nil, dueAt)
		claim.excluded = attempted
		targets, err := q.store.ClaimDueCDNPurgeTargets(ctx, claim)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			return ctx.Err()
		}
		if err := q.attempt(ctx, targets); err != nil {
			return err
		}
		for _, target := range targets {
			attempted = append(attempted, target.Blob)
		}
	}
}

// Close stops new immediate attempts. Targets recorded afterwards stay pending
// for the sweep. Attempts already in flight continue; Wait observes them.
func (q *CDNPurgeQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

// InFlight reports how many immediate attempts are running.
func (q *CDNPurgeQueue) InFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inFlight
}

// Wait observes all immediate attempts in flight when called.
func (q *CDNPurgeQueue) Wait(ctx context.Context) error {
	q.mu.Lock()
	idle := q.idle
	q.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return nil
	}
}

func (q *CDNPurgeQueue) attempt(ctx context.Context, targets []CDNPurgeTarget) error {
	if len(targets) == 0 {
		return nil
	}
	blobs := make([]imageproxy.BlockedBlob, len(targets))
	for i, target := range targets {
		blobs[i] = target.Blob
	}
	attemptedAt := q.config.Now()
	result := q.purger.PurgeBlobs(ctx, blobs)
	// A request cut off by ctx fails as transport, as does every request the
	// purger starts after ctx ended. Those pairs were interrupted, not refused.
	interrupted := ctx.Err() != nil
	acknowledged := make(map[imageproxy.BlockedBlob]bool, len(result.Acknowledged))
	failures := make(map[imageproxy.BlockedBlob]string, len(result.Failed))
	for _, blob := range result.Acknowledged {
		acknowledged[blob] = true
	}
	for _, failure := range result.Failed {
		failures[failure.Blob] = failure.Code
	}
	codes := make([]string, len(targets))
	interruptedCount := 0
	for i, target := range targets {
		if acknowledged[target.Blob] {
			continue
		}
		code, ok := failures[target.Blob]
		if interrupted && (!ok || code == "transport") {
			codes[i] = cdnPurgeInterruptedCode
			interruptedCount++
			continue
		}
		if !ok {
			code = "unacknowledged"
		}
		codes[i] = code
		slog.Error("moderation CDN purge failed", "did", target.Blob.OwnerDID, "cid", target.Blob.CID, "code", code)
	}
	if interruptedCount > 0 {
		slog.Warn("moderation CDN purge interrupted; targets rescheduled", "count", interruptedCount)
	}
	// The attempt's context may have ended during the purge; the outcomes are
	// still written so acknowledged pairs complete and failures back off.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cdnPurgeOutcomeWriteTimeout)
	defer cancel()
	var writeErrors []error
	for i, target := range targets {
		var err error
		operation := "reschedule"
		switch {
		case codes[i] != "":
			err = q.store.RescheduleCDNPurgeTarget(writeCtx, target, attemptedAt.Add(cdnPurgeRetryDelay(target.Attempts+1)), codes[i])
		case !attemptedAt.Before(target.EarliestCompletionAt):
			operation = "complete"
			err = q.store.CompleteCDNPurgeTarget(writeCtx, target)
		default:
			err = q.store.RescheduleCDNPurgeTarget(writeCtx, target, target.EarliestCompletionAt, "")
		}
		if err != nil {
			writeErrors = append(writeErrors, fmt.Errorf("%s CDN purge target did=%s cid=%s: %w",
				operation, target.Blob.OwnerDID, target.Blob.CID, err))
		}
	}
	return errors.Join(writeErrors...)
}

// WithCDNPurgeTargets enables transactionally recorded CDN targets.
func WithCDNPurgeTargets(targets CDNPurgeTargets) MediaReconcilerOption {
	return func(reconciler *MediaReconciler) { reconciler.cdnPurgeTargets = targets }
}

func cdnPurgeRetryDelay(failedAttempts int) time.Duration {
	if failedAttempts <= 1 {
		return time.Minute
	}
	if failedAttempts >= 7 {
		return time.Hour
	}
	return time.Minute << (failedAttempts - 1)
}
