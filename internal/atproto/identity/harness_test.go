//go:build integration

package identity_test

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
// The floor is Postgres alone. What this package's integration tests cover is
// the identity cache, whose storage is the identity_cache table — resolution
// against the PLC directory or a handle's DNS records belongs to tests/live,
// which is the only tier permitted to reach the public network.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
