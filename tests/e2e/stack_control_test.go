//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
)

// The container half of the stack control channel. Its host half — the verb
// list, the reasoning behind the shape, and what it deliberately cannot do —
// lives in scripts/lib/ci-stack.sh under "The stack control channel"; read that
// first, because this file is only the transport.
//
// THE CONSTRAINT THIS EXISTS TO SOLVE
//
// docs/TEST_ARCHITECTURE.md §3.4c's reliability scenarios are about the
// AppView's ingestion machinery under interruption: a persisted cursor is only
// observable across a restart, a rewound replay only happens after a
// reconnection, a second Jetstream feed only exists if the process is
// configured with one. All three are host actions. The tier, by design, runs
// inside the stack (§3.5) in a container with no Docker socket and no route off
// the internal network — so it asks.
//
// A request is a file containing one verb; the answer is a file containing an
// exit status and whatever the host command printed. That is the entire
// protocol. It is synchronous and serial, which costs nothing here: the tier
// runs one test at a time (§3.4 rule 2) and every verb is a container operation
// measured in seconds.
//
// WHEN IT IS ABSENT
//
// `make test-e2e-dev` grades a host-run AppView with no CI stack beside it, so
// there is no watcher and nothing to restart. That target excludes this suite
// by name rather than leaving it to fail; requireStackControl explains as much
// when a scenario reaches this code with no watcher listening, because a
// reliability suite that silently degrades into an assertion-free pass is worse
// than one that is missing.

// defaultStackControlDir is where requests are written when nothing says
// otherwise. The compose runner always sets COVES_STACK_CONTROL_DIR, so this is
// only reached by a hand-run container.
//
// The directory is suffixed with the compose project name, because two stacks
// from one checkout must not share a channel (COVES_CI_PROJECT exists to allow
// exactly that, and scripts/lib/ci-stack.sh now derives a distinct project per
// worktree by default). This constant carries the hand-run FALLBACK project,
// matching docker-compose.ci.yml's own `${COVES_CI_PROJECT:-coves-ci}`; under
// any real run the environment variable is the only correct answer, which is
// why the spellings of this path are worth keeping in step.
const defaultStackControlDir = "/src/.ci-out/stack-control-coves-ci"

// stackControlTimeout bounds one verb, end to end: the watcher's poll latency,
// the container operation itself, and the response's trip back through the bind
// mount. Recreating the AppView pulls no images and runs no migrations that are
// not already applied, so this is roughly ten times what any verb has been
// measured to need — a generous bound on an operation that is not what any
// scenario is timing.
const stackControlTimeout = 90 * time.Second

// stackControlPoll is how often the response file is looked for. Fast enough
// that the channel adds no perceptible latency to a multi-second container
// operation; slow enough not to stat a bind mount in a tight loop.
const stackControlPoll = 100 * time.Millisecond

// stackControlSeq numbers requests within this process, so two scenarios (or a
// scenario and its cleanup) can never collide on a filename. The run prefix in
// the name keeps a stale file from a previous run out of the way even before
// the watcher's start-up wipe.
var stackControlSeq atomic.Uint64

// stackControl is a handle on the host watcher.
type stackControl struct {
	dir string
}

