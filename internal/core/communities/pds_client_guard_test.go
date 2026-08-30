package communities

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	covesoauth "Coves/internal/atproto/oauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The four xrpc.Client sites in this package, which were unguarded by OMISSION
// rather than by construction.
//
// # WHY THESE FOUR LOOKED SAFE
//
// Every other site in this remediation held a visible `&http.Client{}`. These
// four hold `&xrpc.Client{Host: pdsURL}` and leave the optional `.Client` field
// nil — and indigo's getClient() (xrpc/xrpc.go:31-36) then substitutes
// util.RobustHTTPClient() on every call. So the unguarded client is real, is
// used on every request, and appears in this repository's source not at all.
// Nothing to grep for is why the audit's `&http.Client{` sweep walked past them.
//
// # TWO OF THEM CARRY LIVE CREDENTIALS TO THE HOST THEY DIAL
//
// refreshPDSToken sends a community's PDSRefreshToken as the Authorization
// header. reauthenticateWithPassword POSTs the community's cleartext PDSPassword
// and PDSEmail. Both are on the wire before any response exists, so "the address
// was dialled" and "a live PDS credential left the process" are the same event.
// A refused address costs an attacker a retry; a leaked password does not come
// back.
//
// # THE HOST IS OPERATOR-PINNED TODAY AND THE FILE PLANS OTHERWISE
//
// pdsURL is fresh.PDSURL, a per-community database column, which today is always
// this instance's own PDS. pds_provisioning.go's own doc comment describes V2.1
// portability to non-Coves PDSs, and that is the commit where this column starts
// carrying a value someone else chose. Guarding it now costs nothing; guarding it
// then means remembering.

