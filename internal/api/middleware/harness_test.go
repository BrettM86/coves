//go:build integration

package middleware_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package needs nothing out of
// process, and must not be made to probe the PDS before it can run.
//
// oauth_token_verification_test.go creates a real PDS account and never opens
// a database connection, so the floor here is RequirePDS alone.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePDS))
}
