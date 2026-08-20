package jetstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	covesoauth "Coves/internal/atproto/oauth"
)

// The §5.4 direct post fetch, and the gate that decides whether it is guarded.
//
// # THE CALL SITE
//
// convergeOnAcceptedSubject reaches it when a community acceptance names a post
// this AppView has never indexed. The PDS it dials is resolved from the DID
// document of the repo the ACCEPTANCE named, and anyone on the federated network
// can write an acceptance and publish a DID document — so the destination of
// this request is chosen by a stranger, from inside the AppView's own network,
// next to its Postgres and its PDS.
//
// # WHY THIS FILE IS T0
//
// httpClient() — the one line that reads f.allowPrivateHosts — has exactly one
// other behavioural proof in the tree, TestDirectPostFetcher_RefusesAPrivateHostByDefault
// at acceptance_consumer_test.go:518. That file is `//go:build integration`, so
// `go test ./...` does not even COMPILE it and the inner loop had no coverage of
// this guard at all. That test is untouched and still the T1 authority; this file
// is its T0 counterpart, plus the first coverage the GATE has ever had.
//
// # WHAT REPLACED NewDevDirectPostFetcher, AND WHY IT IS STRONGER
//
// The hatch used to be a second EXPORTED constructor whose name encoded the
// unsafe choice and whose body hardcoded `allowPrivateHosts: true`. Its own doc
// comment claimed "no production wiring can reach it by passing a variable that
// happens to be true" — but being exported, any package including cmd/server
// could simply call it. The guarantee was prose.
//
// withPrivateHostsAllowed is unexported, so outside this package the hatch is
// unreachable and the only route in is PrivatePostFetcherOptions, whose false
// branch returns nothing. That is the same guarantee enforced by the type system.
// And the BRANCH left cmd/server/consumers.go, which is what §7 requires:
// `.env.ci:140` sets IS_DEV_ENV=true, so the hermetic merge gate only ever ran
// that inline conditional's permissive side.
//
// # WHY THE BEHAVIOURAL ASSERTIONS ARE NOT TYPE-SHAPED
//
// A guarded and a hatched fetcher are the same type — the hatch is a bool field
// inside one struct — so no type check and no reflect.TypeOf can tell them apart.
// Reachability is asserted for a second reason: mutation testing
// produced a guard that classified correctly, emitted a byte-identical message,
// and refused the request AFTER delivering it. For a destination a stranger
// named, the packet leaving IS the SSRF.

// ssrfTestPostURI is a well-formed at:// record URI. parseRecordURI runs before
// anything else in FetchPost, so a malformed one would make these tests pass for
// the wrong reason — the fetcher would refuse the URI and never reach a client.
const ssrfTestPostURI = "at://did:plc:z72i7hdynmk6r22z27h6tvur/social.coves.community.postv2/3kabcdefghij"

// ssrfTestAuthorDID is the repo half of the URI above.
const ssrfTestAuthorDID = "did:plc:z72i7hdynmk6r22z27h6tvur"

// countingPDS is a PDS that answers getRecord and records whether anything ever
// reached it. It listens on loopback, which is the address class the guard
// exists to refuse, so its counter IS the assertion.
type countingPDS struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingPDS(t *testing.T) *countingPDS {
	t.Helper()

	pds := &countingPDS{}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pds.requests.Add(1)
		// Not a CAR. The hatched case asserts on the counter, not on success:
		// what it has to show is that the request ARRIVED, and building a real
		// signed repo here would test repo.ReadRepoFromCar instead of the gate.
		// The T1 test that does build a real repo is
		// TestDirectFetch_RecomputesTheCIDFromARealRepo.
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write([]byte("not a car"))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// resolverFor points the fetcher's DID resolution at the counting PDS.
func resolverFor(pds *countingPDS) identity.Resolver {
	return &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		ssrfTestAuthorDID: {DID: ssrfTestAuthorDID, Handle: "author.test", PDSURL: pds.server.URL},
	}}
}

// fetchThroughGate drives one fetch through the gate the way
// cmd/server/consumers.go does — NewDirectPostFetcher, from
// PrivatePostFetcherOptions and the same allow-private boolean.
//
// Through the production route on purpose. Setting f.allowPrivateHosts directly,
// or replacing f.client, would prove nothing about the wiring: the community
// consumer's own SSRF file records a mutation where a fixture that replaced the
// client left `newWellKnownClient(true)` — the guard disabled for every
// production consumer — failing zero tests.
func fetchThroughGate(t *testing.T, pds *countingPDS, allowPrivate bool) error {
	t.Helper()

	fetcher := NewDirectPostFetcher(resolverFor(pds), PrivatePostFetcherOptions(allowPrivate)...)

	_, err := fetcher.FetchPost(context.Background(), ssrfTestPostURI)
	return err
}

// TestPrivatePostFetcherOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed
// is the §7-standard assertion: isDev=false yields NO options.
//
// The claim is not "the options returned are safe". It is that there are none —
// length zero, nothing applied, the constructor's own defaults left untouched.
func TestPrivatePostFetcherOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivatePostFetcherOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivatePostFetcherOptions(false) returned %d option(s). The production branch — the one "+
			"IS_DEV_ENV=true keeps `make ci` from ever evaluating — must contribute nothing at all, "+
			"so that what production gets is exactly the constructor's own defaults", len(opts))
}

