//go:build integration

package community_test

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
// in-package unit tests in get_test.go, list_test.go and friends. Those tests
// are pure handler tests with hand-written fakes and must keep building and
// running without a socket in sight, which is exactly what the tag guarantees:
// without -tags integration this file does not exist and the unit build has no
// TestMain at all.
//
// The floor is Postgres and nothing more. The only integration tests here —
// viewer_state_test.go — drive the get and list handlers over a real
// communities repository so that the viewer.subscribed field is answered from
// the real subscriptions table rather than from a fake. Authentication is
// injected straight into the request context, no PDS is dialled, and no
// firehose event is consumed, so demanding a PDS or a Jetstream would make the
// whole package fail for infrastructure it never touches.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
