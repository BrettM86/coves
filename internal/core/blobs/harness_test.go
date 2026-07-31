//go:build integration

package blobs_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package (types_test.go) needs nothing
// out of process and must not be made to probe Postgres before it can run.
//
// The floor is Postgres AND a real PDS. Postgres because the tests drive the
// blob service and the image-URL hydration the post and community read paths
// depend on through in-process handlers against a real schema.
//
// The PDS because blob upload cannot be faked and still mean anything: the
// service signs com.atproto.repo.uploadBlob with a community's credentials and
// keeps the CID the server returns, so the tests provision real accounts
// (testkit.NewPDS(t).CreateAccount) and upload real image bytes. A stubbed
// upload would assert that a fake returned what the fake was told to return.
//
// TestBlobUpload_PDS_MockServer is the exception that proves the floor is not
// over-broad: it points the service at an httptest server precisely to exercise
// the error shapes a real PDS will not produce on demand. It shares this binary,
// so the floor is the union.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
