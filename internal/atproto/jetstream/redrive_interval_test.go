package jetstream

import (
	"context"
	"testing"
	"time"
)

// intervalProbeQueue is a minimal DeadLetterQueue whose ListRetryable signals
// each invocation on a buffered channel, so a test can observe how many
// redrive passes DeadLetterRedriver.Run has started without inferring it
// from wall-clock sleeps. Every other method is a trivial no-op: this queue
// exists only to count passes.
type intervalProbeQueue struct {
	passes chan struct{}
}

func newIntervalProbeQueue() *intervalProbeQueue {
	return &intervalProbeQueue{passes: make(chan struct{}, 16)}
}

func (q *intervalProbeQueue) AddDeadLetter(context.Context, string, int64, []byte, string, int) error {
	return nil
}

func (q *intervalProbeQueue) ListRetryable(_ context.Context, _ string, _, _ int) ([]DeadLetterEvent, error) {
	q.passes <- struct{}{}
	return nil, nil
}

func (q *intervalProbeQueue) DeleteDeadLetter(context.Context, int64) error { return nil }

func (q *intervalProbeQueue) MarkRedriveAttempt(context.Context, int64, string) error { return nil }

func (q *intervalProbeQueue) RetireDeadLetter(context.Context, int64, string) error { return nil }

func (q *intervalProbeQueue) CountDeadLetters(context.Context) (map[string]int64, error) {
	return nil, nil
}

// TestDeadLetterRedriver_HonoursConfiguredInterval proves that
// WithRedriveInterval actually controls Run's redrive cadence, rather than
// being stored in a field Run never reads.
//
// Run always fires an immediate boot pass, so the first receive below
// passes today regardless of the bug. The second receive is the one that
// exposes it: with a 10ms configured interval, a second pass must start
// well within 2 seconds. Today it can't — Run ticks on a hardcoded
// 5-minute interval no matter what WithRedriveInterval was given.
func TestDeadLetterRedriver_HonoursConfiguredInterval(t *testing.T) {
	queue := newIntervalProbeQueue()
	handlers := map[string]EventHandler{"test-consumer": newFakeEventHandler()}
	redriver := NewDeadLetterRedriver(queue, handlers, WithRedriveInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go redriver.Run(ctx)

	select {
	case <-queue.passes:
		// boot pass
	case <-time.After(2 * time.Second):
		t.Fatal("boot pass never started; DeadLetterRedriver.Run did not call ListRetryable at all")
	}

	select {
	case <-queue.passes:
		// second pass, driven by the configured 10ms interval
	case <-time.After(2 * time.Second):
		t.Fatal("second redrive pass did not start within 2s of a 10ms configured interval; " +
			"WithRedriveInterval's value is stored in configuredInterval but Run still ticks " +
			"on its hardcoded 5-minute interval")
	}
}
