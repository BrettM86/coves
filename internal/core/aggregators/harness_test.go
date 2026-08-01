//go:build integration

package aggregators_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package (apikey_service_test.go, in
// package aggregators) needs nothing out of process and must not be made to
// probe Postgres before it can run. A future test file here must therefore NOT
// declare a second TestMain — with -tags integration both halves compile into
// one binary, and two TestMains do not.
//
// The tests are in package aggregators_test (external) rather than in
// aggregators, because they exercise the service against the real repository in
// internal/db/postgres, which imports aggregators. In-package that is an import
// cycle; from outside it is an ordinary dependency. This mirrors
// internal/core/posts and internal/core/communities, whose integration tests are
// external test packages for the same reason.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
