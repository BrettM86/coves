package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/notifications"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type notificationRetentionSweeperFake struct {
	readCalls                atomic.Int64
	unreadCalls              atomic.Int64
	groupCalls               atomic.Int64
	hiddenCalls              atomic.Int64
	secondReadAfterAllSweeps atomic.Bool
	readResults              []int64
	unreadResults            []int64
	groupResults             []int64
	hiddenResults            []int64
	firstReadError           error
	// interruptSweep names the sweep whose first call runs interrupt, which
	// stands in for the cycle context ending mid-sweep. While interrupt is
	// set, every other call made with a done context returns ctx.Err(), as a
	// database call would.
	interruptSweep string
	interrupt      func(ctx context.Context) (int64, error)
}

func (f *notificationRetentionSweeperFake) interrupted(ctx context.Context, sweep string, call int64) (removed int64, handled bool, err error) {
	switch {
	case f.interrupt == nil:
		return 0, false, nil
	case sweep == f.interruptSweep && call == 1:
		removed, err = f.interrupt(ctx)
		return removed, true, err
	case ctx.Err() != nil:
		return 0, true, ctx.Err()
	}
	return 0, false, nil
}

func (f *notificationRetentionSweeperFake) SweepReadNotifications(ctx context.Context) (int64, error) {
	call := f.readCalls.Add(1)
	if removed, handled, err := f.interrupted(ctx, "read", call); handled {
		return removed, err
	}
	if call == 2 {
		f.secondReadAfterAllSweeps.Store(f.unreadCalls.Load() > 0 && f.groupCalls.Load() > 0 && f.hiddenCalls.Load() > 0)
	}
	if call == 1 && f.firstReadError != nil {
		return 0, f.firstReadError
	}
	if index := int(call - 1); index < len(f.readResults) {
		return f.readResults[index], nil
	}
	return 0, nil
}

func (f *notificationRetentionSweeperFake) SweepUnreadOverflow(ctx context.Context) (int64, error) {
	call := f.unreadCalls.Add(1)
	if removed, handled, err := f.interrupted(ctx, "unread_cap", call); handled {
		return removed, err
	}
	if index := int(call - 1); index < len(f.unreadResults) {
		return f.unreadResults[index], nil
	}
	return 0, nil
}

func (f *notificationRetentionSweeperFake) SweepEmptyUpvoteGroups(ctx context.Context) (int64, error) {
	call := f.groupCalls.Add(1)
	if removed, handled, err := f.interrupted(ctx, "empty_groups", call); handled {
		return removed, err
	}
	if index := int(call - 1); index < len(f.groupResults) {
		return f.groupResults[index], nil
	}
	return 0, nil
}

func (f *notificationRetentionSweeperFake) SweepHiddenReferenceNotifications(ctx context.Context) (int64, error) {
	call := f.hiddenCalls.Add(1)
	if removed, handled, err := f.interrupted(ctx, "hidden_references", call); handled {
		return removed, err
	}
	if index := int(call - 1); index < len(f.hiddenResults) {
		return f.hiddenResults[index], nil
	}
	return 0, nil
}

func TestApplication_WiresNotificationRetention(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()

	require.NotNil(t, app.notificationRetentionSweeper, "buildRepositories must wire the notification retention sweeper")
}

func TestNotificationRetentionInterval_IsHourly(t *testing.T) {
	require.Equal(t, time.Hour, notificationRetentionInterval)
}

func TestStartNotificationRetentionJob_DrainsFullBatchesBeforeNextTick(t *testing.T) {
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	sweeper := &notificationRetentionSweeperFake{
		readResults:   []int64{10000, 10000, 7},
		unreadResults: []int64{10000, 0},
		groupResults:  []int64{3},
		hiddenResults: []int64{10000, 4},
	}
	startNotificationRetentionJob(ctx, &waitGroup, sweeper, time.Hour)
	t.Cleanup(func() {
		cancel()
		requireDiscoverHotCleanupJobStops(t, &waitGroup)
	})

	testkit.WaitFor(t, time.Second, func() (bool, error) {
		return sweeper.readCalls.Load() == 3 && sweeper.unreadCalls.Load() == 2 && sweeper.groupCalls.Load() == 1 &&
			sweeper.hiddenCalls.Load() == 2, nil
	}, testkit.WithPollInterval(time.Millisecond),
		testkit.WithDescription("the startup retention cycle to drain the read, unread and hidden-reference batches and sweep groups"))

	testkit.Holds(t, 150*time.Millisecond, func() (bool, error) {
		return sweeper.readCalls.Load() == 3 && sweeper.unreadCalls.Load() == 2 && sweeper.groupCalls.Load() == 1 &&
			sweeper.hiddenCalls.Load() == 2, nil
	}, testkit.WithPollInterval(5*time.Millisecond),
		testkit.WithDescription("the retention sweeps to stop after their short batches until the next tick"))

	cancel()
	requireDiscoverHotCleanupJobStops(t, &waitGroup)
	assert.Regexp(t, `level=INFO .*sweep=hidden_references removed=10004`, logged.String(),
		"the hidden-reference sweep must report its drained total under its own name")
}

