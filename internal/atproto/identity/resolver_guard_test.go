package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The identity resolver's HTTP client, against a PLC directory nobody vetted.
//
// # WHY THIS SITE IS UNAUTHENTICATED
//
// The /img route resolves a DID before it fetches anything, and it does so with
// no credential of any kind. A did:web carries its own hostname — the resolver
// fetches https://<hostname>/.well-known/did.json — so the destination is a
// string in the URL a stranger typed. Indigo's AllowedTLD check refuses eight
// reserved TLDs — local, arpa, invalid, localhost, internal, example, onion and
// alt — and stops nothing else: a public hostname with a 127.0.0.1 A record has
// a perfectly ordinary TLD, and the TLD is all that check looks at.
//
// # THE TRAP THIS FILE EXISTS FOR: TWO RESOLVERS, OPPOSITE REQUIREMENTS
//
// cmd/server builds two, from the same DefaultConfig, and they need the guard
// set differently:
//
//   - a.identityResolver (wiring.go:205) points at cfg.Identity.ResolverPLCURL,
//     which in dev is a PLC on loopback. It NEEDS the hatch when IsDevEnv.
//   - productionPLCResolver (wiring.go:357) always points at plc.directory,
//     because quoted Bluesky posts name real handles the dev PLC cannot resolve.
//     It must stay GUARDED even in dev.
//
// So the decision cannot live in this package as an ambient "are we in dev"
// read. It has to be per-construction, and
// TestTwoResolvers_OppositeRequirementsInTheSameProcess is the assertion that
// says so: it builds both in one process with IS_DEV_ENV set and requires them
// to behave differently. A hatch hoisted to package level passes every other
// test in this file and fails that one.
//
// # WHY THESE DRIVE THE BASE RESOLVER
//
// cachingResolver.ResolveDID reads the identity_cache table BEFORE it calls the
// base resolver — the same shape as the image proxy's cache short-circuit, and
// the same false-pass: a warm cache returns a document without a fetch and
// asserts nothing about the guard. Driving newBaseResolver directly removes
// both the cache and the Postgres dependency, which is what keeps these in T0
// where the guarded branch is the only branch CI ever evaluates.
//
// # WHY REACHABILITY, NOT ONLY THE ERROR
//
// Mutation testing produced a guard that classified correctly, emitted
// a byte-identical message, and refused the request AFTER delivering it. For a
// destination a stranger named, the packet leaving IS the SSRF. Every case here
// stands up a real listener and asserts its handler never ran.

// testDID is a syntactically valid did:plc. The value is irrelevant — nothing
// below gets far enough for it to matter — but syntax.ParseDID rejects a
// malformed one before any client is used, which would make a guard test pass
// for the wrong reason.
const testDID = "did:plc:z72i7hdynmk6r22z27h6tvur"

// countingPLC is a PLC directory that answers a DID lookup and records whether
// anything ever reached it. It listens on loopback — the address class the
// guard exists to refuse — so its counter doubles as the assertion.
type countingPLC struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingPLC(t *testing.T) *countingPLC {
	t.Helper()

	plc := &countingPLC{}
	plc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plc.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// No alsoKnownAs, deliberately. Indigo's LookupDID resolves a DECLARED
		// handle over DNS and HTTPS to verify it bidirectionally, and that
		// lookup would leave the machine — which the hermetic tiers forbid and
		// an egress-blocked CI would fail on. With no handle declared, indigo
		// marks it invalid and returns, so this fixture stays local.
		_, _ = fmt.Fprintf(w, `{"id":%q,"service":[{"id":"#atproto_pds",`+
			`"type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.invalid"}]}`, testDID)
	}))
	t.Cleanup(plc.server.Close)
	return plc
}

// resolveThrough points a base resolver at the listener with the given client
// and asks it to resolve a DID. It returns whatever the resolver returned.
func resolveThrough(t *testing.T, plc *countingPLC, client *http.Client) (*DIDDocument, error) {
	t.Helper()

	require.NotNil(t, client, "the config under test produced a nil HTTP client, which indigo dereferences")
	return newBaseResolver(plc.server.URL, client).ResolveDID(context.Background(), testDID)
}

