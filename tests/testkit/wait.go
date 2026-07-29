package testkit

import (
	"fmt"
	"strings"
	"time"
)

// Waiting primitives. These are the only ones tests may use: time.Sleep in a
// *_test.go file is counted as a violation by scripts/test-audit.sh and becomes
// a hard CI failure at the end of the migration.
//
// A sleep encodes a guess about how long something takes, and it is wrong in
// both directions at once — too short on a loaded CI machine (flake), too long
// on an idle laptop (slow suite). Worse, when a sleep-based test does fail, its
// failure message says nothing about what the system was doing.
//
// WaitFor and Holds are the two shapes that actually come up:
//
//	WaitFor — eventually true. "the firehose delivered the record."
//	Holds   — stays true. "the delete stayed deleted."
//
// Holds is not redundant. An eventually-check cannot catch a record that is
// deleted and then resurrected by a replayed event: the check passes on the
// first poll and never looks again.

// DefaultPollInterval is how often a probe is re-evaluated. 100ms is fast
// enough that a passing test is not noticeably slowed by the granularity, and
// slow enough not to hammer a serving endpoint.
const DefaultPollInterval = 100 * time.Millisecond

// Probe reports whether the awaited condition currently holds.
//
// A non-nil error is TERMINAL: the wait fails immediately with that error
// attached. This is deliberate and it is the difference between a useful
// failure and a useless one. A probe that polls an XRPC endpoint and gets a 401
// will never succeed by being asked again — retrying it for 30 seconds converts
// "the session was rejected" into "timed out waiting for the post to appear",
// which is exactly the failure message an agent cannot debug. Return the error;
// do not swallow it into `false`.
//
// Conversely, a condition that is merely not-yet-true — no rows, HTTP 404 for a
// record still in flight — is `false, nil`.
//
// Probes should be cheap and side-effect free: they run every poll interval.
//
// To report what the system looked like when a wait times out, close over the
// observation and render it with WithDiagnostics:
//
//	var last []Post
//	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
//	        var err error
//	        last, err = client.Feed(ctx)
//	        if err != nil {
//	                return false, err // terminal: a broken endpoint is not a delay
//	        }
//	        return len(last) == 3, nil
//	}, testkit.WithDescription("3 posts in the community feed"),
//	   testkit.WithDiagnostics(func() string { return fmt.Sprintf("last feed: %v", last) }))
type Probe func() (done bool, err error)

type waitConfig struct {
	interval    time.Duration
	description string
	diagnostics func() string
}

// WaitOption customises WaitFor and Holds.
type WaitOption func(*waitConfig)

// WithPollInterval overrides DefaultPollInterval.
func WithPollInterval(d time.Duration) WaitOption {
	return func(c *waitConfig) {
		if d > 0 {
			c.interval = d
		}
	}
}

// WithDescription names the condition being awaited, so the failure message
// reads as a statement about the system rather than about the harness.
func WithDescription(format string, args ...any) WaitOption {
	return func(c *waitConfig) { c.description = fmt.Sprintf(format, args...) }
}

// WithDiagnostics registers a function rendered into the failure message, and
// only into the failure message — it is never called on the success path.
//
// This is the hook the pipeline tier uses to attach consumer health (cursor
// positions, dead-letter counts) to a timeout, so "the record never appeared"
// arrives with the reason attached.
func WithDiagnostics(fn func() string) WaitOption {
	return func(c *waitConfig) { c.diagnostics = fn }
}

func newWaitConfig(opts []WaitOption) waitConfig {
	cfg := waitConfig{interval: DefaultPollInterval}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (c waitConfig) subject() string {
	if c.description == "" {
		return "condition"
	}
	return c.description
}

// render assembles a failure message with the diagnostics hook appended.
func (c waitConfig) render(msg string) string {
	var b strings.Builder
	b.WriteString(msg)
	if c.diagnostics != nil {
		if diag := c.diagnostics(); diag != "" {
			b.WriteString("\n  diagnostics: ")
			b.WriteString(strings.ReplaceAll(diag, "\n", "\n    "))
		}
	}
	return b.String()
}

// WaitFor polls probe until it reports true, failing the test if that has not
// happened within timeout or if the probe returns an error.
//
// The last poll happens AT the deadline, not one interval before it. Sleeping a
// full interval and then giving up would make the effective timeout
// `timeout - interval`, so a condition that became true at 4.95s would be
// reported as never satisfied by a WaitFor(t, 5*time.Second, …) — a flake whose
// message actively misleads.
func WaitFor(t TestingT, timeout time.Duration, probe Probe, opts ...WaitOption) {
	t.Helper()
	cfg := newWaitConfig(opts)

	start := time.Now()
	deadline := start.Add(timeout)
	polls := 0

	for {
		polls++
		done, err := probe()
		if err != nil {
			t.Fatalf("%s", cfg.render(fmt.Sprintf(
				"waiting for %s: probe failed after %s (poll %d): %v",
				cfg.subject(), time.Since(start).Round(time.Millisecond), polls, err)))
			return
		}
		if done {
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("%s", cfg.render(fmt.Sprintf(
				"timed out after %s (limit %s) waiting for %s: still false after %d polls",
				time.Since(start).Round(time.Millisecond), timeout, cfg.subject(), polls)))
			return
		}
		// Clamped, so the final sleep lands exactly on the deadline rather than
		// past it.
		time.Sleep(min(cfg.interval, remaining))
	}
}

// Holds asserts that probe is true now and remains true for the whole window.
//
// Use it for negative and steady-state claims — a deleted record staying
// deleted, a vote count staying at one after a duplicate event — where an
// eventually-check would pass on its first poll and never notice the system
// changing its mind a moment later.
//
// The whole window is observed: the last poll is at `start + window`, so
// Holds(t, 250*time.Millisecond, …) really does watch for 250ms. Stopping an
// interval early would silently shorten every stays-true assertion in the
// suite by up to 100ms, which is exactly the span a resurrection-by-replay
// takes to arrive.
func Holds(t TestingT, window time.Duration, probe Probe, opts ...WaitOption) {
	t.Helper()
	cfg := newWaitConfig(opts)

	start := time.Now()
	end := start.Add(window)
	polls := 0

	for {
		polls++
		done, err := probe()
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s", cfg.render(fmt.Sprintf(
				"holding %s: probe failed after %s of a %s window (poll %d): %v",
				cfg.subject(), elapsed.Round(time.Millisecond), window, polls, err)))
			return
		}
		if !done {
			if polls == 1 {
				t.Fatalf("%s", cfg.render(fmt.Sprintf(
					"%s was already false at the start of the %s window",
					cfg.subject(), window)))
				return
			}
			t.Fatalf("%s", cfg.render(fmt.Sprintf(
				"%s stopped holding after %s of a %s window (poll %d)",
				cfg.subject(), elapsed.Round(time.Millisecond), window, polls)))
			return
		}

		remaining := time.Until(end)
		if remaining <= 0 {
			return
		}
		// Clamped: a window shorter than one poll interval is still observed
		// end to end, with a probe at 0 and a probe at the window's close.
		time.Sleep(min(cfg.interval, remaining))
	}
}
