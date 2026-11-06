package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountStatusRecheckerFake reports each RecheckDue limit on called. When
// blockUntilCancelled is set it then holds the call until its context ends,
// standing in for a pass whose PDS confirmation is in flight at shutdown.
type accountStatusRecheckerFake struct {
	called              chan int
	err                 error
	blockUntilCancelled bool
}

func (fake *accountStatusRecheckerFake) RecheckDue(ctx context.Context, limit int) (accountstatus.RecheckReport, error) {
	select {
	case fake.called <- limit:
	case <-ctx.Done():
		return accountstatus.RecheckReport{}, ctx.Err()
	}
	if fake.blockUntilCancelled {
		<-ctx.Done()
		return accountstatus.RecheckReport{}, ctx.Err()
	}
	return accountstatus.RecheckReport{}, fake.err
}

func TestStartAccountStatusRecheckJobRunsOnStartupAndTickAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	rechecker := &accountStatusRecheckerFake{called: make(chan int, 4)}
	startAccountStatusRecheckJob(ctx, &waitGroup, rechecker, 20*time.Millisecond)
	for call := 0; call < 2; call++ {
		select {
		case limit := <-rechecker.called:
			assert.Equal(t, 50, limit, "each pass asks for one batch of due rechecks")
		case <-time.After(time.Second):
			t.Fatalf("account status recheck did not run on %s", []string{"startup", "tick"}[call])
		}
	}
	cancel()
	requireAccountStatusRecheckStops(t, &waitGroup)
}

func TestStartAccountStatusRecheckJobContinuesAfterError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	rechecker := &accountStatusRecheckerFake{called: make(chan int, 4), err: errors.New("database unavailable")}
	startAccountStatusRecheckJob(ctx, &waitGroup, rechecker, 20*time.Millisecond)
	for call := 0; call < 2; call++ {
		select {
		case <-rechecker.called:
		case <-time.After(time.Second):
			t.Fatal("a failed recheck pass must not stop the next tick")
		}
	}
	cancel()
	requireAccountStatusRecheckStops(t, &waitGroup)
}

func TestStartAccountStatusRecheckJobStopsWhenCancelledDuringAPass(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	rechecker := &accountStatusRecheckerFake{called: make(chan int, 1), blockUntilCancelled: true}
	startAccountStatusRecheckJob(ctx, &waitGroup, rechecker, time.Hour)
	select {
	case <-rechecker.called:
	case <-time.After(time.Second):
		t.Fatal("account status recheck did not run on startup")
	}
	cancel()
	requireAccountStatusRecheckStops(t, &waitGroup)
}

func TestStartAccountStatusRecheckJobGuardsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name      string
		rechecker accountStatusRechecker
		interval  time.Duration
	}{
		{name: "nil rechecker", interval: time.Second},
		{name: "zero interval", rechecker: &accountStatusRecheckerFake{called: make(chan int, 1)}},
		{name: "negative interval", rechecker: &accountStatusRecheckerFake{called: make(chan int, 1)}, interval: -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			var waitGroup sync.WaitGroup
			startAccountStatusRecheckJob(t.Context(), &waitGroup, test.rechecker, test.interval)
			requireAccountStatusRecheckStops(t, &waitGroup)
			if fake, ok := test.rechecker.(*accountStatusRecheckerFake); ok {
				assert.Empty(t, fake.called)
			}
		})
	}
}

func requireAccountStatusRecheckStops(t *testing.T, waitGroup *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("account status recheck job did not stop")
	}
}

var _ accountStatusRechecker = (*accountStatusRecheckerFake)(nil)

// accountStatusRecheckScript runs first on the job's first pass and answers
// every later pass with an empty report. entered closes when the first pass
// starts and later when the second starts: runGuarded runs passes one at a
// time, so by then the first pass has returned and logged.
type accountStatusRecheckScript struct {
	first   func(ctx context.Context) (accountstatus.RecheckReport, error)
	entered chan struct{}
	later   chan struct{}
	calls   atomic.Int32
}

func newAccountStatusRecheckScript(first func(ctx context.Context) (accountstatus.RecheckReport, error)) *accountStatusRecheckScript {
	return &accountStatusRecheckScript{first: first, entered: make(chan struct{}), later: make(chan struct{})}
}

func (script *accountStatusRecheckScript) RecheckDue(ctx context.Context, _ int) (accountstatus.RecheckReport, error) {
	switch script.calls.Add(1) {
	case 1:
		close(script.entered)
		return script.first(ctx)
	case 2:
		close(script.later)
	}
	return accountstatus.RecheckReport{}, nil
}

// errCancelledStatement is the error lib/pq returns when the context of an
// in-flight query ends; it wraps no context error.
var errCancelledStatement = errors.New("pq: canceling statement due to user request")

