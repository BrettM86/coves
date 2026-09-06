package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// MaxRedriveAttempts is the redrive budget for infrastructure failures.
	// Rows at or above this value are skipped by the redriver.
	MaxRedriveAttempts = 10

	// UnresolvedRedriveAttempts is the smaller budget for attacker-controlled
	// references that may be genuine ordering races or may never exist.
	UnresolvedRedriveAttempts = 3

	// DeadLetterRetention bounds storage growth while leaving a week for
	// inspection and recovery. Pruning uses updated_at, so every real replay
	// attempt extends the window.
	DeadLetterRetention = 7 * 24 * time.Hour
)

// DeadLetterEvent is a dead-lettered Jetstream event awaiting redrive.
type DeadLetterEvent struct {
	ID           int64
	ConsumerName string
	EventTimeUS  int64
	EventData    []byte
	LastError    string
	Attempts     int
	CreatedAt    time.Time
	// UpdatedAt is the time of the row's last redrive attempt (its insertion
	// time until the first attempt).
	UpdatedAt time.Time
}

// DeadLetterPageQuery bounds one ID-ordered redrive page within a pass.
type DeadLetterPageQuery struct {
	ConsumerName string
	MaxAttempts  int
	AfterID      int64
	ThroughID    int64
	Limit        int
	// MinimumAge excludes rows whose UpdatedAt is newer than now minus this
	// duration. Zero means no age filter.
	MinimumAge time.Duration
}

// DeadLetterRedriveSource snapshots and reads replayable dead letters.
type DeadLetterRedriveSource interface {
	// LatestDeadLetterID returns the current high-water mark for a consumer.
	// A redrive pass snapshots it so rows arriving during the pass wait for the
	// next one and no row is attempted twice in one pass.
	LatestDeadLetterID(ctx context.Context, consumerName string) (int64, error)
	// ListRetryable returns one ID-ordered page inside a pass snapshot.
	ListRetryable(ctx context.Context, query DeadLetterPageQuery) ([]DeadLetterEvent, error)
}

// DeadLetterRedriveMutator applies the three possible replay outcomes.
type DeadLetterRedriveMutator interface {
	// DeleteDeadLetter removes a successfully redriven event.
	DeleteDeadLetter(ctx context.Context, id int64) error
	// MarkRedriveAttempt increments the attempt counter after a failed
	// redrive. If this write fails the event will be retried again next
	// pass; maxAttempts bounds recorded attempts, not total handler
	// invocations.
	MarkRedriveAttempt(ctx context.Context, id int64, handleErr string) error
	// RetireDeadLetter marks a dead letter as permanently exhausted in one
	// step (attempts jump straight to MaxRedriveAttempts). Used for rows
	// that can never succeed, e.g. unparseable payloads. The row remains
	// for forensics until the retention window expires.
	RetireDeadLetter(ctx context.Context, id int64, reason string) error
}

// DeadLetterPruner enforces the dead-letter retention window.
type DeadLetterPruner interface {
	// PruneDeadLetters deletes rows whose last update is older than before.
	PruneDeadLetters(ctx context.Context, before time.Time) (int64, error)
}

// DeadLetterCounter supplies backlog counts to operational health reporting.
type DeadLetterCounter interface {
	// CountDeadLetters returns the dead letter backlog per consumer.
	CountDeadLetters(ctx context.Context) (map[string]int64, error)
}

// DeadLetterRedriveStore groups the small persistence capabilities the
// redriver owns without forcing connectors or health reporting to depend on
// operations they never call.
type DeadLetterRedriveStore struct {
	Source  DeadLetterRedriveSource
	Mutator DeadLetterRedriveMutator
	Pruner  DeadLetterPruner
}

