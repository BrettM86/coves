//go:build integration

package jetstream

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
// The PDS floor is here for ONE property: §5.4's direct fetch must verify a
// record by recomputing its CID from the bytes the repo actually holds, and the
// only honest source of those bytes is a real repo. A hand-built CAR fixture
// would encode this package's guesses about the verification's internals and
// fail for reasons that have nothing to do with the property — see
// direct_fetch_verification_test.go.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
