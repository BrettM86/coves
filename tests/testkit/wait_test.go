package testkit

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitFor_ReturnsOnceTheProbeIsTrue(t *testing.T) {
	var polls atomic.Int32

	start := time.Now()
	WaitFor(t, 5*time.Second, func() (bool, error) {
		return polls.Add(1) >= 3, nil
	}, WithPollInterval(20*time.Millisecond))
	elapsed := time.Since(start)

	assert.EqualValues(t, 3, polls.Load(), "should stop polling as soon as the probe is true")
	// Two intervals of waiting between three polls: enough to prove it really
	// polled rather than getting lucky on the first call.
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second, "should not have waited out the timeout")
}

func TestWaitFor_ProbeTrueOnFirstCallDoesNotWait(t *testing.T) {
	start := time.Now()
	WaitFor(t, 5*time.Second, func() (bool, error) { return true, nil })
	assert.Less(t, time.Since(start), 50*time.Millisecond,
		"an already-true condition must not cost a poll interval")
}

func TestWaitFor_TimesOutWithTheLastObservation(t *testing.T) {
	f := &fakeT{}
	observed := "http 404: post not indexed"

	var polls atomic.Int32
	start := time.Now()
	runIsolated(func() {
		WaitFor(f, 300*time.Millisecond, func() (bool, error) {
			polls.Add(1)
			return false, nil
		},
			WithPollInterval(50*time.Millisecond),
			WithDescription("the post to appear in the feed"),
			WithDiagnostics(func() string { return observed }))
	})
	elapsed := time.Since(start)

	require.True(t, f.failed(), "a probe that never becomes true must fail the test")
	msg := f.message()
	assert.Contains(t, msg, "limit 300ms")
	assert.Contains(t, msg, "the post to appear in the feed")
	assert.Contains(t, msg, observed, "the last observation must reach the failure message")
	assert.Contains(t, msg, fmt.Sprintf("%d polls", polls.Load()))
	assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond,
		"the full timeout must elapse before giving up, not timeout-minus-one-interval")
	assert.Less(t, elapsed, 2*time.Second)
}

func TestWaitFor_SucceedsWhenTheConditionArrivesJustBeforeTheDeadline(t *testing.T) {
	// The off-by-one-interval bug, stated as a test. With a 300ms timeout and a
	// 100ms interval, a condition that becomes true at ~280ms is inside the
	// stated timeout; a loop that stops polling one interval early would report
	// it as never satisfied, and the failure message would be a lie.
	start := time.Now()
	WaitFor(t, 300*time.Millisecond, func() (bool, error) {
		return time.Since(start) >= 280*time.Millisecond, nil
	}, WithPollInterval(100*time.Millisecond))

	assert.GreaterOrEqual(t, time.Since(start), 280*time.Millisecond)
}

func TestWaitFor_ProbeErrorIsTerminal(t *testing.T) {
	f := &fakeT{}
	boom := errors.New("401 unauthorized")

	var polls atomic.Int32
	start := time.Now()
	runIsolated(func() {
		WaitFor(f, 10*time.Second, func() (bool, error) {
			polls.Add(1)
			return false, boom
		}, WithDescription("the vote count to settle"))
	})
	elapsed := time.Since(start)

	require.True(t, f.failed())
	assert.EqualValues(t, 1, polls.Load(), "a terminal error must not be retried")
	// The point of terminal errors: an unauthorised request fails in
	// milliseconds with the status attached instead of burning the whole
	// timeout and reporting an opaque "never became true".
	assert.Less(t, elapsed, time.Second, "must fail fast rather than wait out the timeout")
	msg := f.message()
	assert.Contains(t, msg, "401 unauthorized")
	assert.Contains(t, msg, "the vote count to settle")
}

func TestWaitFor_DiagnosticsAreNotEvaluatedOnSuccess(t *testing.T) {
	var called atomic.Bool
	WaitFor(t, time.Second, func() (bool, error) { return true, nil },
		WithDiagnostics(func() string {
			called.Store(true)
			return "should never be rendered"
		}))
	assert.False(t, called.Load(),
		"diagnostics may be expensive (an extra HTTP call); they belong on the failure path only")
}