// DeadLetterRedriver periodically replays dead-lettered events against the
// same consumers that originally failed them. This makes transient failures
// (e.g. a Postgres blip) self-healing: the event lands in the DLQ, the
// failure clears, and the next redrive pass indexes it.
type DeadLetterRedriver struct {
	source      DeadLetterRedriveSource
	mutator     DeadLetterRedriveMutator
	pruner      DeadLetterPruner
	handlers    map[string]EventHandler // keyed by consumer name
	interval    time.Duration
	batchSize   int
	maxAttempts int
	minimumAge  time.Duration
}

// RedriveOption configures optional DeadLetterRedriver behaviour.
type RedriveOption func(*DeadLetterRedriver)

// WithRedriveInterval sets how often Run starts a redrive pass, replacing the
// constructor's five-minute default.
func WithRedriveInterval(interval time.Duration) RedriveOption {
	return func(r *DeadLetterRedriver) { r.interval = interval }
}

// WithRedriveMinimumAge makes a pass skip rows attempted or inserted within
// the last age, so a redrive never re-asks a question whose answer is still
// held by the identity negative cache.
func WithRedriveMinimumAge(age time.Duration) RedriveOption {
	return func(r *DeadLetterRedriver) { r.minimumAge = age }
}

// NewDeadLetterRedriver creates a redriver over the given consumers.
// Events that exhaust maxAttempts redrives stay in the table for the retention
// window and are surfaced in the /health/consumers backlog counts.
func NewDeadLetterRedriver(store DeadLetterRedriveStore, handlers map[string]EventHandler, opts ...RedriveOption) *DeadLetterRedriver {
	r := &DeadLetterRedriver{
		source:      store.Source,
		mutator:     store.Mutator,
		pruner:      store.Pruner,
		handlers:    handlers,
		interval:    5 * time.Minute,
		batchSize:   100,
		maxAttempts: MaxRedriveAttempts,
	}
	for _, opt := range opts {
		opt(r)
	}
	// A non-positive interval panics time.NewTicker, and Run builds its ticker
	// inside the goroutine cmd/server launches and never joins — so the failure
	// would land after boot has reported healthy, on a stack naming the ticker
	// rather than the configuration that supplied the value. Raised here it
	// happens during wiring, on the main goroutine, before anything is served.
	// The environment path already rejects this at load; what reaches here is a
	// hand-assembled config, which is programmer error.
	if r.interval <= 0 {
		panic(fmt.Sprintf("jetstream: NewDeadLetterRedriver needs a positive redrive interval, got %s", r.interval))
	}
	return r
}

// Run redrives dead letters on an interval until ctx is cancelled.
func (r *DeadLetterRedriver) Run(ctx context.Context) {
	// Initial pass at boot: a backlog accumulated while the process was
	// down should not have to wait out the first full interval.
	r.redriveAll(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("jetstream dead letter redriver stopped")
			return
		case <-ticker.C:
			r.redriveAll(ctx)
		}
	}
}

// redriveAll runs one redrive pass across every registered consumer,
// draining each consumer's retryable backlog batch by batch so a large
// backlog does not take one 100-row batch per tick to clear.
func (r *DeadLetterRedriver) redriveAll(ctx context.Context) {
	for consumerName, handler := range r.handlers {
		if ctx.Err() != nil {
			return
		}

		throughID, err := r.source.LatestDeadLetterID(ctx, consumerName)
		if err != nil {
			slog.Error("jetstream dead letter redrive snapshot failed",
				slog.String("consumer", consumerName), slog.String("error", err.Error()))
			continue
		}

		totalRedriven, totalFailed := 0, 0
		var afterID int64
		for {
			if ctx.Err() != nil {
				return
			}
			query := DeadLetterPageQuery{
				ConsumerName: consumerName,
				MaxAttempts:  r.maxAttempts,
				AfterID:      afterID,
				ThroughID:    throughID,
				Limit:        r.batchSize,
				MinimumAge:   r.minimumAge,
			}
			redriven, failed, listed, lastID, err := r.redriveConsumer(ctx, handler, query)
			if err != nil {
				slog.Error("jetstream dead letter redrive pass failed",
					slog.String("consumer", consumerName), slog.String("error", err.Error()))
				break
			}
			totalRedriven += redriven
			totalFailed += failed
			if listed == 0 || listed < r.batchSize || lastID >= throughID {
				break
			}
			afterID = lastID
		}
		if totalRedriven > 0 || totalFailed > 0 {
			slog.Info("jetstream dead letter redrive pass completed",
				slog.String("consumer", consumerName),
				slog.Int("redriven", totalRedriven),
				slog.Int("still_failing", totalFailed))
		}
	}

	if ctx.Err() != nil {
		return
	}
	pruned, err := r.pruner.PruneDeadLetters(ctx, time.Now().Add(-DeadLetterRetention))
	if err != nil {
		slog.Error("failed to prune expired jetstream dead letters", slog.String("error", err.Error()))
	} else if pruned > 0 {
		slog.Info("pruned expired jetstream dead letters", slog.Int64("rows", pruned))
	}
}

