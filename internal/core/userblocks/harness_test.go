//go:build integration

package userblocks_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test binary:
// the untagged unit build of this package (service_test.go, in package
// userblocks) needs nothing out of process and must not be made to probe
// Postgres and a PDS before it can run. A future test file here must therefore
// NOT declare a second TestMain — with -tags integration both halves compile
// into one binary, and two TestMains do not.
//
// The tests are in package userblocks_test (external) rather than in
// userblocks, because they exercise the service against the real repository in
// internal/db/postgres, which imports userblocks. In-package that is an import
// cycle; from outside it is an ordinary dependency. This mirrors
// internal/core/communities and internal/core/posts, whose integration tests
// are external packages for the same reason.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
