package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type moderationIdempotencySweeperFake struct {
	called chan time.Time
	err    error
}

func (fake *moderationIdempotencySweeperFake) DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int64, error) {
	select {
	case fake.called <- now:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return 0, fake.err
}

func TestStartModerationIdempotencySweepJobRunsOnStartupAndTickAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	sweeper := &moderationIdempotencySweeperFake{called: make(chan time.Time, 4)}
	started := time.Now()
	startModerationIdempotencySweepJob(ctx, &waitGroup, sweeper, 20*time.Millisecond)
	for call := 0; call < 2; call++ {
		select {
		case sweepTime := <-sweeper.called:
			assert.False(t, sweepTime.Before(started), "sweep must use the current time")
			assert.WithinDuration(t, time.Now(), sweepTime, time.Second)
		case <-time.After(time.Second):
			t.Fatalf("moderation idempotency sweep did not run on %s", []string{"startup", "tick"}[call])
		}
	}
	cancel()
	requireModerationIdempotencySweepStops(t, &waitGroup)
}

func TestStartModerationIdempotencySweepJobContinuesAfterError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	sweeper := &moderationIdempotencySweeperFake{called: make(chan time.Time, 4), err: errors.New("database unavailable")}
	startModerationIdempotencySweepJob(ctx, &waitGroup, sweeper, 20*time.Millisecond)
	for call := 0; call < 2; call++ {
		select {
		case <-sweeper.called:
		case <-time.After(time.Second):
			t.Fatal("a failed sweep must not stop the next tick")
		}
	}
	cancel()
	requireModerationIdempotencySweepStops(t, &waitGroup)
}

func TestStartModerationIdempotencySweepJobGuardsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name     string
		sweeper  moderationIdempotencySweeper
		interval time.Duration
	}{
		{name: "nil sweeper", interval: time.Second},
		{name: "zero interval", sweeper: &moderationIdempotencySweeperFake{called: make(chan time.Time, 1)}},
		{name: "negative interval", sweeper: &moderationIdempotencySweeperFake{called: make(chan time.Time, 1)}, interval: -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			var waitGroup sync.WaitGroup
			startModerationIdempotencySweepJob(t.Context(), &waitGroup, test.sweeper, test.interval)
			requireModerationIdempotencySweepStops(t, &waitGroup)
			if fake, ok := test.sweeper.(*moderationIdempotencySweeperFake); ok {
				assert.Empty(t, fake.called)
			}
		})
	}
}

func requireModerationIdempotencySweepStops(t *testing.T, waitGroup *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("moderation idempotency sweep job did not stop")
	}
}

var _ moderationIdempotencySweeper = (*moderationIdempotencySweeperFake)(nil)
