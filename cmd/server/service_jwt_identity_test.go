package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The identity directory behind service-JWT validation.
//
// # WHY THIS IS THE MOST CREDENTIAL-FREE SITE OF THE NINE
//
// buildDualAuth hands this directory to indigo's ServiceAuthValidator, which
// resolves a JWT's `iss` DID in order to find the key that would verify the
// signature. That resolution happens BEFORE the credential is trusted, and it
// has to — there is nothing to verify against until the DID document is in
// hand. So an attacker sends a syntactically valid JWT claiming any `iss` they
// like, and the AppView fetches whatever that DID document points at. No token
// needs to be valid. No account needs to exist. Where the image proxy at least
// required a did:plc to have been minted, this one requires nothing at all.
//
// # WHY A PURE FUNCTION RATHER THAN AN `if` IN buildDualAuth
//
// buildDualAuth is a method on *application, so reaching it means standing up
// wiring with a config, a database and an OAuth client — which nothing in this
// tree does. `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` would take the
// permissive branch even if it could. Extracted, the decision is testable
// without any of that, and T0 becomes the one place in the repository where the
// branch production actually runs is evaluated at all. Do not inline it back.
//
// # WHY REACHABILITY IS ASSERTED
//
// Mutation testing produced a guard that classified correctly, emitted
// a byte-identical message, and refused the request AFTER delivering it. For a
// destination a stranger named, the packet leaving IS the SSRF. Both directions
// below stand up a real listener and count what reached it.

// serviceJWTTestDID is a syntactically valid did:plc. syntax.ParseDID runs
// before any client is touched, so a malformed one would make these tests pass
// for the wrong reason.
const serviceJWTTestDID = "did:plc:z72i7hdynmk6r22z27h6tvur"

// countingPLC is a PLC directory that answers a DID lookup and records whether
// anything ever reached it. It listens on loopback, which is the address class
// the guard exists to refuse, so its counter doubles as the assertion.
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

		// No alsoKnownAs: indigo verifies a DECLARED handle bidirectionally over
		// DNS and HTTPS, and that lookup would leave the machine. With none
		// declared it marks the handle invalid and returns, so this stays local.
		_, _ = w.Write([]byte(`{"id":"` + serviceJWTTestDID + `","service":[{"id":"#atproto_pds",` +
			`"type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.invalid"}]}`))
	}))
	t.Cleanup(plc.server.Close)
	return plc
}

func resolveThroughDirectory(t *testing.T, plc *countingPLC, allowPrivateHosts bool) error {
	t.Helper()

	dir := serviceJWTIdentityDirectory(plc.server.URL, allowPrivateHosts)
	require.NotNil(t, dir, "the directory must be built")

	did, err := syntax.ParseDID(serviceJWTTestDID)
	require.NoError(t, err, "the test's own DID must parse, or the client is never reached")

	_, err = dir.ResolveDID(context.Background(), did)
	return err
}

// TestServiceJWTIdentityDirectory_GuardedRefusesAPrivatePLCWithoutReachingIt is
// the binding contract: false is the production branch, and it must refuse.
func TestServiceJWTIdentityDirectory_GuardedRefusesAPrivatePLCWithoutReachingIt(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	err := resolveThroughDirectory(t, plc, false)

	require.Errorf(t, err,
		"the service-JWT directory resolved a DID against a PLC on loopback with the hatch shut. This "+
			"is the resolution an unauthenticated caller triggers by sending a JWT with an `iss` of "+
			"their choosing, before anything about that JWT has been verified")
	assert.Containsf(t, err.Error(), "SSRF blocked",
		"the refusal must be the guard's and must say so: indigo wraps this in its own resolution "+
			"error, so the sentence is what an operator has to tell a blocked address from a PLC that "+
			"is merely down; got: %v", err)
	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times. The refusal happened, but it happened AFTER the request "+
			"was delivered — which prevents none of the SSRF", plc.requests.Load())
}

