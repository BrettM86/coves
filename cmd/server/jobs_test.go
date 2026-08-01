package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
)

// The reason recovery lives per cycle rather than around the whole goroutine:
// a job that dies on its first panic takes aggregator token refresh with it,
// and every aggregator silently stops working at the next token expiry — no
// error, no alert, no restart.
func TestRunTicker_SurvivesAPanickingCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var cycles atomic.Int64

	runTicker(ctx, &wg, "panicky", time.Millisecond, func(context.Context) {
		if cycles.Add(1) == 1 {
			panic("first cycle explodes")
		}
	})

	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return cycles.Load() >= 3, nil
	}, testkit.WithDescription("the panicky job to keep cycling after its first cycle panicked"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("cycles completed: %d", cycles.Load())
		}))

	cancel()
	wg.Wait()
}

func TestRunTicker_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var cycles atomic.Int64

	runTicker(ctx, &wg, "counter", time.Millisecond, func(context.Context) {
		cycles.Add(1)
	})

	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return cycles.Load() >= 2, nil
	}, testkit.WithDescription("the counter job to run at least two cycles"))

	cancel()

	// wg.Wait must return: shutdown blocks on it, so a job that ignored
	// cancellation would hang the drain until the shutdown timeout expired.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not exit after context cancellation")
	}

	// No further cycles once the WaitGroup has been released. This is a
	// stays-true claim, and the window is worth several hundred ticks of the
	// 1ms interval — a job that ignored cancellation would be caught on the
	// first poll after the window opens.
	settled := cycles.Load()
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		return cycles.Load() == settled, nil
	}, testkit.WithDescription("the cycle count to stay at %d after the job exited", settled),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("cycles now: %d", cycles.Load())
		}))
}

// Work receives a live, deadline-bounded context derived from the job's own,
// so a cycle is cut short by shutdown rather than blocking the drain.
func TestRunTicker_PassesLiveBoundedContextToWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	type observation struct {
		err         error
		deadline    time.Time
		hasDeadline bool
	}
	observed := make(chan observation, 1)

	runTicker(ctx, &wg, "ctx-check", time.Minute, func(workCtx context.Context) {
		deadline, ok := workCtx.Deadline()
		select {
		case observed <- observation{err: workCtx.Err(), deadline: deadline, hasDeadline: ok}:
		default:
		}
	})

	var got observation
	select {
	case got = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("job never ran its first cycle")
	}

	if got.err != nil {
		t.Errorf("work received an already-cancelled context: %v", got.err)
	}
	// The deadline is what stops a hung cycle from silently killing the job:
	// without it, a blocked cycle means the ticker loop never re-enters its
	// select and the job stops forever with nothing logged.
	if !got.hasDeadline {
		t.Error("work's context has no deadline; a hung cycle would stall the job permanently")
	}

	cancel()
	wg.Wait()
}

// Cancelling the job context must interrupt a cycle that is already running,
// not merely stop the next one from starting.
func TestRunTicker_CancelInterruptsAnInFlightCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	started := make(chan struct{})
	finished := make(chan error, 1)

	runTicker(ctx, &wg, "long-cycle", time.Minute, func(workCtx context.Context) {
		select {
		case started <- struct{}{}:
		default:
			return
		}
		<-workCtx.Done()
		finished <- workCtx.Err()
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job never ran its first cycle")
	}

	cancel()

	select {
	case err := <-finished:
		if err == nil {
			t.Error("in-flight cycle's context was not cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the job did not interrupt the running cycle")
	}

	wg.Wait()
}

// Every job must do a pass at boot. Waiting out a full 30- or 60-minute
// interval means a frequently-restarted deployment never refreshes aggregator
// tokens at all, and a backlog accumulated while the process was down sits
// untouched.
func TestRunTicker_RunsImmediatelyAtStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	ran := make(chan struct{}, 1)

	// An interval far longer than the test's patience: only a startup cycle
	// can satisfy this.
	runTicker(ctx, &wg, "startup-cycle", time.Hour, func(context.Context) {
		select {
		case ran <- struct{}{}:
		default:
		}
	})

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not run a cycle at startup; it waited for the first tick")
	}

	cancel()
	wg.Wait()
}

func TestRunGuarded_ConvertsPanicToReturn(t *testing.T) {
	// The bare call must not propagate the panic to the caller.
	runGuarded(context.Background(), "boom", time.Second, func(context.Context) {
		panic("kaboom")
	})

	ran := false
	runGuarded(context.Background(), "fine", time.Second, func(context.Context) {
		ran = true
	})
	if !ran {
		t.Error("runGuarded did not execute the work function")
	}
}

// Runtime panics must be contained too, not just explicit panic() calls: a
// nil dereference or an out-of-range index inside a job is exactly the kind of
// bug this guard exists to survive.
func TestRunGuarded_ContainsRuntimePanics(t *testing.T) {
	runGuarded(context.Background(), "index-out-of-range", time.Second, func(context.Context) {
		values := []int{1, 2, 3}
		index := len(values) + 1
		_ = values[index]
	})

	runGuarded(context.Background(), "nil-deref", time.Second, func(context.Context) {
		type box struct{ n int }
		var b *box
		_ = b.n
	})
}