// TestPrivatePostFetcherOptions_BindTheGateToTheConstructor pins both directions
// through the state the constructor actually ends up in.
//
// A length check on the false branch is worthless alone: a helper returning
// nothing in BOTH directions satisfies it while guaranteeing the hatch can never
// open, and a helper returning the wrong single option satisfies it while
// leaving every developer unable to reach their local PDS.
//
// This asserts a FIELD, not a type — the two directions produce the same type,
// so a type assertion here would be measuring nothing.
func TestPrivatePostFetcherOptions_BindTheGateToTheConstructor(t *testing.T) {
	t.Parallel()

	guarded := NewDirectPostFetcher(nil, PrivatePostFetcherOptions(false)...)
	assert.False(t, guarded.allowPrivateHosts,
		"a fetcher built from PrivatePostFetcherOptions(false) has the SSRF hatch open. This is the "+
			"branch production runs and CI never does")

	hatched := NewDirectPostFetcher(nil, PrivatePostFetcherOptions(true)...)
	assert.True(t, hatched.allowPrivateHosts,
		"a fetcher built from PrivatePostFetcherOptions(true) is still guarded, so the dev hatch does "+
			"nothing and a local stack's PDS — which is on loopback — cannot be fetched from")
}

// TestPrivatePostFetcherOptions_GuardedRefusesAPrivatePDSWithoutReachingIt is the
// binding contract, and the behavioural half of the pin: false is the branch
// production runs and the one `make ci` never evaluates.
//
// THE ASSERTIONS THAT GO RED UNDER MUTATION are the ErrBlockedAddress check and
// the zero-requests check. Neither is type-shaped: the first names the mechanism,
// so an unrelated failure cannot satisfy it, and the second observes a real
// listener, so a guard that refuses only after delivering still fails.
func TestPrivatePostFetcherOptions_GuardedRefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	err := fetchThroughGate(t, pds, false)

	require.Error(t, err,
		"the direct fetch reached a PDS on loopback with the gate shut. This is the request any "+
			"federated instance triggers by writing an acceptance for a post this AppView has not "+
			"indexed, against a DID document it published itself")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity. Without this, a build where the guarded client "+
			"was never wired looks identical — the fixture serves no valid CAR, so an error comes "+
			"back either way and an assertion that only says 'it failed' would still pass; got: %v", err)
	assert.Zerof(t, pds.requests.Load(),
		"the PDS listener was reached %d times. The refusal happened, but it happened AFTER the "+
			"request was delivered — which prevents none of the SSRF", pds.requests.Load())
}

// TestPrivatePostFetcherOptions_HatchedReachesAPrivatePDS is the falsifiability
// control.
//
// Identical fixture, identical call, only the boolean differs. Without it, a
// fetcher that could make no request at all satisfies the guarded case just as
// well, and the test above would prove nothing about CLASSIFICATION.
//
// It is also the half a developer depends on: the hermetic stack's PDS is a
// private address, so with the gate stuck shut acceptance-before-post never
// converges locally.
func TestPrivatePostFetcherOptions_HatchedReachesAPrivatePDS(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	err := fetchThroughGate(t, pds, true)

	require.Error(t, err,
		"the fixture serves nine bytes that are not a CAR, so the fetch must still fail — if it "+
			"succeeded, this test is not reaching the fixture it thinks it is")
	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the gate was open and the address was still refused by the guard. Either "+
			"PrivatePostFetcherOptions is not reaching the client, or the guarded case above proves "+
			"nothing: a fetcher that refuses every address refuses the guarded case too, for a reason "+
			"that has nothing to do with classification; got: %v", err)
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the PDS listener was reached %d times rather than once", pds.requests.Load())
}

// TestNewDirectPostFetcher_RefusesAPrivateHostByDefault is the T0 counterpart of
// acceptance_consumer_test.go:518, which is the only behavioural proof this
// guard had and is `//go:build integration` — so `go test ./...` never compiled
// it and the inner loop could not see a regression here at all.
//
// It passes NO options, so it holds independently of PrivatePostFetcherOptions:
// a caller that expressed no opinion must degrade to the guarded fetcher. The T1
// test is untouched and remains the authority; this one exists so the failure is
// visible without Postgres.
func TestNewDirectPostFetcher_RefusesAPrivateHostByDefault(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	_, err := NewDirectPostFetcher(resolverFor(pds)).FetchPost(context.Background(), ssrfTestPostURI)

	require.Error(t, err,
		"NewDirectPostFetcher with no options reached a PDS on loopback. The PDS this dials is named "+
			"by a DID document anyone can publish, so the default must be the guarded one: every "+
			"caller that expressed no opinion degrades to it")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must be the guard's and must say so; got: %v", err)
	assert.Zerof(t, pds.requests.Load(),
		"the PDS listener was reached %d times by the default construction", pds.requests.Load())
}

// TestPrivatePostFetcherOptions_GuardIsNotAmbient states the invariant at this
// site too.
//
// The decision belongs to the CALLER — cmd/server passes allowPrivateHosts() —
// and must never become a read of the environment inside the gate, the
// constructor or the fetch. This process has IS_DEV_ENV set to true and the
// guarded branch must still refuse, because the same binary builds
// productionPLCResolver, which has to stay guarded in dev.
func TestPrivatePostFetcherOptions_GuardIsNotAmbient(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "true")

	pds := newCountingPDS(t)

	err := fetchThroughGate(t, pds, false)

	require.Error(t, err,
		"the guarded branch reached a loopback PDS while IS_DEV_ENV was true. The gate must be a "+
			"property of the argument, not of the environment: an ambient read opens every other "+
			"construction in this process at the same time")
	assert.Zerof(t, pds.requests.Load(),
		"the PDS listener was reached %d times with IS_DEV_ENV=true", pds.requests.Load())
}