// requireStackControl returns a channel that has been proven to answer, or
// fails the test saying why it could not be.
//
// The ping is not ceremony. Without it, a suite launched with no watcher beside
// it would issue its first real verb, wait out stackControlTimeout, and then
// report a ninety-second stack failure whose actual cause — nobody is listening
// — was knowable in a hundred milliseconds.
func requireStackControl(t *testing.T) *stackControl {
	t.Helper()

	dir := os.Getenv("COVES_STACK_CONTROL_DIR")
	if dir == "" {
		dir = defaultStackControlDir
	}
	control := &stackControl{dir: dir}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf(
			"the stack control channel is not present at %s (%v).\n"+
				"This suite restarts and reconfigures the AppView CONTAINER, so it needs the host-side\n"+
				"watcher scripts/lib/ci-stack.sh starts around a run. Both 'make ci' and 'make test-e2e'\n"+
				"start it; 'make test-e2e-dev' cannot (it grades a host-run AppView) and therefore\n"+
				"excludes these tests by name. If you are running the tier some other way, run it through\n"+
				"one of those two targets.",
			dir, err)
	}

	if err := control.send(t, "ping"); err != nil {
		t.Fatalf("the stack control channel at %s did not answer a ping: %v\n"+
			"The directory exists but nothing is serving it — a watcher that died, or a stack brought\n"+
			"up without one. 'make test-e2e' starts a fresh watcher for the run.", dir, err)
	}
	return control
}

// do issues one verb and fails the test if the host could not carry it out.
func (s *stackControl) do(t *testing.T, verb string) {
	t.Helper()
	if err := s.send(t, verb); err != nil {
		t.Fatalf("stack control %q: %v", verb, err)
	}
}

// nonFatal adapts a T for use inside a t.Cleanup.
//
// The wait primitives end a lapsed deadline with Fatalf, which on a real T is a
// runtime.Goexit — from a cleanup that unwinds the rest of the cleanup and, more
// to the point, discards the caller's own message about what the failure means
// for the tests that run next. A cleanup's failure is never about the test that
// registered it, because cleanups run after its verdict is already in; it is a
// warning to whatever grades this stack afterwards, and it has to be printed
// rather than jumped out of.
//
// So Fatalf becomes Errorf, which still fails the test, and the flag lets the
// caller see that the wait gave up and return its own diagnosis on top.
type nonFatal struct {
	testkit.TestingT
	failed bool
}

func (n *nonFatal) Fatalf(format string, args ...any) {
	n.failed = true
	n.TestingT.Errorf(format, args...)
}

// send writes a request, waits for its response, and reports what the host
// command did. The error carries the host's own output, which is where a
// compose failure explains itself.
func (s *stackControl) send(t testkit.TestingT, verb string) error {
	id := fmt.Sprintf("%s-%03d-%s", testkit.RunPrefix(), stackControlSeq.Add(1), verb)
	requestPath := filepath.Join(s.dir, id+".req")
	responsePath := filepath.Join(s.dir, id+".res")

	// Written to a temporary name and renamed, mirroring what the watcher does
	// with its response: the watcher polls for *.req and must never read a
	// half-written one. Rename inside a directory is atomic.
	temporaryPath := requestPath + ".tmp"
	if err := os.WriteFile(temporaryPath, []byte(verb+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing the request: %w", err)
	}
	if err := os.Rename(temporaryPath, requestPath); err != nil {
		return fmt.Errorf("publishing the request: %w", err)
	}

	// testkit.WaitFor rather than a poll loop of its own: §3.3 makes these the
	// only waiting primitives the suite may use, and the reason applies here as
	// much as to a serving endpoint — a bespoke loop grows its own timeout
	// semantics and reports a bare deadline where the shared one reports what it
	// last saw. The probe's read error is TERMINAL by the Probe contract, which
	// is right: a response file that exists but cannot be read is a broken
	// channel, not a slow one.
	var response []byte
	var probeErr error
	waiter := &nonFatal{TestingT: t}
	testkit.WaitFor(waiter, stackControlTimeout, func() (bool, error) {
		data, err := os.ReadFile(responsePath)
		switch {
		case err == nil:
			response = data
			return true, nil
		case os.IsNotExist(err):
			return false, nil
		default:
			// Kept as well as returned: the wait reports it, and send returns
			// it, so the caller's error says the channel broke rather than that
			// it timed out — two different things to debug.
			probeErr = fmt.Errorf("reading the response: %w", err)
			return false, probeErr
		}
	},
		testkit.WithPollInterval(stackControlPoll),
		testkit.WithDescription("the host to answer stack-control %q", verb),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf(
				"no response at %s. The host watcher (scripts/lib/ci-stack.sh) either is not running "+
					"or is wedged; its progress lines appear in the run's own output as "+
					"\"stack-control: <verb>\"", responsePath)
		}))

	switch {
	case probeErr != nil:
		return probeErr
	case waiter.failed:
		return fmt.Errorf("no response to %q within %s", verb, stackControlTimeout)
	}
	return parseStackControlResponse(string(response))
}

