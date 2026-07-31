//go:build integration

package users_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package needs nothing out of
// process, and must not be made to probe Postgres before it can run.
//
// The moved tests store a PDS URL string on user rows but never dial the PDS
// itself, so the floor here is RequirePostgres alone.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