// TestDefaultConfig_ClientRefusesAPrivateAddressWithoutReachingIt is the
// binding contract for every caller who never thinks about this at all.
//
// DefaultConfig() with no arguments is what productionPLCResolver uses, what
// tests/live uses, and what the next caller will use. Its client must be
// guarded, so that forgetting is safe.
//
// IS_DEV_ENV IS SET HERE ON PURPOSE, and it is the most important line in the
// test. The gate belongs to the CALLER, not to this package: a future edit that
// "helpfully" reads the environment inside DefaultConfig would open the hatch
// for productionPLCResolver too, in the one environment where that resolver is
// pointed at the public directory and the machine also runs a PLC, a Postgres
// and a PDS on loopback. This test fails on that edit; nothing else would.
func TestDefaultConfig_ClientRefusesAPrivateAddressWithoutReachingIt(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it, and this file's subject is what the
	// environment must NOT be able to change.
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("PLC_DIRECTORY_URL", "")

	plc := newCountingPLC(t)

	_, err := resolveThrough(t, plc, DefaultConfig().httpClient)

	require.Errorf(t, err,
		"DefaultConfig()'s client resolved a DID against a PLC directory on loopback. This is the "+
			"config every caller gets by writing nothing, and the /img route reaches it with no "+
			"credential at all")
	assert.Containsf(t, err.Error(), "SSRF blocked",
		"the refusal must be the guard's and must say so: identity's error taxonomy flattens its cause "+
			"into a string, so this sentence is all an operator gets to tell a blocked address from a "+
			"PLC directory that is merely down; got: %v", err)
	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times with IS_DEV_ENV=true. Either the client is unguarded, or "+
			"the hatch has been made ambient — and an ambient hatch opens productionPLCResolver as well, "+
			"which is the one resolver that must stay guarded in dev", plc.requests.Load())
}

// TestDefaultConfig_HatchedClientReachesALoopbackPLC is the other direction,
// and it is not a nicety: a dev stack runs its PLC on localhost:3002, and the
// Many integration fixtures point a resolver at a loopback
// PLC. Without an injectable allowance the guard takes all of them with it.
func TestDefaultConfig_HatchedClientReachesALoopbackPLC(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	plc := newCountingPLC(t)

	doc, err := resolveThrough(t, plc, DefaultConfig(WithPrivateHostsAllowed()).httpClient)

	require.NoErrorf(t, err,
		"a resolver built with WithPrivateHostsAllowed() must reach a loopback PLC; got: %v", err)
	require.NotNil(t, doc, "the resolved document must come back")
	assert.Equal(t, testDID, doc.DID, "the document must be the one the directory served")
	require.Lenf(t, doc.Service, 1, "the PDS service entry must survive resolution: %+v", doc)
	assert.Equal(t, "https://pds.example.invalid", doc.Service[0].ServiceEndpoint,
		"the PDS endpoint is what every downstream fetch is aimed at")
	assert.Equalf(t, int64(1), plc.requests.Load(),
		"the PLC listener was reached %d times rather than once", plc.requests.Load())
}

