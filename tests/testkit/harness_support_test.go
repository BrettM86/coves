package testkit

import (
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Test support shared by every tier of testkit's own suite, and therefore
// untagged: a tagged build includes untagged files, but not the reverse, so
// anything both tiers use has to live here. TestMain, which genuinely needs
// infrastructure, is in harness_test.go behind the integration tag.

// fakeT records what a testkit helper did to the test it was handed, so the
// failure paths can be asserted on.
//
// This is why TestingT exists as an interface: testing.TB cannot be implemented
// outside the standard library, so without it the only way to test "WaitFor
// fails with the last observation attached" would be to compile and run a
// throwaway test binary in a subprocess.
type fakeT struct {
	mu       sync.Mutex
	failures []string
	logs     []string
	cleanups []func()
	fatal    bool
}

func (f *fakeT) Helper()      {}
func (f *fakeT) Name() string { return "fakeT" }

func (f *fakeT) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeT) Logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *fakeT) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

// Fatalf mirrors *testing.T: it records the failure and does not return.
// Helpers under test rely on that — they call Fatalf and then `return`, and a
// fake that simply returned would let them run on with invalid state.
func (f *fakeT) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
	f.fatal = true
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeT) failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failures) > 0
}

func (f *fakeT) message() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.failures, "\n")
}

func (f *fakeT) runCleanups() {
	f.mu.Lock()
	cleanups := f.cleanups
	f.cleanups = nil
	f.mu.Unlock()
	// Reverse order, as testing.T runs them.
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

// runIsolated runs fn on its own goroutine, so a fakeT.Fatalf inside it exits
// that goroutine instead of the test's.
func runIsolated(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

// resetTemplateVerification forgets that this process already checked the
// template, forcing the next EnsureTemplate to re-verify against Postgres.
func resetTemplateVerification() {
	templateMu.Lock()
	defer templateMu.Unlock()
	templateVerified = false
}

// swapEndpoints rebinds only the endpoint singleton, re-reading it from the
// current environment, and registers the restore as a cleanup.
//
// Assigning endpointsOnce directly is a data race the moment `-shuffle=on`
// reorders tests or anything runs in parallel — the accessors read it under
// singletonMu, so writers must take that lock too. Every endpoint test needs to
// re-read the environment after t.Setenv, so the locking lives here once rather
// than being retyped (and forgotten) at each site.
func swapEndpoints(t *testing.T) {
	t.Helper()
	singletonMu.Lock()
	savedEndpoints, savedTemplateName := endpointsOnce, templateNameOnce
	endpointsOnce = sync.OnceValue(loadEndpoints)
	// The template name is validated against the maintenance database, so it
	// has to be re-derived whenever the endpoints change.
	templateNameOnce = sync.OnceValues(loadTemplateName)
	singletonMu.Unlock()

	t.Cleanup(func() {
		singletonMu.Lock()
		endpointsOnce, templateNameOnce = savedEndpoints, savedTemplateName
		singletonMu.Unlock()
	})
}

// swapSingletons rebinds the memoised process singletons and returns a function
// that restores them.
//
// Every write goes through singletonMu, the same lock the accessors read under.
// Assigning these directly from a test — as an earlier version did — is a data
// race the moment any test runs in parallel or `-shuffle=on` reorders things,
// and it would surface as an unrelated test failing under -race.
func swapSingletons(endpoints func() EndpointSet, admin func() (*sql.DB, error)) func() {
	singletonMu.Lock()
	savedEndpoints, savedAdmin := endpointsOnce, adminDBOnce
	savedTemplateName := templateNameOnce
	endpointsOnce, adminDBOnce = endpoints, admin
	// The template name is validated against the maintenance database, so it
	// has to be re-derived whenever the endpoints change.
	templateNameOnce = sync.OnceValues(loadTemplateName)
	singletonMu.Unlock()

	templateMu.Lock()
	savedVerified := templateVerified
	templateVerified = false
	templateMu.Unlock()

	return func() {
		singletonMu.Lock()
		endpointsOnce, adminDBOnce = savedEndpoints, savedAdmin
		templateNameOnce = savedTemplateName
		singletonMu.Unlock()

		templateMu.Lock()
		templateVerified = savedVerified
		templateMu.Unlock()
	}
}