func TestStartNotificationRetentionJob_LogsReadErrorContinuesAndRetries(t *testing.T) {
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	sweeper := &notificationRetentionSweeperFake{firstReadError: errors.New("retention read sweep sentinel failure")}
	startNotificationRetentionJob(ctx, &waitGroup, sweeper, time.Millisecond)
	t.Cleanup(func() {
		cancel()
		requireDiscoverHotCleanupJobStops(t, &waitGroup)
	})

	assert.Eventually(t, func() bool {
		return sweeper.readCalls.Load() >= 2
	}, time.Second, time.Millisecond, "a later tick must retry the read sweep")
	cancel()
	requireDiscoverHotCleanupJobStops(t, &waitGroup)

	assert.True(t, sweeper.secondReadAfterAllSweeps.Load(),
		"the unread, group and hidden-reference sweeps must run despite the first read error, before the next read cycle")
	assert.GreaterOrEqual(t, sweeper.unreadCalls.Load(), int64(1))
	assert.GreaterOrEqual(t, sweeper.groupCalls.Load(), int64(1))
	assert.GreaterOrEqual(t, sweeper.hiddenCalls.Load(), int64(1))

	var errorRecord string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "retention read sweep sentinel failure") {
			errorRecord = line
			break
		}
	}
	require.NotEmpty(t, errorRecord, "an ERROR log record must carry the read sweep error")
	assert.Regexp(t, `(?:^|\s)sweep=\S+`, errorRecord, "the ERROR record must identify the failed sweep")
}

// expiringContext stands in for a cycle whose deadline passes mid-sweep:
// after expire it reports context.DeadlineExceeded, without the test
// depending on how long the sweep took to start.
type expiringContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newExpiringContext() *expiringContext {
	return &expiringContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *expiringContext) expire() { c.once.Do(func() { close(c.done) }) }

func TestStartNotificationRetentionJob_StopsCycleWhenContextEnds(t *testing.T) {
	cases := []struct {
		name                            string
		interruptSweep                  string
		deadline                        bool
		fullBatch                       bool
		wantRead, wantUnread, wantGroup int64
		wantHidden                      int64
		skipped                         []string
	}{
		{name: "cancelled during read sweep", interruptSweep: "read",
			wantRead: 1, wantHidden: 0, skipped: []string{"unread_cap", "empty_groups", "hidden_references"}},
		{name: "deadline exceeded during read sweep", interruptSweep: "read", deadline: true,
			wantRead: 1, wantHidden: 0, skipped: []string{"unread_cap", "empty_groups", "hidden_references"}},
		{name: "cancelled during a full read batch", interruptSweep: "read", fullBatch: true,
			wantRead: 1, wantHidden: 0, skipped: []string{"unread_cap", "empty_groups", "hidden_references"}},
		{name: "deadline exceeded during unread_cap sweep", interruptSweep: "unread_cap", deadline: true,
			wantRead: 1, wantUnread: 1, wantHidden: 0, skipped: []string{"empty_groups", "hidden_references"}},
		{name: "deadline exceeded during empty_groups sweep", interruptSweep: "empty_groups", deadline: true,
			wantRead: 1, wantUnread: 1, wantGroup: 1, wantHidden: 0, skipped: []string{"hidden_references"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })

			var ctx context.Context
			var end func()
			if tc.deadline {
				expiring := newExpiringContext()
				ctx, end = expiring, expiring.expire
			} else {
				ctx, end = context.WithCancel(context.Background())
			}
			sweeper := &notificationRetentionSweeperFake{
				interruptSweep: tc.interruptSweep,
				interrupt: func(cycleCtx context.Context) (int64, error) {
					end()
					<-cycleCtx.Done()
					if tc.fullBatch {
						return notifications.RetentionBatchSize, nil
					}
					return 0, cycleCtx.Err()
				},
			}
			var waitGroup sync.WaitGroup
			// An hourly interval leaves only the startup cycle; ending its
			// context also stops the job, so the wait below means that cycle
			// has completed.
			startNotificationRetentionJob(ctx, &waitGroup, sweeper, time.Hour)
			t.Cleanup(end)
			requireDiscoverHotCleanupJobStops(t, &waitGroup)

			assert.Equal(t, []int64{tc.wantRead, tc.wantUnread, tc.wantGroup, tc.wantHidden},
				[]int64{sweeper.readCalls.Load(), sweeper.unreadCalls.Load(), sweeper.groupCalls.Load(), sweeper.hiddenCalls.Load()},
				"no sweep call may follow the end of the cycle context (read, unread_cap, empty_groups, hidden_references calls)")
			for _, line := range strings.Split(logged.String(), "\n") {
				for _, sweep := range tc.skipped {
					assert.False(t, strings.Contains(line, "level=ERROR") && strings.Contains(line, "sweep="+sweep),
						"a sweep skipped after the cycle context ended must not log a failure: %s", line)
				}
			}
		})
	}
}

var _ notifications.RetentionSweeper = (*notificationRetentionSweeperFake)(nil)
