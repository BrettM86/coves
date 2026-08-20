package users

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The profile backfill's PDS fetch, from a goroutine nobody is waiting on.
//
// # WHY THIS SITE IS DIFFERENT FROM THE OTHER EIGHT
//
// Every other fetch in this remediation returns its error to a caller who does
// something with it — a handler maps it to a status, a service wraps it, a test
// asserts on it. This one runs in `go s.backfillProfile(context.WithoutCancel(ctx), …)`.
// Nothing waits on it, nothing retries it, and its only output is a log line. So
// the SSRF here is invisible in both directions: an attacker gets no response
// body back, and an operator gets no signal that anything was attempted.
//
// That asymmetry is why this file asserts TWO separate properties. The guard
// must refuse the address, and the refusal must be OBSERVABLE — because a
// security control in a detached goroutine that fails silently is
// indistinguishable, from outside, from one that was never wired.
//
// # WHAT THE FETCH IS POINTED AT
//
// `pdsURL` comes from the indexed user's record. Users are indexed from the
// firehose as well as from login, so the value arrives from another instance's
// account data — a stranger's choice, resolved and dialled from inside the
// AppView's own network.

const (
	backfillGuardDID = "did:plc:backfillguard"

	// backfillGuardHost passes every shape check this path applies and is a name
	// rather than an address, so classification is the only thing that can refuse
	// it. `.example` is reserved by RFC 2606, so nothing resolves it for real if
	// the seam is ever bypassed.
	backfillGuardHost = "https://user-pds.example"
)

// countingPDS records whether the backfill ever reached a listener.
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

// TestFetchProfileRecord_RefusesAPrivatePDSWithoutReachingIt is the binding
// contract for the fetch itself.
//
// It drives FetchProfileRecord rather than the goroutine because the goroutine
// swallows the error by design; the observability test below is what covers
// that half.
func TestFetchProfileRecord_RefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	_, err := FetchProfileRecord(context.Background(),
		NewProfileBackfillClient(false), pds.server.URL, backfillGuardDID)

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. The PDS URL comes from an indexed user's record, so a "+
			"stranger chose this address, and the request leaving the process is the SSRF whatever "+
			"comes back", pds.requests.Load())

	require.Error(t, err,
		"the backfill fetched a loopback address successfully. Nothing waits on this goroutine, so "+
			"in production this would have happened with no trace at all")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the fetch failed, but not because the guard refused the address. A client that simply could "+
			"not reach the host fails identically, so without the guard's own identity this "+
			"assertion would pass against a build where NewProfileBackfillClient was never "+
			"converted; got: %v", err)
}

// TestNewProfileBackfillClient_ReachesThePDSWhenTheHatchIsOpen is the other
// direction, and the falsifiability control for the case above.
func TestNewProfileBackfillClient_ReachesThePDSWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	input, err := FetchProfileRecord(context.Background(),
		NewProfileBackfillClient(true), pds.server.URL, backfillGuardDID)

	require.NoErrorf(t, err,
		"the hatch is what a dev stack depends on: NewProfileBackfillClient(true) must reach a "+
			"loopback PDS; got: %v", err)
	require.NotNil(t, input, "the fixture serves a displayName, so a profile must come back")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestNewProfileBackfillClient_PreservesTheConfiguredTimeout guards the setting
// the shared client would otherwise swallow.
//
// The 10s here is not an ordinary timeout: backfillProfile detaches from the
// caller's context with context.WithoutCancel, so profileBackfillTimeout is the
// ONLY deadline this goroutine has. Inheriting the shared client's 15s would
// silently extend every backfill, and there is no request lifetime left to bound
// it.
func TestNewProfileBackfillClient_PreservesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	client := NewProfileBackfillClient(false)

	require.NotNil(t, client, "the gate must return a client")
	assert.Equalf(t, profileBackfillTimeout, client.Timeout,
		"the backfill client runs on a %v timeout instead of profileBackfillTimeout (%v). This "+
			"goroutine is detached from its caller's context, so this value is its only deadline",
		client.Timeout, profileBackfillTimeout)
}

// resolvingBackfillClient builds the client the way production does and then
// replaces only its NAME RESOLUTION, so the client under test is the real one.
func resolvingBackfillClient(t *testing.T, allowPrivateHosts bool, resolvesTo string) *http.Client {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(resolvesTo)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", resolvesTo)

	return newProfileBackfillClient(allowPrivateHosts,
		covesoauth.WithHostResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{ip}, nil
		}))
}

// TestFetchProfileRecord_RefusesAWellFormedHostThatResolvesPrivate is the
// assertion a loopback-literal fixture cannot make: the guard's CLASSIFICATION
// pass, on a name that survives every earlier check.
func TestFetchProfileRecord_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	client := resolvingBackfillClient(t, false, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	_, err := FetchProfileRecord(context.Background(), client, backfillGuardHost, backfillGuardDID)

	require.Errorf(t, err,
		"%s is a well-formed https host whose DNS answer was 127.0.0.1, and the backfill fetched it "+
			"anyway. A user's PDS URL is chosen by whoever runs their PDS, and they own the zone",
		backfillGuardHost)
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity, or a build where this client was never "+
			"converted looks identical; got: %v", err)
}