// TestTwoResolvers_OppositeRequirementsInTheSameProcess is the trap, stated as
// one test.
//
// Both configs are built in the same process, in the same environment, with
// IS_DEV_ENV=true — the arrangement cmd/server actually produces. The dev
// resolver must reach the loopback PLC and the production-directory resolver
// must refuse it. Any implementation where the hatch is a property of the
// PACKAGE or of the ENVIRONMENT rather than of the individual construction
// makes these two agree, and this is the only test that notices.
//
// The production side is pointed at the loopback listener rather than at
// plc.directory deliberately. What is under test is its CLIENT, and a client
// aimed at a host it will never be allowed to dial proves nothing; aiming it at
// the address the guard is supposed to refuse is what makes the refusal
// observable. Note also that this is precisely how the mistake would present in
// production — the wiring difference between the two resolvers is one line, and
// getting it backwards leaves both green.
func TestTwoResolvers_OppositeRequirementsInTheSameProcess(t *testing.T) {
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("PLC_DIRECTORY_URL", "")

	devPLC := newCountingPLC(t)
	productionPLC := newCountingPLC(t)

	// a.identityResolver: dev points it at a local PLC, so it holds the hatch.
	devConfig := DefaultConfig(PrivateHostOptions(true)...)
	// productionPLCResolver: always the public directory, so it holds nothing.
	productionConfig := DefaultConfig()

	_, devErr := resolveThrough(t, devPLC, devConfig.httpClient)
	_, productionErr := resolveThrough(t, productionPLC, productionConfig.httpClient)

	require.NoErrorf(t, devErr,
		"the dev-configured resolver could not reach its local PLC. Every developer's stack and roughly "+
			"twenty test sites resolve against a PLC on loopback; got: %v", devErr)
	assert.Equalf(t, int64(1), devPLC.requests.Load(),
		"the dev PLC was reached %d times rather than once", devPLC.requests.Load())

	require.Errorf(t, productionErr,
		"the production-directory resolver reached a PLC on loopback while IS_DEV_ENV was true. Its "+
			"hatch must be closed in EVERY environment: it is the resolver aimed at plc.directory, and "+
			"a dev machine is exactly where the loopback it would otherwise dial is a real PLC, a real "+
			"Postgres and a real PDS")
	assert.Zerof(t, productionPLC.requests.Load(),
		"the production-directory resolver's listener was reached %d times. If both resolvers behave the "+
			"same way here, the hatch is a property of the package or the environment rather than of the "+
			"construction — which is the one shape of this fix that cannot be right, because the two "+
			"resolvers need opposite answers", productionPLC.requests.Load())
}

// TestNewResolver_FallbackClientIsGuarded covers factory.go:50, the OTHER bare
// client in this package.
//
// A caller who passes a Config with no HTTPClient — `NewResolver(db,
// Config{PLCURL: ...})`, which the existing suite shows is a normal thing to
// write — gets one built for them. Guarding DefaultConfig and leaving this
// branch bare would mean the safety of a resolver depends on which of two
// equally ordinary constructions its author happened to use.
//
// It reaches through to the base resolver on purpose: cachingResolver would
// read the identity_cache table first, and this test has no database. That is
// the cache trap, avoided rather than papered over.
func TestNewResolver_FallbackClientIsGuarded(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	resolver := NewResolver(nil, Config{PLCURL: plc.server.URL})

	caching, ok := resolver.(*cachingResolver)
	require.True(t, ok, "NewResolver must return the caching wrapper")

	_, err := caching.base.ResolveDID(context.Background(), testDID)

	require.Errorf(t, err,
		"a Config with no HTTPClient produced an unguarded client. factory.go:50 fills the gap for "+
			"every caller who writes Config{PLCURL: ...}, which the rest of this package's tests show "+
			"is the ordinary spelling")
	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times through NewResolver's fallback client", plc.requests.Load())
}