// recordingPDS is a loopback PDS that records whether it was ever reached.
//
// THE ASSERTION IS "NEVER INVOKED", not "an error came back". A guard that
// refuses AFTER delivering the request is byte-identical from the caller's side
// and useless — that exact mutation survived every error-message assertion in
// an earlier mutation test, and only the reached/not-reached counter caught it. It matters more
// here than anywhere else in this remediation, because what the request carries
// is the credential.
func recordingPDS(t *testing.T) (url string, reached *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"did":"did:web:pds.example","accessJwt":"a","refreshJwt":"r","handle":"h"}`))
	}))
	t.Cleanup(server.Close)
	return server.URL, &hits
}

// resolverOption points the guarded client's NAME RESOLUTION at a chosen
// address, so the classification pass can be driven by a hostname that is
// otherwise well-formed. It cannot open the guard.
func resolverOption(t *testing.T, answer string) PDSClientOption {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd literal would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(answer)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", answer)

	return withTransportOptions(covesoauth.WithHostResolver(
		func(context.Context, string) ([]net.IP, error) { return []net.IP{ip}, nil }))
}

func TestRefreshPDSToken_RefusesAPrivatePDSWithoutSendingTheRefreshToken(t *testing.T) {
	t.Parallel()

	pdsURL, reached := recordingPDS(t)

	_, _, err := refreshPDSToken(context.Background(), newPDSHTTPClient(), pdsURL, "the-refresh-token")

	// THE REACHABILITY CLAIM COMES FIRST, and the ordering is load-bearing rather
	// than stylistic. require aborts the test, so asserting the error first means
	// that under the mutation this test exists to catch — deleting
	// `retryClient.HTTPClient = inner`, which leaves retryablehttp's own
	// cleanhttp.DefaultPooledClient in place and the request succeeding — the
	// failure reads "An error is expected but got nil" and says NOTHING about
	// whether the refresh token left the process. That is the one fact a reader of
	// this failure most needs. Verified by running that mutation, not assumed.
	assert.Zerof(t, reached.Load(),
		"the PDS listener was reached %d time(s). refreshPDSToken sends the community's refresh token "+
			"as the Authorization header, so a guard that refuses AFTER the request is indistinguishable "+
			"from no guard: the credential is already gone", reached.Load())

	require.Error(t, err, "a PDS on loopback must be refused when the hatch is shut")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal does not carry the guard's identity, so a build where xrpc.Client's nil .Client "+
			"field is still falling back to util.RobustHTTPClient() looks the same — some error would "+
			"come back either way; got: %v", err)
}

// TestRefreshPDSToken_RefusesAWellFormedHostThatResolvesPrivate is the
// assertion a loopback-literal fixture cannot make.
//
// The transport refuses an IP LITERAL on shape, one branch before it classifies
// anything, so the test above passes against an implementation that only checks
// shape. A hostname that resolves privately is the input that reaches
// classification, and it is the cheapest SSRF here once PDSURL carries a value
// somebody else chose: the attacker owns the zone.
func TestRefreshPDSToken_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	client := newPDSHTTPClient(resolverOption(t, "127.0.0.1")) // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	_, _, err := refreshPDSToken(context.Background(), client,
		"https://pds.aggregator.example", "the-refresh-token")

	require.Error(t, err, "a well-formed host resolving to loopback must be refused")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"a hostname whose DNS answer was 127.0.0.1 was not refused by CLASSIFICATION. This is the "+
			"input an IP-literal fixture cannot produce and the one an attacker controlling a zone "+
			"actually uses; got: %v", err)
}

// TestRefreshPDSToken_DoesNotMistakeAGuardRefusalForAnExpiredToken is a
// deterministic repro of a flake, and the flake is a real classification bug.
//
// refreshPDSToken classifies an expired refresh token twice: once from the typed
// *xrpc.Error, and then again by searching the error's TEXT for "401" or
// "Unauthorized". The second check is the problem, and it is worse than
// redundant, because the branch it takes does not wrap: it discards the original
// error and returns a fresh one saying "refresh token expired or invalid (needs
// password re-auth)". Whatever chain the error carried is gone.
//
// A transport error's text contains the URL that was dialled, port included. So
// any PDS on a port whose digits happen to contain "401" — 401, 4010, 34013,
// 8401, 14019 — produces a guard refusal that this function reports as an
// expired credential, with ErrBlockedAddress no longer in the chain.
//
// # WHY IT SURFACED AS A RANDOM CI FAILURE
//
// TestRefreshPDSToken_RefusesAPrivatePDSWithoutSendingTheRefreshToken points at
// an httptest listener, and httptest takes whatever ephemeral port the kernel
// hands it. Roughly one run in forty draws a port containing those three digits,
// and on that run the ErrorIs assertion fails while every other run passes. The
// test was not flaky; the code is nondeterministic in a way the test happened to
// sample.
//
// This case removes the sampling. The port is written down, no listener is
// needed — the guard refuses an IP literal on shape, one branch before it would
// dial — so the collision happens on every run instead of on 2% of them.
//
// # WHY MISREPORTING IT IS SERIOUS ON ITS OWN
//
// The two diagnoses call for opposite actions. "Refresh token expired" tells the
// operator to re-authenticate with the stored password — which is
// reauthenticateWithPassword, the call that POSTs the community's CLEARTEXT
// password to that same address. So a refused address does not merely produce a
// confusing message: it invites the caller to retry with a far worse payload
// against the host the guard just refused.
func TestRefreshPDSToken_DoesNotMistakeAGuardRefusalForAnExpiredToken(t *testing.T) {
	t.Parallel()

	// coves:allow-host-literal: never dialled — the guard refuses a loopback
	// literal on shape. The port is the fixture: its digits contain "401".
	const pdsURL = "http://127.0.0.1:34013" // coves:allow-host-literal: the port must contain the digits 401 to reproduce the misclassification, and the guard refuses before anything dials it
	require.Contains(t, pdsURL, "401",
		"this test is only meaningful if the URL the guard names contains the digits the string "+
			"fallback searches for; a port change here quietly turns it into a duplicate of the "+
			"loopback case above")

	_, _, err := refreshPDSToken(context.Background(), newPDSHTTPClient(), pdsURL, "the-refresh-token")

	require.Error(t, err, "a PDS on loopback must be refused")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the guard's refusal was rewritten as an expired credential and its identity dropped from the "+
			"chain, because the dialled URL in the message contains \"401\". Nothing above this can "+
			"tell a blocked address from a dead token any more — and the remedy for a dead token is "+
			"reauthenticateWithPassword, which POSTs the community's cleartext password to the very "+
			"address that was just refused; got: %v", err)
}

// TestRefreshPDSToken_ReportsARealUnauthorizedAsAnExpiredToken is the fence for
// the case above.
//
// The friendly message is right and must survive: a 401 from the PDS genuinely
// does mean the refresh token is spent, and the caller's next step genuinely is
// a password re-auth. Deleting the string fallback must not take the diagnosis
// with it — the typed *xrpc.Error branch already carries it, and this says so.
//
// The hatch is open here because the fixture has to be REACHED: this is the one
// case in this file that needs a real response rather than a refusal, so it is
// the one place the guard is deliberately out of the way.
func TestRefreshPDSToken_ReportsARealUnauthorizedAsAnExpiredToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"ExpiredToken","message":"Token has expired"}`))
	}))
	t.Cleanup(server.Close)

	client := newPDSHTTPClient(WithPrivateHostsAllowed()) // coves:allow-ssrf-hatch: the fixture must be reached; a refusal here would assert nothing about 401 handling

	_, _, err := refreshPDSToken(context.Background(), client, server.URL, "the-spent-refresh-token")

	require.Error(t, err)
	assert.ErrorContains(t, err, "needs password re-auth",
		"a genuine 401 must still be reported as a spent refresh token: it is the one diagnosis that "+
			"tells the caller to fall back to reauthenticateWithPassword, and the typed *xrpc.Error "+
			"branch is what has to carry it once the string search is gone")
	assert.NotErrorIs(t, err, covesoauth.ErrBlockedAddress,
		"a real 401 is not a guard refusal")
}