// TestFetchProfileRecord_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control for the case above.
//
// Identical client, identical seam, identical host — only the hatch differs.
// With it open the address is no longer refused, so the request proceeds to a
// dial, which fails because nothing is listening on loopback:443. That
// difference is what pins the refusal above to classification rather than to
// this test being unable to make requests at all.
func TestFetchProfileRecord_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	client := resolvingBackfillClient(t, true, "127.0.0.1") // coves:allow-host-literal: with the hatch open this is dialled and refused by the OS

	_, err := FetchProfileRecord(context.Background(), client, backfillGuardHost, backfillGuardDID)

	require.Error(t, err,
		"nothing listens on loopback:443, so this fetch must fail — if it succeeded, the seam is "+
			"not answering with the address this test gave it")
	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either the gate is not "+
			"reaching the client, or the guarded case above proves nothing: a client that refuses "+
			"every address refuses that case too, for a reason unconnected to classification; got: %v",
		err)
}

// populatedRepo answers the one call backfillProfile makes after a SUCCESSFUL
// fetch, and panics on anything else.
//
// It exists because of what the RED run showed: with the fetch unguarded, the
// blocked-path test sailed past the refusal, reached the re-check at
// service.go:597 and panicked on a nil repository — a segfault instead of a
// readable failure. The embedded nil interface keeps that property for every
// OTHER method, which is the repo's convention (see aggregator's fakeUserService):
// a call nobody predicted panics immediately rather than returning a zero value
// that quietly changes what the test proves.
//
// GetByDID reports a user who already has a display name, so the
// firehose-won-the-race branch returns before any write. That keeps these tests
// about the fetch and its log line, and nothing else.
type populatedRepo struct {
	UserRepository
}

func (populatedRepo) GetByDID(context.Context, string) (*User, error) {
	return &User{DID: backfillGuardDID, DisplayName: "already indexed by the firehose"}, nil
}

// TestBackfillProfile_ABlockedFetchIsObservable is the second half of this
// site's contract, and the one the other eight call sites do not need.
//
// # WHY A SILENT REFUSAL IS ITS OWN DEFECT
//
// backfillProfile runs detached: no caller receives its error, no retry
// consumes it, no status code reflects it. If a blocked fetch produced nothing,
// then from outside the process a guarded build and an unguarded one would be
// indistinguishable — and so would a working backfill and one that has been
// refusing every user since a config change. An operator's only evidence that
// this control exists is the log line, which makes the log line part of the
// control rather than decoration around it.
//
// # WHY THIS TEST IS NOT PARALLEL
//
// It swaps the default slog handler, which is process-global. slog.SetDefault is
// the only seam here because backfillProfile logs through the package-level
// slog.Warn rather than an injected logger — which is itself worth knowing: it
// is why this assertion has to reach for global state, and why a future change
// that gives userService its own *slog.Logger would make this test better.
func TestBackfillProfile_ABlockedFetchIsObservable(t *testing.T) {
	pds := newCountingPDS(t)

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	service := &userService{
		userRepo:              populatedRepo{},
		profileBackfillClient: NewProfileBackfillClient(false),
	}

	// Called synchronously. IndexUser spawns this in a goroutine, but the
	// behaviour under test is the function's own, and driving it directly is
	// what keeps this test free of the sleep-for-a-goroutine pattern the audit
	// forbids.
	service.backfillProfile(context.Background(), backfillGuardDID, pds.server.URL)

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times", pds.requests.Load())

	output := logged.String()
	require.NotEmptyf(t, output,
		"a blocked profile backfill produced NO log output at all. This goroutine is detached, so "+
			"the log line is the only evidence the refusal ever happened — without it an operator "+
			"cannot tell a guarded build from an unguarded one, or a working backfill from one that "+
			"has been refusing every user since a config change")

	assert.Containsf(t, output, backfillGuardDID,
		"the log line does not name the DID whose backfill was refused, so an operator cannot tell "+
			"which user needs reconciling with cmd/backfill-profiles. Got: %s", output)
	assert.Containsf(t, output, "SSRF blocked",
		"the log line does not say the address was refused by the guard, so a refusal reads as an "+
			"ordinary network failure and gets triaged as a flaky remote PDS. Got: %s", output)
}

// TestBackfillProfile_ASuccessfulFetchIsAlsoObservable is the control: a
// message-matching assertion is only worth something if the message can differ.
//
// Without this, a backfillProfile that logged the same warning unconditionally —
// or one that could not fetch anything at all — would satisfy the test above.
func TestBackfillProfile_ASuccessfulFetchIsAlsoObservable(t *testing.T) {
	pds := newCountingPDS(t)

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	service := &userService{
		userRepo:              populatedRepo{},
		profileBackfillClient: NewProfileBackfillClient(true),
	}

	service.backfillProfile(context.Background(), backfillGuardDID, pds.server.URL)

	assert.Equalf(t, int64(1), pds.requests.Load(),
		"with the hatch open the backfill must reach the PDS; it was reached %d times",
		pds.requests.Load())
	assert.NotContainsf(t, logged.String(), "SSRF blocked",
		"a backfill that reached its PDS still logged an SSRF refusal, so the assertion in the test "+
			"above matches whatever this function logs and proves nothing. Got: %s", logged.String())
}
