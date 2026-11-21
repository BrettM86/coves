package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
)

// RoleRecordDelete is one pending guarded delete of an erased account's role
// record from a hosted community's repo.
type RoleRecordDelete struct {
	ID              int64
	CommunityDID    string
	RecordKey       string
	RecordCID       string
	Attempts        int
	NextAttemptAt   time.Time
	LastFailureCode string
	Generation      int64
	Claim           int64
}

// RoleRecordDeleteClaim selects the due rows one sweep claims.
type RoleRecordDeleteClaim struct {
	DueAt time.Time
	Lease time.Duration
	Limit int
}

// RoleRecordDeleteStore is the durable queue of pending role record deletes.
// Complete and Reschedule act only while the row's ID, generation and claim
// are still the ones claimed; otherwise they persist nothing and report
// ErrRoleRecordDeleteClaimLost.
type RoleRecordDeleteStore interface {
	ClaimDueRoleRecordDeletes(ctx context.Context, claim RoleRecordDeleteClaim) ([]RoleRecordDelete, error)
	// CompleteRoleRecordDelete removes the finished row.
	CompleteRoleRecordDelete(ctx context.Context, d RoleRecordDelete) error
	// RescheduleRoleRecordDelete counts a failed attempt and makes the row
	// due at nextAttemptAt.
	RescheduleRoleRecordDelete(ctx context.Context, d RoleRecordDelete, nextAttemptAt time.Time, failureCode string) error
}

// ErrRoleRecordDeleteClaimLost reports that a row's claim passed to another
// attempt, so nothing was persisted.
var ErrRoleRecordDeleteClaimLost = errors.New("role record delete claim lost")

// RoleRecordDeleterFactory opens a guarded deleter for a hosted community.
type RoleRecordDeleterFactory func(ctx context.Context, communityDID string) (pds.GuardedDeleter, error)

// RoleRecordDeleteQueueConfig configures a RoleRecordDeleteQueue.
type RoleRecordDeleteQueueConfig struct {
	Now func() time.Time
	// AttemptTimeout bounds one row's delete; zero means
	// roleRecordDeleteAttemptTimeout. With the outcome write it must stay
	// below the lease.
	AttemptTimeout time.Duration
}

const (
	// roleRecordDeleteLease is how long a claim holds a row before another
	// worker may claim it.
	roleRecordDeleteLease = 2 * time.Minute
	// Each row is claimed just before its attempt, and
	// roleRecordDeleteAttemptTimeout plus roleRecordDeleteOutcomeWriteTimeout
	// stays below the lease, so the outcome is written before the row can
	// pass to another worker.
	roleRecordDeleteAttemptTimeout      = 90 * time.Second
	roleRecordDeleteOutcomeWriteTimeout = 5 * time.Second

	roleRecordDeleteFailurePDS         = "pds_delete_failed"
	roleRecordDeleteFailureTimedOut    = "pds_timed_out"
	roleRecordDeleteFailureCredentials = "credentials_failed"
)

// RoleRecordDeleteQueue deletes an erased account's role records from the
// hosted communities' repos, guarded by the CID the erased index row held, so
// a record the account wrote again after re-signup is never removed.
type RoleRecordDeleteQueue struct {
	store    RoleRecordDeleteStore
	deleters RoleRecordDeleterFactory
	config   RoleRecordDeleteQueueConfig

	mu      sync.Mutex
	closed  bool
	running bool
	rerun   bool
	idle    chan struct{}
}

// NewRoleRecordDeleteQueue builds the role record delete queue.
func NewRoleRecordDeleteQueue(store RoleRecordDeleteStore, deleters RoleRecordDeleterFactory, config RoleRecordDeleteQueueConfig) *RoleRecordDeleteQueue {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.AttemptTimeout <= 0 {
		config.AttemptTimeout = roleRecordDeleteAttemptTimeout
	}
	if config.AttemptTimeout+roleRecordDeleteOutcomeWriteTimeout >= roleRecordDeleteLease {
		panic("moderation.NewRoleRecordDeleteQueue: AttemptTimeout plus the outcome write timeout must stay below the lease")
	}
	idle := make(chan struct{})
	close(idle)
	return &RoleRecordDeleteQueue{store: store, deleters: deleters, config: config, idle: idle}
}

