//go:build integration

package communities_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test binary:
// the untagged unit build of this package needs nothing out of process, and must
// not be made to probe Postgres and a PDS before it can run. A future unit test
// file here must therefore NOT declare its own TestMain — with -tags integration
// both files compile into one binary, and two TestMains do not.
//
// The tests are in package communities_test (external) rather than in
// communities, because they exercise the service against the real repository in
// internal/db/postgres, which imports communities. In-package that is an import
// cycle; from outside it is an ordinary dependency.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
