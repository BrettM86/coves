//go:build integration

package discover_test

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
// The floor is Postgres alone. The discover feed is a pure read over indexed
// rows — the ranking and the cursor both live in SQL — so these tests build
// posts directly in the database and drive the handler in process. No PDS,
// Jetstream or AppView is dialed.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