func TestReauthenticateWithPassword_RefusesAPrivatePDSWithoutSendingThePassword(t *testing.T) {
	t.Parallel()

	pdsURL, reached := recordingPDS(t)

	_, _, err := reauthenticateWithPassword(context.Background(), newPDSHTTPClient(),
		pdsURL, "c-test@example.com", "the-cleartext-password")

	// Reachability first — see the note in the refresh-token case above.
	assert.Zerof(t, reached.Load(),
		"the PDS listener was reached %d time(s). This call POSTs the community's CLEARTEXT password "+
			"and email in the request body — the worst payload in this package to deliver to an address "+
			"of someone else's choosing", reached.Load())

	require.Error(t, err, "a PDS on loopback must be refused when the hatch is shut")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal does not carry the guard's identity; got: %v", err)
}

func TestProvisionCommunityAccount_RefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pdsURL, reached := recordingPDS(t)

	_, err := NewPDSAccountProvisioner("coves.example", pdsURL).
		ProvisionCommunityAccount(context.Background(), "guardtest")

	// Reachability first — see the note in the refresh-token case above.
	assert.Zerof(t, reached.Load(),
		"the PDS listener was reached %d time(s). createAccount POSTs a generated password and the "+
			"community's system email", reached.Load())

	require.Error(t, err, "a PDS on loopback must be refused when the hatch is shut")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal does not carry the guard's identity; got: %v", err)
}

func TestFetchPDSDID_RefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pdsURL, reached := recordingPDS(t)

	_, err := FetchPDSDID(context.Background(), pdsURL)

	// Reachability first — see the note in the refresh-token case above.
	assert.Zerof(t, reached.Load(),
		"the PDS listener was reached %d time(s)", reached.Load())

	require.Error(t, err, "a PDS on loopback must be refused when the hatch is shut")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal does not carry the guard's identity; got: %v", err)
}

// TestNewPDSHTTPClient_TheHatchOpensIt is the falsifiability control. Without
// it, a client that could not make any request at all — a transport wired to
// nothing, a fixture that never starts — would satisfy every refusal above.
func TestNewPDSHTTPClient_TheHatchOpensIt(t *testing.T) {
	t.Parallel()

	pdsURL, reached := recordingPDS(t)

	_, err := FetchPDSDID(context.Background(), pdsURL, PrivateHostOptions(true)...)

	require.NoError(t, err, "with the hatch open the same loopback PDS must answer")
	assert.Positivef(t, reached.Load(),
		"the listener was never reached even with the hatch OPEN, so the refusals above prove nothing "+
			"about classification — this client cannot make requests at all")
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// only place in this repository where the branch production actually runs is
// evaluated. `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the
// PERMISSIVE branch at every call site holding such a boolean.
//
// ZERO, not "options that are safe": what production gets must be exactly the
// constructor's own defaults, which is a claim a reader can check in one glance.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	assert.Empty(t, PrivateHostOptions(false),
		"PrivateHostOptions(false) returned options. The production branch must apply NOTHING, so that "+
			"a reviewer reading newPDSHTTPClient sees the whole of what production runs")
	assert.Len(t, PrivateHostOptions(true), 1,
		"PrivateHostOptions(true) must return exactly the hatch")
}

