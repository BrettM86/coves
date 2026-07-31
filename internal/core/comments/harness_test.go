//go:build integration

package comments_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a tagged file because a TestMain applies to the whole test
// binary: the untagged unit build of this package (comment_service_test.go,
// comment_write_service_test.go) runs against fakes and must not be made to
// probe Postgres before it can run.
//
// The floor is Postgres and the PDS. comment_query_test.go,
// comment_consumer_test.go and comment_vote_test.go feed synthetic Jetstream
// events to the comment and vote consumers and read the results back through
// the repository and the query service — the §3.2 T1 consumer seam, which
// terminates at Postgres and needs nothing else.
// comment_write_test.go is the other T1 seam §3.4b names: the write path
// forwards to a real PDS, and asserting the record it actually wrote there is
// the coverage T2 cannot have while sealed sessions mint only in a browser.
//
// Neither dials a websocket: the consumer is fed directly.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS))
}