// Sweep attempts every row due at entry once, claiming each row just before
// its attempt so every attempt starts on a full lease. Each attempt runs on a
// context detached from ctx and bounded by the attempt timeout, so ending the
// sweep never cuts a PDS delete in flight; once ctx is done the sweep claims
// and attempts nothing more. Failed outcome writes are returned joined, or
// ctx.Err() when there are none.
func (q *RoleRecordDeleteQueue) Sweep(ctx context.Context) error {
	dueAt := q.config.Now()
	attempted := make(map[int64]bool)
	var failures []error
	for ctx.Err() == nil {
		rows, err := q.store.ClaimDueRoleRecordDeletes(ctx, RoleRecordDeleteClaim{
			DueAt: dueAt, Lease: roleRecordDeleteLease, Limit: 1,
		})
		if err != nil {
			if ctx.Err() == nil {
				failures = append(failures, err)
			}
			break
		}
		// A row already attempted in this pass is left to the next sweep. A
		// row claimed after ctx is done waits out its lease.
		if len(rows) == 0 || attempted[rows[0].ID] || ctx.Err() != nil {
			break
		}
		row := rows[0]
		attempted[row.ID] = true
		if err := q.attempt(ctx, row); err != nil {
			slog.Error("role record delete outcome update failed",
				"community", row.CommunityDID, "error", err)
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return ctx.Err()
}

// attempt sends one row's guarded delete and writes its outcome under the
// row's claim.
func (q *RoleRecordDeleteQueue) attempt(ctx context.Context, row RoleRecordDelete) error {
	attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), q.config.AttemptTimeout)
	defer cancel()
	attemptedAt := q.config.Now()

	complete := false
	failureCode := ""
	deleter, err := q.deleters(attemptCtx, row.CommunityDID)
	switch {
	case err == nil:
		err = deleter.DeleteRecordWithSwap(attemptCtx, communities.RoleCollection, row.RecordKey, row.RecordCID)
		switch {
		case err == nil, errors.Is(err, pds.ErrNotFound), errors.Is(err, pds.ErrSwapConflict):
			// Deleted, already gone, or replaced by a newer record the
			// guard protects: nothing more is owed.
			complete = true
		case attemptCtx.Err() != nil:
			failureCode = roleRecordDeleteFailureTimedOut
		default:
			failureCode = roleRecordDeleteFailurePDS
		}
	case errors.Is(err, posts.ErrCommunityNotHosted):
		// This AppView no longer holds the community's credentials, so no
		// retry can delete.
		slog.Info("role record delete skipped: the community is not hosted by this AppView",
			"community", row.CommunityDID)
		complete = true
	case attemptCtx.Err() != nil:
		failureCode = roleRecordDeleteFailureTimedOut
	default:
		failureCode = roleRecordDeleteFailureCredentials
	}

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), roleRecordDeleteOutcomeWriteTimeout)
	defer cancelWrite()
	var outcomeErr error
	if complete {
		outcomeErr = q.store.CompleteRoleRecordDelete(writeCtx, row)
	} else {
		slog.Error("role record delete failed",
			"community", row.CommunityDID, "attempts", row.Attempts+1, "code", failureCode, "error", err)
		next := attemptedAt.Add(cdnPurgeRetryDelay(row.Attempts + 1))
		outcomeErr = q.store.RescheduleRoleRecordDelete(writeCtx, row, next, failureCode)
	}
	switch {
	case outcomeErr == nil:
		return nil
	case errors.Is(outcomeErr, ErrRoleRecordDeleteClaimLost):
		slog.Warn("role record delete claim lost before its outcome was written; the current claim owns the row",
			"community", row.CommunityDID, "complete", complete)
		return nil
	case complete:
		return fmt.Errorf("complete role record delete community=%s: %w", row.CommunityDID, outcomeErr)
	default:
		return fmt.Errorf("reschedule role record delete community=%s: %w", row.CommunityDID, outcomeErr)
	}
}

// AfterErasure starts a background sweep after an account erasure commits, so
// the erased account's role records go without waiting for the periodic
// sweep. Sweeps are single-flight: a call while one runs makes it run once
// more after it finishes, which picks up rows committed during the pass. The
// sweep runs on a context detached from ctx. After Close it does nothing; the
// rows stay pending for the periodic sweep.
func (q *RoleRecordDeleteQueue) AfterErasure(ctx context.Context, did string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.running {
		q.rerun = true
		return
	}
	q.running = true
	q.idle = make(chan struct{})
	go q.runBackground(context.WithoutCancel(ctx))
}

func (q *RoleRecordDeleteQueue) runBackground(ctx context.Context) {
	for {
		q.backgroundSweep(ctx)
		q.mu.Lock()
		if !q.rerun || q.closed {
			q.running = false
			q.rerun = false
			close(q.idle)
			q.mu.Unlock()
			return
		}
		q.rerun = false
		q.mu.Unlock()
	}
}

func (q *RoleRecordDeleteQueue) backgroundSweep(ctx context.Context) {
	// The rows are durable, so a panic loses only this pass.
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("role record delete background sweep panicked; the periodic sweep will retry", "panic", recovered)
		}
	}()
	if err := q.Sweep(ctx); err != nil {
		slog.Error("role record delete background sweep failed", "error", err)
	}
}

// Close stops new background sweeps. A sweep in flight finishes its pass;
// Wait observes it.
func (q *RoleRecordDeleteQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

// Wait waits for the background sweep in flight when called, or for ctx.
func (q *RoleRecordDeleteQueue) Wait(ctx context.Context) error {
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