// TestNewPDSHTTPClient_PreservesTheRobustClientsTimeout pins the ceiling these
// four sites have always run under.
//
// indigo's util.RobustHTTPClient sets 30s. The shared SSRF client ships 15s, so
// adopting it without re-applying would HALVE the allowance for every community
// provisioning and token refresh in the AppView, as a silent side effect of an
// SSRF fix — a second change wearing the first one's clothes.
func TestNewPDSHTTPClient_PreservesTheRobustClientsTimeout(t *testing.T) {
	t.Parallel()

	assert.Equalf(t, 30*time.Second, newPDSHTTPClient().Timeout,
		"the PDS client runs on a %v ceiling. indigo's util.RobustHTTPClient — what xrpc.Client's nil "+
			".Client field used to fall back to — allows 30s across all retries, and that is the "+
			"behaviour this conversion has to preserve", newPDSHTTPClient().Timeout)
}

// TestNewPDSHTTPClient_KeepsTheRetryBehaviourItReplaces is the other half of
// "preserve what was there".
//
// util.RobustHTTPClient retries connection errors and 5xx up to three times.
// Dropping that while fixing an SSRF hole would make a single transient blip
// fail a community's creation or its token refresh — a regression arriving
// inside a security fix and attributable to nothing.
func TestNewPDSHTTPClient_KeepsTheRetryBehaviourItReplaces(t *testing.T) {
	t.Parallel()

	// ONE failure, not three: retryablehttp's first backoff is a full second, so
	// each extra attempt costs this T0 test a second of wall clock to re-prove
	// something the first retry already proved.
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"did":"did:web:pds.example"}`))
	}))
	t.Cleanup(server.Close)

	did, err := FetchPDSDID(context.Background(), server.URL, PrivateHostOptions(true)...)

	require.NoError(t, err, "a transient 500 must be ridden out, as util.RobustHTTPClient rode it out")
	assert.Equal(t, "did:web:pds.example", did)
	assert.EqualValuesf(t, 2, attempts.Load(),
		"the PDS was called %d time(s). util.RobustHTTPClient retries 5xx, and these four sites have "+
			"always had that; a conversion that silently drops it turns one bad gateway response into a "+
			"failed community provisioning or a failed token refresh", attempts.Load())
}

// TestNewPDSHTTPClient_DoesNotRetryTheGuardsRefusal is the other side of keeping
// the retry wrapper: retryablehttp's default treats every transport-level error
// as transient, and a refused address is not one.
//
// Without this the guard's decision is taken four times, 1s + 2s + 4s of backoff
// are spent on a request somebody is waiting behind, and four log lines suggest a
// flaky network where there is a deliberate block. Asserted on the CLOCK because
// the retry attempts are invisible from the outside — the listener is never
// reached, so a counter cannot see them.
func TestNewPDSHTTPClient_DoesNotRetryTheGuardsRefusal(t *testing.T) {
	t.Parallel()

	pdsURL, _ := recordingPDS(t)

	start := time.Now()
	_, err := FetchPDSDID(context.Background(), pdsURL)
	elapsed := time.Since(start)

	require.Error(t, err, "a PDS on loopback must be refused when the hatch is shut")
	assert.Lessf(t, elapsed, time.Second,
		"the refusal took %v. retryablehttp's default policy re-dials a transport error after 1s, 2s "+
			"and 4s, so a blocked address is being classified four times and answered seven seconds "+
			"late — with four warnings implying a flaky network rather than one saying the address was "+
			"refused", elapsed)
}

// TestNewCommunityService_BuildsAGuardedPDSClientByDefault pins the wiring, not
// the helper: a service constructed with no options must hold a guarded client,
// because that is what forgetting looks like and forgetting has to be safe.
func TestNewCommunityService_BuildsAGuardedPDSClientByDefault(t *testing.T) {
	t.Parallel()

	svc, ok := NewCommunityService(nil, "https://pds.example", "did:web:coves.example",
		"coves.example", nil, nil, nil).(*communityService)
	require.True(t, ok, "NewCommunityService must return the concrete service these tests drive")
	require.NotNil(t, svc.pdsHTTPClient, "the service must hold a PDS client, not nil")

	pdsURL, reached := recordingPDS(t)
	_, _, err := refreshPDSToken(context.Background(), svc.pdsHTTPClient, pdsURL, "the-refresh-token")

	// Reachability first — see the note in the refresh-token case above.
	assert.Zerof(t, reached.Load(), "the service's client reached the listener %d time(s)", reached.Load())

	require.Error(t, err, "the service's own client must refuse a loopback PDS")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the service's default PDS client is not the guarded one; got: %v", err)
}
