package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// MaxRedriveAttempts is the redrive budget for a dead letter. Rows at or
// above this many attempts are skipped by the redriver but remain in the
// table for manual inspection. The connector dead-letters permanent
// failures (ErrPermanentEvent) with attempts already at this value so the
// redriver never touches them.
const MaxRedriveAttempts = 10

// DeadLetterEvent is a dead-lettered Jetstream event awaiting redrive.
type DeadLetterEvent struct {
	ID           int64
	ConsumerName string
	EventTimeUS  int64
	EventData    []byte
	LastError    string
	Attempts     int
	CreatedAt    time.Time
}

// DeadLetterQueue is the full dead letter store used by the redriver.
// Connectors only need the narrower DeadLetterWriter.
type DeadLetterQueue interface {
	DeadLetterWriter
	// ListRetryable returns up to limit dead letters for the consumer with
	// fewer than maxAttempts redrive attempts, oldest first.
	ListRetryable(ctx context.Context, consumerName string, maxAttempts, limit int) ([]DeadLetterEvent, error)
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
	// for forensics and backlog counts.
	RetireDeadLetter(ctx context.Context, id int64, reason string) error
	// CountDeadLetters returns the dead letter backlog per consumer.
	CountDeadLetters(ctx context.Context) (map[string]int64, error)
}

// DeadLetterRedriver periodically replays dead-lettered events against the
// same consumers that originally failed them. This makes transient failures
// (e.g. a Postgres blip) self-healing: the event lands in the DLQ, the
// failure clears, and the next redrive pass indexes it.
type DeadLetterRedriver struct {
	queue       DeadLetterQueue
	handlers    map[string]EventHandler // keyed by consumer name
	interval    time.Duration
	batchSize   int
	maxAttempts int
}

// RedriveOption configures optional DeadLetterRedriver behaviour.
type RedriveOption func(*DeadLetterRedriver)

// WithRedriveInterval sets how often Run starts a redrive pass, replacing the
// constructor's five-minute default.
func WithRedriveInterval(interval time.Duration) RedriveOption {
	return func(r *DeadLetterRedriver) { r.interval = interval }
}

// NewDeadLetterRedriver creates a redriver over the given consumers.
// Events that exhaust maxAttempts redrives stay in the table for manual
// inspection and are surfaced in the /health/consumers backlog counts.
func NewDeadLetterRedriver(queue DeadLetterQueue, handlers map[string]EventHandler, opts ...RedriveOption) *DeadLetterRedriver {
	r := &DeadLetterRedriver{
		queue:       queue,
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

		totalRedriven, totalFailed := 0, 0
		for {
			if ctx.Err() != nil {
				return
			}
			redriven, failed, listed, err := r.redriveConsumer(ctx, consumerName, handler)
			if err != nil {
				slog.Error("jetstream dead letter redrive pass failed",
					slog.String("consumer", consumerName), slog.String("error", err.Error()))
				break
			}
			totalRedriven += redriven
			totalFailed += failed
			// A short batch means the retryable backlog is drained.
			if listed < r.batchSize {
				break
			}
		}
		if totalRedriven > 0 || totalFailed > 0 {
			slog.Info("jetstream dead letter redrive pass completed",
				slog.String("consumer", consumerName),
				slog.Int("redriven", totalRedriven),
				slog.Int("still_failing", totalFailed))
		}
	}
}

// redriveConsumer replays one batch of dead letters for a single consumer.
// listed reports how many rows the batch contained so the caller can tell
// when the backlog is drained.
func (r *DeadLetterRedriver) redriveConsumer(ctx context.Context, consumerName string, handler EventHandler) (redriven, failed, listed int, err error) {
	deadLetters, err := r.queue.ListRetryable(ctx, consumerName, r.maxAttempts, r.batchSize)
	if err != nil {
		return 0, 0, 0, err
	}
	listed = len(deadLetters)

	for _, deadLetter := range deadLetters {
		if ctx.Err() != nil {
			return redriven, failed, listed, nil
		}

		var event JetstreamEvent
		if parseErr := json.Unmarshal(deadLetter.EventData, &event); parseErr != nil {
			// Unparseable payloads can never succeed; retire them in one
			// step so they stop consuming redrive passes but remain for
			// forensics.
			failed++
			if retireErr := r.queue.RetireDeadLetter(ctx, deadLetter.ID, "unparseable event: "+parseErr.Error()); retireErr != nil {
				slog.Error("failed to retire unparseable dead letter",
					slog.Int64("id", deadLetter.ID), slog.String("error", retireErr.Error()))
			}
			continue
		}

		if handleErr := handler.HandleEvent(ctx, &event); handleErr != nil {
			// A failure caused by shutdown is not the event's fault: return
			// without burning one of its redrive attempts.
			if ctx.Err() != nil || errors.Is(handleErr, context.Canceled) || errors.Is(handleErr, context.DeadlineExceeded) {
				return redriven, failed, listed, nil
			}
			failed++
			if markErr := r.queue.MarkRedriveAttempt(ctx, deadLetter.ID, handleErr.Error()); markErr != nil {
				slog.Error("failed to mark dead letter redrive attempt",
					slog.Int64("id", deadLetter.ID), slog.String("error", markErr.Error()))
			}
			continue
		}

		if deleteErr := r.queue.DeleteDeadLetter(ctx, deadLetter.ID); deleteErr != nil {
			// The event was indexed but the row remains; the next pass will
			// replay it, which is safe because handlers are idempotent.
			slog.Error("failed to delete redriven dead letter (will replay next pass)",
				slog.Int64("id", deadLetter.ID), slog.String("error", deleteErr.Error()))
			continue
		}
		redriven++
	}
	return redriven, failed, listed, nil
}
