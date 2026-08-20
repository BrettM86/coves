package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
)

// The reconciliation job's PDS fetch — the same request the AppView's detached
// backfill makes, from a process nothing else is watching.
//
// # WHY A CLI IS NOT A LESSER CALL SITE
//
// `u.pdsURL` is read straight out of the `users` table, so it is the value some
// other instance's account data put there — the same attacker-influenced input
// the AppView's own backfill was converted for. What differs is the
// surroundings, and every difference makes it worse rather than better:
//
//   - It is run BY HAND against production, by an operator reconciling a real
//     incident, so a request to an internal address goes out from an
//     interactive session with a human's attention on the summary line.
//   - It fetches for EVERY bare user at once, four at a time — one hostile PDS
//     URL in the table is dialled on the next run, whoever triggers it.
//   - Its failures are one `log.Printf` per user among thousands, and the
//     process exits 1 on any failure, so a refused address reads as ordinary
//     churn.
//
// # WHY THE CLIENT MOVED OUT OF main()
//
// It was `&http.Client{Timeout: 15 * time.Second}` on a line inside main(),
// which no test can reach: main() opens a database, queries it and blocks on a
// worker pool. newProfileFetchClient is the smallest extraction that makes the
// construction addressable — it takes the gate boolean main's flag supplies and
// the option seam these tests need, and main keeps every other line it had.

const (
	backfillCLIDID = "did:plc:backfillcliguard"

	// backfillCLIHost passes every shape check this path applies and is a name
	// rather than an address, so classification is the only thing that can refuse
	// it. `.example` is reserved by RFC 2606, so nothing resolves it for real if
	// the seam is ever bypassed.
	backfillCLIHost = "https://user-pds.example"
)

// countingPDS records whether the job ever reached a listener.
type countingPDS struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingPDS(t *testing.T) *countingPDS {
	t.Helper()

	pds := &countingPDS{}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pds.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":{"displayName":"leaked from an internal endpoint"}}`))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// TestNewProfileFetchClient_RefusesAPrivatePDSWithoutReachingIt is the binding
// contract for this site.
//
// It drives users.FetchProfileRecord — the call processUser actually makes — so
// what is under test is the client as the job uses it, not the constructor in
// isolation.
func TestNewProfileFetchClient_RefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	_, err := users.FetchProfileRecord(context.Background(),
		newProfileFetchClient(false), pds.server.URL, backfillCLIDID)

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. The PDS URL comes straight from the users table, so "+
			"whoever wrote that row chose this address, and the request leaving the process is the "+
			"SSRF whatever comes back", pds.requests.Load())

	require.Error(t, err,
		"the job fetched a loopback address successfully. This runs by hand against production, "+
			"and a dialled internal address arrives as one log line among thousands")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the fetch failed, but not because the guard refused the address. A PDS that simply could "+
			"not be reached fails identically, so without the guard's own identity this assertion "+
			"passes against the current build, where the client is a bare http.Client; got: %v", err)
}

// TestNewProfileFetchClient_ReachesThePDSWhenTheHatchIsOpen is the other
// direction, and the falsifiability control for the case above.
//
// The hatch is not decoration here: DATABASE_URL defaults to the LOCAL DEV
// database, so running this job against a developer's own stack — where the PDS
// is on loopback — is the documented default invocation. Guarding it without a
// flag would break the only usage that needs no environment at all.
func TestNewProfileFetchClient_ReachesThePDSWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	input, err := users.FetchProfileRecord(context.Background(),
		newProfileFetchClient(true), pds.server.URL, backfillCLIDID)

	require.NoErrorf(t, err,
		"with -allow-private-hosts the job must reach a loopback PDS, which is what the local dev "+
			"database this tool defaults to is paired with; got: %v", err)
	require.NotNil(t, input, "the fixture serves a displayName, so a profile must come back")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestNewProfileFetchClient_PreservesTheConfiguredTimeout guards the setting the
// shared client would otherwise swallow.
//
// NewSSRFSafeHTTPClient ships a 15s ceiling of its own, which happens to equal
// this one TODAY — which is exactly why the assertion is against
// profileFetchTimeout rather than against a literal. The two values are
// independent, and a conversion that drops this one leaves no visible trace
// until the shared default moves and every fetch in this job silently re-times
// with it.
func TestNewProfileFetchClient_PreservesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	client := newProfileFetchClient(false)

	require.NotNil(t, client, "the gate must return a client")
	assert.Equalf(t, profileFetchTimeout, client.Timeout,
		"the job's client runs on a %v timeout instead of profileFetchTimeout (%v). processUser "+
			"wraps each fetch in a 20s context, and this value has always sat below it so a stalled "+
			"PDS is attributed to the fetch rather than to the context expiring",
		client.Timeout, profileFetchTimeout)
}

// resolvingProfileFetchClient builds the client the way main does and then
// replaces only its NAME RESOLUTION, so the client under test is the real one.
func resolvingProfileFetchClient(t *testing.T, allowPrivateHosts bool, resolvesTo string) *http.Client {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(resolvesTo)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", resolvesTo)

	return newProfileFetchClient(allowPrivateHosts,
		covesoauth.WithHostResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{ip}, nil
		}))
}

// TestNewProfileFetchClient_RefusesAWellFormedHostThatResolvesPrivate is the
// assertion a loopback-literal fixture cannot make: the guard's CLASSIFICATION
// pass, on a name that survives every earlier check.
func TestNewProfileFetchClient_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	client := resolvingProfileFetchClient(t, false, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	_, err := users.FetchProfileRecord(context.Background(), client, backfillCLIHost, backfillCLIDID)

	require.Errorf(t, err,
		"%s is a well-formed https host whose DNS answer was 127.0.0.1, and the job fetched it "+
			"anyway. A user's PDS URL is chosen by whoever runs their PDS, and they own the zone",
		backfillCLIHost)
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity, or a build where this client was never "+
			"converted looks identical; got: %v", err)
}

// TestNewProfileFetchClient_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control for the case above.
//
// Identical constructor, identical seam, identical host — only the hatch
// differs. With it open the address is no longer refused, so the request
// proceeds to a dial, which fails because nothing is listening on loopback:443.
// That difference is what pins the refusal above to classification rather than to
// this test being unable to make requests at all.
func TestNewProfileFetchClient_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	client := resolvingProfileFetchClient(t, true, "127.0.0.1") // coves:allow-host-literal: with the hatch open this is dialled and refused by the OS

	_, err := users.FetchProfileRecord(context.Background(), client, backfillCLIHost, backfillCLIDID)

	require.Error(t, err,
		"nothing listens on loopback:443, so this fetch must fail — if it succeeded, the seam is "+
			"not answering with the address this test gave it")
	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either the gate is not "+
			"reaching the client, or the guarded case above proves nothing: a client that refuses "+
			"every address refuses that case too, for a reason unconnected to classification; got: %v",
		err)
}