// TestServiceJWTIdentityDirectory_HatchedReachesAPrivatePLC is the other
// direction. a.cfg.Identity.PLCURL is a local PLC in dev, so this half is what
// keeps service-JWT auth working on a developer's machine — and unlike
// productionPLCResolver, this site genuinely needs it.
func TestServiceJWTIdentityDirectory_HatchedReachesAPrivatePLC(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	err := resolveThroughDirectory(t, plc, true)

	require.NoErrorf(t, err,
		"the service-JWT directory could not reach a local PLC with the hatch open. In dev, "+
			"a.cfg.Identity.PLCURL IS a loopback PLC, so without this every aggregator's service JWT "+
			"fails to validate locally; got: %v", err)
	assert.Equalf(t, int64(1), plc.requests.Load(),
		"the PLC listener was reached %d times rather than once", plc.requests.Load())
}

// TestServiceJWTIdentityDirectory_PreservesItsOwnTimeout guards the setting the
// shared client would otherwise swallow.
//
// identityHTTPTimeout is 10s and bounds DID fetches made while validating a
// credential nobody has authenticated yet — so it is a denial-of-service bound
// as much as a latency one. oauth.NewSSRFSafeHTTPClient ships 15s, and silently
// re-timing this as a side effect of an SSRF fix is a second change wearing the
// first one's clothes.
//
// It is also where an ORDERING mistake would show up. indigo's BaseDirectory
// holds an http.Client by VALUE, so the conversion has to dereference the
// pointer the shared constructor returns — and anything set on that client after
// the copy is taken is lost. (The copy itself is safe: an http.Client's
// mutex-bearing state lives behind the Transport pointer.)
func TestServiceJWTIdentityDirectory_PreservesItsOwnTimeout(t *testing.T) {
	t.Parallel()

	for name, allowPrivate := range map[string]bool{"guarded": false, "hatched": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := serviceJWTIdentityDirectory("https://plc.example.invalid", allowPrivate)
			require.NotNil(t, dir)

			assert.Equalf(t, identityHTTPTimeout, dir.HTTPClient.Timeout,
				"the %s directory runs on a %v timeout instead of identityHTTPTimeout (%v). This client "+
					"is reached by unauthenticated callers, so its ceiling is what bounds how long one of "+
					"them can hold a request open", name, dir.HTTPClient.Timeout, identityHTTPTimeout)
		})
	}
}

// TestServiceJWTIdentityDirectory_KeepsThePLCURLItWasGiven pins the other half
// of the construction, because a guard that quietly changed where this
// directory points would be a worse bug than the one being fixed: the PLC URL
// is what decides which network's DIDs this instance believes.
func TestServiceJWTIdentityDirectory_KeepsThePLCURLItWasGiven(t *testing.T) {
	t.Parallel()

	const plcURL = "https://plc.example.invalid"

	dir := serviceJWTIdentityDirectory(plcURL, false)
	require.NotNil(t, dir)

	assert.Equal(t, plcURL, dir.PLCURL,
		"the directory must resolve against the configured PLC directory and nothing else")
}

// TestServiceJWTIdentityDirectory_GuardIsNotAmbient states the invariant cycle 2
// found the hard way, at this site too.
//
// The decision belongs to the CALLER — buildDualAuth passes a.cfg.IsDevEnv —
// and must never become a read of the environment inside the constructor. This
// process has IS_DEV_ENV set to true and the guarded construction must still
// refuse, because cmd/server also builds a resolver aimed at the public
// directory that has to stay guarded in dev.
func TestServiceJWTIdentityDirectory_GuardIsNotAmbient(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "true")

	plc := newCountingPLC(t)

	err := resolveThroughDirectory(t, plc, false)

	require.Error(t, err,
		"the guarded directory reached a loopback PLC while IS_DEV_ENV was true. The hatch must be a "+
			"property of the argument, not of the environment: an ambient read opens every other "+
			"construction in this process at the same time")
	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times with IS_DEV_ENV=true", plc.requests.Load())
}
