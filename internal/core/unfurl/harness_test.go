//go:build integration

package unfurl_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: this package's unit tests (opengraph, providers, kagi, circuit
// breaker) are untagged, socket-free and must stay that way.
//
// The floor is Postgres alone. The moved tests prove that an unfurled embed
// survives the post write path and is served back — the unfurl targets
// themselves are local httptest servers, so no egress and no PDS are involved.
// Tests against real third-party targets are the T3 live tier (§3.2).
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
