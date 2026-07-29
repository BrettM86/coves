//go:build integration

package posts_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package (blob_transform_test.go,
// embed_validation_test.go, service_get_posts_test.go and friends, all in
// package posts) needs nothing out of process and must not be made to probe
// Postgres and a PDS before it can run. A future test file here must therefore
// NOT declare a second TestMain — with -tags integration both halves compile
// into one binary, and two TestMains do not.
//
// The tests are in package posts_test (external) rather than in posts, because
// they exercise the service against the real repositories in
// internal/db/postgres, which imports posts. In-package that is an import
// cycle; from outside it is an ordinary dependency. This mirrors
// internal/core/communities, whose integration tests are package
// communities_test for the same reason.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