// partialRecheckReport is what a pass cut short reports for the accounts it
// finished.
var partialRecheckReport = accountstatus.RecheckReport{Due: 3, Confirmed: 1, Failed: 1}

// captureDefaultLogs replaces slog.Default with a capturing handler until
// the test ends. Tests using it must not run in parallel.
func captureDefaultLogs(t *testing.T) *cdnBootLogHandler {
	t.Helper()
	previous := slog.Default()
	handler := &cdnBootLogHandler{}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return handler
}

func waitForAccountStatusRecheckPass(t *testing.T, passStarted <-chan struct{}, which string) {
	t.Helper()
	select {
	case <-passStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("account status recheck %s pass did not start", which)
	}
}

func recordsAtLevel(records []slog.Record, level slog.Level) []slog.Record {
	var matched []slog.Record
	for _, record := range records {
		if record.Level == level {
			matched = append(matched, record)
		}
	}
	return matched
}

func recordAttributes(record slog.Record) map[string]any {
	attributes := make(map[string]any)
	record.Attrs(func(attribute slog.Attr) bool {
		attributes[attribute.Key] = attribute.Value.Any()
		return true
	})
	return attributes
}

// hasRecheckCounts reports whether record carries the counts of
// partialRecheckReport.
func hasRecheckCounts(record slog.Record) bool {
	attributes := recordAttributes(record)
	return attributes["due"] == int64(3) && attributes["confirmed"] == int64(1) && attributes["failed"] == int64(1)
}

// Not parallel: this test replaces slog.Default while the job runs.
func TestStartAccountStatusRecheckJobLogsAPassCutAtTheCycleDeadline(t *testing.T) {
	logs := captureDefaultLogs(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	script := newAccountStatusRecheckScript(func(ctx context.Context) (accountstatus.RecheckReport, error) {
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("first pass ended by %v, want the cycle deadline", ctx.Err())
		}
		return partialRecheckReport, errCancelledStatement
	})
	// runGuarded gives each pass a deadline of one interval.
	startAccountStatusRecheckJob(ctx, &waitGroup, script, 50*time.Millisecond)
	waitForAccountStatusRecheckPass(t, script.later, "second")
	cancel()
	requireAccountStatusRecheckStops(t, &waitGroup)

	records := logs.snapshot()
	assert.Empty(t, recordsAtLevel(records, slog.LevelError), "a pass cut at the cycle deadline is not an error")
	var withCounts []slog.Record
	for _, record := range recordsAtLevel(records, slog.LevelInfo) {
		if hasRecheckCounts(record) {
			withCounts = append(withCounts, record)
		}
	}
	assert.Len(t, withCounts, 1, "want one Info record with due=3 confirmed=1 failed=1; records: %v", records)
}

// Not parallel: this test replaces slog.Default while the job runs.
func TestStartAccountStatusRecheckJobShutdownDuringAPassIsNotAnError(t *testing.T) {
	logs := captureDefaultLogs(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	script := newAccountStatusRecheckScript(func(ctx context.Context) (accountstatus.RecheckReport, error) {
		<-ctx.Done()
		return partialRecheckReport, errCancelledStatement
	})
	startAccountStatusRecheckJob(ctx, &waitGroup, script, time.Hour)
	waitForAccountStatusRecheckPass(t, script.entered, "first")
	cancel()
	requireAccountStatusRecheckStops(t, &waitGroup)

	assert.Empty(t, recordsAtLevel(logs.snapshot(), slog.LevelError), "a pass ended by shutdown is not an error")
}

// Not parallel: this test replaces slog.Default while the job runs.
func TestStartAccountStatusRecheckJobLogsAFailureUnderALiveContext(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "context canceled from below", err: context.Canceled},
		{name: "plain error", err: errors.New("database unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureDefaultLogs(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waitGroup sync.WaitGroup
			script := newAccountStatusRecheckScript(func(ctx context.Context) (accountstatus.RecheckReport, error) {
				if ctx.Err() != nil {
					t.Errorf("first pass context already ended: %v", ctx.Err())
				}
				return partialRecheckReport, test.err
			})
			startAccountStatusRecheckJob(ctx, &waitGroup, script, 50*time.Millisecond)
			waitForAccountStatusRecheckPass(t, script.later, "second")
			cancel()
			requireAccountStatusRecheckStops(t, &waitGroup)

			errorRecords := recordsAtLevel(logs.snapshot(), slog.LevelError)
			require.Len(t, errorRecords, 1, "a failed pass under a live context is an error")
			assert.True(t, hasRecheckCounts(errorRecords[0]), "the error record carries the pass counts: %v", recordAttributes(errorRecords[0]))
		})
	}
}
