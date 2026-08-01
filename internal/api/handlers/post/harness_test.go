//go:build integration

package post_test

import (
	"database/sql"
	"os"
	"testing"

	"Coves/internal/api/handlers/post"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
)

// TestMain sets the infrastructure floor for this package's integration build.
//
// It lives in a build-tagged file on purpose. A TestMain governs the WHOLE test
// binary, and under -tags integration the tagged and untagged files of this
// directory compile into one binary — so this function also runs for the
// in-package unit tests in get_test.go and errors_test.go. Those are pure
// handler tests over hand-written fakes and must keep building and running
// without a socket in sight, which the tag guarantees: without
// -tags integration this file does not exist and the unit build has no TestMain
// at all.
//
// The floor is Postgres and nothing more. The integration tests here drive the
// create endpoint through the real post and community services down to the real
// repositories, because that is the only way to prove the validation is on the
// live path rather than merely unit-tested in isolation. Every case stops at
// validation or at "community not found", both of which are answered from the
// database — nothing here reaches the PDS, so requiring one would fail the
// package for infrastructure it never dials.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}

// createStack is the create endpoint plus the two collaborators a test needs to
// reach around it: the service, for the defence-in-depth checks that must hold
// even when the handler is bypassed, and the community repository, for seeding
// the community a request names.
type createStack struct {
	handler         *post.CreateHandler
	service         posts.Service
	communityRepo   communities.Repository
	communityPDSURL string
}

// newCreateStack wires the create endpoint the way the server wires it, minus
// the services that are legitimately optional.
//
// The wiring is the point of these tests, so the handler is NOT given a fake
// post service: it gets the real posts.Service over the real repositories, so a
// validation rule that stopped being called — moved behind an early return,
// dropped from the request path — fails here even though its own unit test
// still passes.
//
// The aggregator, blob, unfurl and Bluesky services are nil because
// posts.NewPostService documents them as optional and no case below produces a
// post that would reach them: a request only gets that far with a community
// whose PDS credentials actually work, which is the pipeline tier's job to
// prove.
func newCreateStack(t *testing.T, db *sql.DB) createStack {
	t.Helper()

	pdsURL := testkit.Endpoints().PDS.BaseURL
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		fixtures.InstanceDID(),
		testkit.Endpoints().PDS.HandleDomain,
		nil, // no provisioner: no test here creates a community account
		nil, // no PDS client factory
		nil, // no blob service
	)

	postService := posts.NewPostService(
		postgres.NewPostRepository(db),
		communityService,
		nil, nil, nil, nil,
		pdsURL,
	)

	return createStack{
		handler:         post.NewCreateHandler(postService),
		service:         postService,
		communityRepo:   communityRepo,
		communityPDSURL: pdsURL,
	}
}