// redriveConsumer replays one batch of dead letters for a single consumer.
// listed reports how many rows the batch contained so the caller can tell
// when the backlog is drained.
func (r *DeadLetterRedriver) redriveConsumer(
	ctx context.Context,
	handler EventHandler,
	query DeadLetterPageQuery,
) (redriven, failed, listed int, lastID int64, err error) {
	deadLetters, err := r.source.ListRetryable(ctx, query)
	if err != nil {
		return 0, 0, 0, query.AfterID, err
	}
	listed = len(deadLetters)
	lastID = query.AfterID
	if listed > 0 {
		lastID = deadLetters[listed-1].ID
	}

	for _, deadLetter := range deadLetters {
		if ctx.Err() != nil {
			return redriven, failed, listed, lastID, nil
		}

		var event JetstreamEvent
		if parseErr := json.Unmarshal(deadLetter.EventData, &event); parseErr != nil {
			// Unparseable payloads can never succeed; retire them in one
			// step so they stop consuming redrive passes but remain for
			// forensics.
			failed++
			if retireErr := r.mutator.RetireDeadLetter(ctx, deadLetter.ID, "unparseable event: "+parseErr.Error()); retireErr != nil {
				slog.Error("failed to retire unparseable dead letter",
					slog.Int64("id", deadLetter.ID), slog.String("error", retireErr.Error()))
			}
			continue
		}

		if handleErr := handler.HandleEvent(ctx, &event); handleErr != nil {
			// Only cancellation of the redriver's own context means shutdown.
			// A handler may wrap context.DeadlineExceeded from its own bounded
			// network call while this context remains live; that is a real replay
			// attempt and must neither become free nor abort the rest of the page.
			if ctx.Err() != nil {
				return redriven, failed, listed, lastID, nil
			}
			failed++
			if errors.Is(handleErr, ErrPermanentEvent) {
				if retireErr := r.mutator.RetireDeadLetter(ctx, deadLetter.ID, handleErr.Error()); retireErr != nil {
					slog.Error("failed to retire permanently rejected dead letter",
						slog.Int64("id", deadLetter.ID), slog.String("error", retireErr.Error()))
				}
				continue
			}
			if errors.Is(handleErr, errRedriveDeferred) {
				continue
			}
			if markErr := r.mutator.MarkRedriveAttempt(ctx, deadLetter.ID, handleErr.Error()); markErr != nil {
				slog.Error("failed to mark dead letter redrive attempt",
					slog.Int64("id", deadLetter.ID), slog.String("error", markErr.Error()))
			}
			continue
		}

		if deleteErr := r.mutator.DeleteDeadLetter(ctx, deadLetter.ID); deleteErr != nil {
			// The event was indexed but the row remains; the next pass will
			// replay it, which is safe because handlers are idempotent.
			slog.Error("failed to delete redriven dead letter (will replay next pass)",
				slog.Int64("id", deadLetter.ID), slog.String("error", deleteErr.Error()))
			continue
		}
		redriven++
	}
	return redriven, failed, listed, lastID, nil
}