// parseStackControlResponse reads the "exit <n>" header the watcher writes and
// turns a non-zero status into an error carrying the command's output.
func parseStackControlResponse(response string) error {
	header, output, _ := strings.Cut(response, "\n")
	status, err := strconv.Atoi(strings.TrimPrefix(header, "exit "))
	if err != nil {
		return fmt.Errorf("malformed response (first line %q, expected \"exit <status>\"): %s", header, output)
	}
	if status != 0 {
		return fmt.Errorf("the host command exited %d: %s", status, strings.TrimSpace(output))
	}
	return nil
}

// ---------------------------------------------------------------------------
// What the verbs mean to a scenario
// ---------------------------------------------------------------------------

// appViewRestartBudget bounds a stopped AppView coming back to serving. It
// covers the container start, the process' boot (config, database migrations
// that are already applied, consumer wiring) and the first health response.
const appViewRestartBudget = 90 * time.Second

// outage stops the AppView, runs during() while it is down, and brings it back.
//
// The closure is the point. Every scenario that needs a gap in the AppView's
// consumption needs the SAME three things around it — the stack restored
// however the test ends, the AppView healthy again before anything is asserted,
// and the window kept as short as the test's own work — and a helper that owns
// all three is the difference between one honest gap and five subtly different
// ones.
//
// The restore is registered BEFORE the stop, so a t.Fatal or a panic inside
// during() still leaves a running AppView for the tests that follow. It is
// idempotent: starting an already-running container is a no-op to Compose.
func (s *stackControl) outage(t *testing.T, p *pipeline, during func()) {
	t.Helper()

	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		// Not t.Fatalf: cleanups run after the test's own verdict, and a
		// failure here is about the NEXT test, so it is logged as loudly as
		// possible and the error is left to surface there as an unreachable
		// AppView.
		if err := s.send(t, "appview-start"); err != nil {
			t.Errorf("could not restart the AppView after this scenario: %v\n"+
				"Every later contract in this tier observes through it, so they will fail too", err)
		}
	})

	s.do(t, "appview-stop")

	// Asserted rather than assumed: if the container did not actually go down,
	// the write below would be consumed live and the scenario would prove
	// nothing while passing.
	requireAppViewDown(t, p)

	during()

	s.do(t, "appview-start")
	restored = true
	p.AppView.WaitHealthy(t, appViewRestartBudget)
}

// requireAppViewDown fails unless the AppView is refusing connections.
//
// A stopped container in a SHARED network namespace answers "connection
// refused" rather than timing out, which is why this is quick and why it is
// worth checking at all: a verb that silently did nothing would leave every
// downstream assertion measuring the steady-state pipeline while claiming to
// measure recovery.
func requireAppViewDown(t *testing.T, p *pipeline) {
	t.Helper()
	testkit.WaitFor(t, 30*time.Second, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := p.AppView.Health(ctx)
		switch {
		case err == nil:
			return false, nil
		case testkit.StatusOf(err) == 0:
			// A transport-level failure: nothing is listening, which is the
			// state this waits for.
			return true, nil
		default:
			return false, fmt.Errorf(
				"the AppView answered HTTP %d after being stopped, so something is still serving "+
					"on its port: %w", testkit.StatusOf(err), err)
		}
	}, testkit.WithDescription("the AppView to stop answering after appview-stop"),
		testkit.WithPollInterval(contractPollInterval))
}