// TestNewResolver_HonoursAnExplicitlyHatchedConfig pins that a config carrying
// the hatch survives NewResolver, since that is the path cmd/server takes for
// a.identityResolver: DefaultConfig(...), then PLCURL overwritten, then
// NewResolver.
func TestNewResolver_HonoursAnExplicitlyHatchedConfig(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	plc := newCountingPLC(t)

	config := DefaultConfig(WithPrivateHostsAllowed())
	config.PLCURL = plc.server.URL

	resolver := NewResolver(nil, config)
	caching, ok := resolver.(*cachingResolver)
	require.True(t, ok, "NewResolver must return the caching wrapper")

	doc, err := caching.base.ResolveDID(context.Background(), testDID)

	require.NoErrorf(t, err,
		"the hatch did not survive DefaultConfig -> PLCURL override -> NewResolver, which is exactly "+
			"the sequence cmd/server's buildIdentity performs; got: %v", err)
	assert.Equal(t, testDID, doc.DID)
	assert.Equalf(t, int64(1), plc.requests.Load(),
		"the PLC listener was reached %d times rather than once", plc.requests.Load())
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// single most important assertion for this call site.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — T0+T1+T2 — takes the
// PERMISSIVE branch at every site holding such a boolean. The guarded branch is
// evaluated in exactly one place in this repository, and it is this function.
// That is why the gate is a pure function rather than an `if a.cfg.IsDevEnv` in
// cmd/server/wiring.go, and why it must not be inlined back.
//
// The claim is that there are NO options — not that the options are safe. An
// edit that appends a diagnostic option, or returns a one-element slice holding
// a no-op "explicitly deny" closure, keeps every behavioural test green while
// moving the untested branch from "provably applies nothing" to "applies
// something believed harmless".
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). What production gets must be exactly "+
			"DefaultConfig's own defaults, with nothing applied on top", len(opts))
}

// TestPrivateHostOptions_DisallowedConfigIsGuarded is the behavioural half of
// the assertion above: zero options has to also MEAN a guarded client. The
// length check alone would still pass if DefaultConfig's own default regressed
// to permissive — the helper would be correctly returning nothing, onto a base
// that no longer refuses anything.
func TestPrivateHostOptions_DisallowedConfigIsGuarded(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	plc := newCountingPLC(t)

	_, err := resolveThrough(t, plc, DefaultConfig(PrivateHostOptions(false)...).httpClient)

	require.Error(t, err,
		"a config built from PrivateHostOptions(false) reached a loopback PLC. This is the branch "+
			"production runs and CI never does")
	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times, so the packet left the process", plc.requests.Load())
}

// TestPrivateHostOptions_AllowedConfigReachesTheListener pins the permissive
// direction through observed behaviour rather than through the shape of the
// slice. A length check here would be worthless: a helper returning some other
// single option satisfies it while leaving every dev stack unable to resolve
// anything.
func TestPrivateHostOptions_AllowedConfigReachesTheListener(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	plc := newCountingPLC(t)

	doc, err := resolveThrough(t, plc, DefaultConfig(PrivateHostOptions(true)...).httpClient)

	require.NoErrorf(t, err,
		"a config built from PrivateHostOptions(true) was refused. The permissive branch is what every "+
			"developer and every loopback-PLC fixture in this tree runs; got: %v", err)
	assert.Equal(t, testDID, doc.DID)
	assert.Equalf(t, int64(1), plc.requests.Load(),
		"the PLC listener was reached %d times rather than once", plc.requests.Load())
}

// TestDefaultConfig_KeepsItsOwnTimeout guards the setting the shared client
// would otherwise swallow.
//
// oauth.NewSSRFSafeHTTPClient ships a 15s ceiling. This package has always used
// 10s, and factory_test.go pins it with the reason ("an unbounded client here
// hangs an ingestion worker on an unresponsive PDS"). Restated here against the
// hatched construction too, because a conversion that restores the timeout on
// one branch and not the other changes behaviour only in dev, where nobody is
// measuring.
func TestDefaultConfig_KeepsItsOwnTimeout(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	for name, config := range map[string]Config{
		"guarded": DefaultConfig(),
		"hatched": DefaultConfig(WithPrivateHostsAllowed()),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, config.httpClient, "the config must carry a client")
			assert.Equalf(t, 10*time.Second, config.httpClient.Timeout,
				"the %s client runs on a %v timeout instead of this package's 10s. The shared SSRF "+
					"client brings a 15s ceiling of its own, so adopting it without re-applying the "+
					"caller's value re-times every identity lookup in the AppView as a side effect of "+
					"an SSRF fix", name, config.httpClient.Timeout)
		})
	}
}
