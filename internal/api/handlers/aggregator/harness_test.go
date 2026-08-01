//go:build integration

package aggregator

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: this package's unit tests (register_test.go, apikey_handlers_test.go)
// need nothing out of process and must not be made to probe Postgres before
// they can run. Under -tags integration both halves compile into one binary, so
// a future test file here must NOT declare a second TestMain.
//
// Only one test needs this floor — register_users_row_test.go, which checks
// what registration actually writes — and the package stays in-package
// (`package aggregator`) because it reaches for no repository type: the
// dependency runs the other way, through internal/db/postgres, which does not
// import this package.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