func TestWaitFor_DefaultDescription(t *testing.T) {
	f := &fakeT{}
	runIsolated(func() {
		WaitFor(f, 50*time.Millisecond, func() (bool, error) { return false, nil },
			WithPollInterval(20*time.Millisecond))
	})
	require.True(t, f.failed())
	assert.Contains(t, f.message(), "waiting for condition")
}

func TestHolds_PassesForAStableWindow(t *testing.T) {
	var polls atomic.Int32

	start := time.Now()
	Holds(t, 250*time.Millisecond, func() (bool, error) {
		polls.Add(1)
		return true, nil
	}, WithPollInterval(50*time.Millisecond))
	elapsed := time.Since(start)

	// The WHOLE window, not window-minus-one-interval. An earlier version
	// stopped an interval early and this assertion tolerated it, which meant
	// every stays-true assertion in the suite was quietly up to 100ms shorter
	// than it claimed — about the span a resurrection-by-replay takes to land.
	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "must observe the whole window")
	assert.Greater(t, polls.Load(), int32(1), "must poll repeatedly, not just once")
}

func TestHolds_ObservesAWindowShorterThanThePollInterval(t *testing.T) {
	var polls atomic.Int32

	start := time.Now()
	Holds(t, 40*time.Millisecond, func() (bool, error) {
		polls.Add(1)
		return true, nil
	}, WithPollInterval(500*time.Millisecond))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
	assert.Less(t, elapsed, 400*time.Millisecond,
		"a short window must not be rounded up to a poll interval")
	assert.EqualValues(t, 2, polls.Load(), "one probe at the start, one at the close")
}

func TestHolds_FailsWhenTheConditionFlipsMidWindow(t *testing.T) {
	f := &fakeT{}
	start := time.Now()

	runIsolated(func() {
		Holds(f, 2*time.Second, func() (bool, error) {
			// The resurrection case: true for a moment, then not. An
			// eventually-check would have passed on its first poll.
			return time.Since(start) < 150*time.Millisecond, nil
		},
			WithPollInterval(25*time.Millisecond),
			WithDescription("the deleted post to stay deleted"))
	})

	require.True(t, f.failed())
	msg := f.message()
	assert.Contains(t, msg, "the deleted post to stay deleted")
	assert.Contains(t, msg, "stopped holding after")
	assert.Contains(t, msg, "2s window")
	assert.Less(t, time.Since(start), time.Second, "should fail at the flip, not at the end of the window")
}

func TestHolds_FailsWhenFalseAtTheStart(t *testing.T) {
	f := &fakeT{}
	runIsolated(func() {
		Holds(f, time.Second, func() (bool, error) { return false, nil },
			WithDescription("the row count to stay at 1"))
	})
	require.True(t, f.failed())
	msg := f.message()
	assert.Contains(t, msg, "already false at the start")
	assert.NotContains(t, msg, "stopped holding",
		"a condition that was never true is a different failure from one that stopped being true")
}

func TestHolds_ProbeErrorIsTerminal(t *testing.T) {
	f := &fakeT{}
	var polls atomic.Int32
	runIsolated(func() {
		Holds(f, 10*time.Second, func() (bool, error) {
			polls.Add(1)
			return true, errors.New("500 internal server error")
		}, WithPollInterval(20*time.Millisecond))
	})
	require.True(t, f.failed())
	assert.EqualValues(t, 1, polls.Load())
	assert.Contains(t, f.message(), "500 internal server error")
}

func TestHolds_RendersDiagnostics(t *testing.T) {
	f := &fakeT{}
	runIsolated(func() {
		Holds(f, 100*time.Millisecond, func() (bool, error) { return false, nil },
			WithDiagnostics(func() string { return "dead letters: 3\ncursor: 172" }))
	})
	require.True(t, f.failed())
	msg := f.message()
	assert.Contains(t, msg, "diagnostics:")
	assert.Contains(t, msg, "dead letters: 3")
	assert.True(t, strings.Contains(msg, "cursor: 172"))
}
