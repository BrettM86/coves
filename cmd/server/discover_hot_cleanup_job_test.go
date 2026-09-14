package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/discover"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type discoverHotStateCleanerFake struct {
	calls               atomic.Int64
	batches             chan int
	firstCallError      error
	waitForCancellation bool
	returned            chan error
	removed             []int64
}

func (f *discoverHotStateCleanerFake) CleanupExpiredDiscoverHotState(ctx context.Context, derivedRowBatchSize int) (int64, error) {
	call := f.calls.Add(1)
	if f.batches != nil {
		f.batches <- derivedRowBatchSize
	}
	if f.waitForCancellation {
		<-ctx.Done()
		if f.returned != nil {
			f.returned <- ctx.Err()
		}
		return 0, ctx.Err()
	}
	if call == 1 && f.firstCallError != nil {
		return 0, f.firstCallError
	}
	if index := int(call - 1); index < len(f.removed) {
		return f.removed[index], nil
	}
	return 1, nil
}

func TestStartDiscoverHotCleanupJob_RunsImmediatelyAndForwardsBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleaner := &discoverHotStateCleanerFake{batches: make(chan int, 1)}
	var waitGroup sync.WaitGroup

	const configuredDerivedRowBatchSize = 7
	startDiscoverHotCleanupJob(ctx, &waitGroup, cleaner, time.Hour, configuredDerivedRowBatchSize)

	select {
	case batch := <-cleaner.batches:
		assert.Equal(t, configuredDerivedRowBatchSize, batch)
	case <-time.After(time.Second):
		t.Fatal("Discover Hot cleanup did not run immediately at startup")
	}

	cancel()
	requireDiscoverHotCleanupJobStops(t, &waitGroup)
}

func TestStartDiscoverHotCleanupJob_DrainsFullBatchesBeforeWaitingForNextTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	const batchSize = 10_000
	cleaner := &discoverHotStateCleanerFake{
		batches: make(chan int, 3),
		removed: []int64{batchSize, batchSize, 37},
	}
	var waitGroup sync.WaitGroup

	startDiscoverHotCleanupJob(ctx, &waitGroup, cleaner, time.Hour, batchSize)

	for call := 1; call <= 3; call++ {
		select {
		case gotBatch := <-cleaner.batches:
			assert.Equalf(t, batchSize, gotBatch, "cleanup call %d", call)
		case <-ctx.Done():
			require.FailNowf(t, "cleanup stopped before draining the current tick",
				"received %d of 3 batches: full batches must be followed immediately so later checkpoint rows are not starved until future ticks", call-1)
		}
	}

	cancel()
	requireDiscoverHotCleanupJobStops(t, &waitGroup)
	assert.Equal(t, int64(3), cleaner.calls.Load(),
		"cleanup must stop draining when a call removes fewer rows than the configured batch")
}

func TestStartDiscoverHotCleanupJob_ReportsErrorAndContinues(t *testing.T) {
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	ctx, cancel := context.WithCancel(context.Background())
	cleaner := &discoverHotStateCleanerFake{firstCallError: errors.New("cleanup database unavailable")}
	var waitGroup sync.WaitGroup

	startDiscoverHotCleanupJob(ctx, &waitGroup, cleaner, time.Millisecond, 2)
	continued := assert.Eventually(t, func() bool {
		return cleaner.calls.Load() >= 2
	}, time.Second, time.Millisecond, "a cleaner error must not stop later cleanup cycles")
	cancel()
	requireDiscoverHotCleanupJobStops(t, &waitGroup)

	assert.Contains(t, logged.String(), "Discover Hot cleanup failed", "the cleaner error must be reported")
	if continued {
		assert.GreaterOrEqual(t, cleaner.calls.Load(), int64(2))
	}
}

func TestStartDiscoverHotCleanupJob_CancellationInterruptsCleanerAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleaner := &discoverHotStateCleanerFake{
		batches:             make(chan int, 1),
		waitForCancellation: true,
		returned:            make(chan error, 1),
	}
	var waitGroup sync.WaitGroup

	startDiscoverHotCleanupJob(ctx, &waitGroup, cleaner, time.Hour, 2)
	select {
	case <-cleaner.batches:
	case <-time.After(time.Second):
		t.Fatal("Discover Hot cleanup never started")
	}

	cancel()
	select {
	case err := <-cleaner.returned:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the in-flight Discover Hot cleanup")
	}
	requireDiscoverHotCleanupJobStops(t, &waitGroup)
}

func requireDiscoverHotCleanupJobStops(t *testing.T, waitGroup *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Discover Hot cleanup job did not stop after cancellation")
	}
}

var _ discover.DiscoverHotStateCleaner = (*discoverHotStateCleanerFake)(nil)
