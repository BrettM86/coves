//go:build integration

package imageproxy_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a build-tagged file on purpose. A TestMain governs the WHOLE test
// binary, and under -tags integration the tagged and untagged files of this
// directory compile into one binary — so this function also runs for the
// in-package unit tests in handler_test.go. Those are pure handler tests over a
// fake service and a fake resolver and must keep building and running without a
// socket in sight, which the tag guarantees: without -tags integration this
// file does not exist and the unit build has no TestMain at all.
//
// The floor is Postgres AND a real PDS, and only one of the two files here
// needs the second half. avatar_serving_test.go provisions a community account
// on the PDS, uploads a real avatar blob to it, and then asks the proxy to
// fetch that blob back through DID resolution — it is the only place the
// proxy's PDS fetcher is exercised against a server that behaves like a PDS
// rather than like an httptest handler someone wrote to look like one.
// proxy_serving_test.go needs neither: it stands the routed handler up over
// mock PDS servers in-process. The floor is the union because a TestMain cannot
// be scoped to a file, and probing up front is what turns "the stack is down"
// into one clear failure instead of a dozen confusing ones.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
