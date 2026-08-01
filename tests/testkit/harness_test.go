//go:build integration

package testkit

import (
	"os"
	"testing"
)

// TestMain is also the worked example of the TestMain every migrating package
// gets: testkit's own tests exercise Postgres, the PDS and Jetstream, so all
// three are probed once here rather than failing test by test.
//
// It is tagged, and the support code it used to share a file with is not,
// because testkit's own suite spans two tiers: the pure tests (wait, fixtures,
// endpoints, the XRPC client against httptest) run in the tagless unit tier and
// must not be gated on infrastructure that they never touch. A TestMain governs
// the whole binary, so leaving this one untagged would have gated them anyway.
func TestMain(m *testing.M) {
	os.Exit(Main(m, RequirePostgres, RequirePDS, RequireJetstream))
}
