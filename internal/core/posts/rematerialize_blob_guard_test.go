package posts

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The re-materialization tool's blob copy, and the input nobody reads as
// attacker-controlled because of where it arrives from.
//
// # WHERE THE HOST COMES FROM
//
// Fetch and Present both take `host`, and every production caller gets it from
// hostURLOf(repo) — HostURL() on the author's repo, which is the
// serviceEndpoint of that author's DID document. Anyone can mint a DID document
// and put any URL in it. So this is the same input class as the image proxy's
// pdsURL and the profile backfill's PDS URL, arriving by a route that reads like
// internal plumbing: a repo handle the tool already opened, not a string off the
// wire.
//
// # WHY THE DELAY MAKES IT WORSE, NOT BETTER
//
// This is an operator-run batch tool, so an attacker cannot fire it. They do not
// need to. They plant the serviceEndpoint and wait — the migration runs once, by
// hand, during a maintenance window, at whatever hour the operator picked, with
// -yes already passed and 4,131 records scrolling past. A refused address and a
// dialled one look the same in that output. Nothing about the timing reduces the
// exposure; it only guarantees nobody is watching the request that matters.
//
// # WHAT THE COMMENT AT THE CONSTRUCTOR ALREADY SAYS
//
// DefaultRematerializeBlobClient documents why http.DefaultClient was rejected:
// no timeout, so a half-open socket hangs the whole run. That is one half of
// "which client may this tool dial with". The address guard is the other half,
// and it was missing from the same three lines that argued the first one.

const (
	// rematerializeGuardDID and rematerializeGuardCID are shaped like the real
	// thing so HydrateBlobURL builds a URL; neither participates in the refusal.
	rematerializeGuardDID = "did:plc:rematerializeguard22222"
	rematerializeGuardCID = "bafkreirematerializeguard"

	// rematerializeGuardHost passes every check this path applies — HydrateBlobURL
	// only refuses empty strings — and is a NAME rather than an address, so
	// classification is the only thing that can refuse it. `.example` is reserved
	// by RFC 2606, so nothing resolves it for real if the seam is ever bypassed.
	rematerializeGuardHost = "https://author-pds.example"
)

// countingBlobHost records whether the copy ever reached a listener.
type countingBlobHost struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingBlobHost(t *testing.T) *countingBlobHost {
	t.Helper()

	host := &countingBlobHost{}
	host.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		host.requests.Add(1)
		_, _ = w.Write([]byte("blob bytes from an internal endpoint"))
	}))
	t.Cleanup(host.server.Close)
	return host
}

// TestDefaultRematerializeBlobClient_FetchRefusesAPrivateHostWithoutReachingIt
// is the binding contract for the copy leg.
func TestDefaultRematerializeBlobClient_FetchRefusesAPrivateHostWithoutReachingIt(t *testing.T) {
	t.Parallel()

	host := newCountingBlobHost(t)

	_, err := DefaultRematerializeBlobClient(false).
		Fetch(context.Background(), host.server.URL, rematerializeGuardDID, rematerializeGuardCID)

	assert.Zerof(t, host.requests.Load(),
		"the listener was reached %d times. The host is the author repo's HostURL — a DID "+
			"document's serviceEndpoint, which anyone can mint — so the request leaving the process "+
			"is the SSRF whatever comes back", host.requests.Load())

	require.Error(t, err,
		"the blob copy fetched a loopback address successfully. This runs inside a maintenance "+
			"window with thousands of records scrolling past, so a dialled internal address is "+
			"indistinguishable from a refused one in the tool's output")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the fetch failed, but not because the guard refused the address. A host that simply could "+
			"not be reached fails identically, so without the guard's own identity this assertion "+
			"passes against the current build, where the client is a bare http.Client; got: %v", err)
}

// TestDefaultRematerializeBlobClient_PresentRefusesAPrivateHostWithoutReachingIt
// covers the OTHER method, which is not a duplicate of the one above.
//
// Present is the probe the tool consults before deleting the community's copy of
// a blob, and its contract is that a transport failure is an ERROR and never a
// false. A guard bolted onto Fetch alone would leave the probe dialling; a guard
// whose refusal Present collapsed into "absent" would be worse than no guard at
// all, because "absent" is the answer that licenses a delete.
func TestDefaultRematerializeBlobClient_PresentRefusesAPrivateHostWithoutReachingIt(t *testing.T) {
	t.Parallel()

	host := newCountingBlobHost(t)

	present, err := DefaultRematerializeBlobClient(false).
		Present(context.Background(), host.server.URL, rematerializeGuardDID, rematerializeGuardCID)

	assert.Zerof(t, host.requests.Load(),
		"the presence probe reached the listener %d times", host.requests.Load())

	require.Error(t, err,
		"the presence probe dialled a loopback address. This probe decides whether it is safe to "+
			"delete the only surviving copy of a blob")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity; got: %v", err)
	assert.Falsef(t, present,
		"a refused address was reported as PRESENT. Present is what licenses the delete of the "+
			"community's copy, so a refusal must never read as 'the bytes are there'")
}

// TestDefaultRematerializeBlobClient_ReachesTheHostWhenTheHatchIsOpen is the
// other direction, and the falsifiability control for both cases above: a client
// that could make no request at all would satisfy them just as well.
func TestDefaultRematerializeBlobClient_ReachesTheHostWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	host := newCountingBlobHost(t)

	data, err := DefaultRematerializeBlobClient(true).
		Fetch(context.Background(), host.server.URL, rematerializeGuardDID, rematerializeGuardCID)

	require.NoErrorf(t, err,
		"the hatch is what a dev run of cmd/rematerialize-posts depends on: with it open the copy "+
			"must reach a loopback PDS; got: %v", err)
	assert.NotEmpty(t, data, "the fixture serves bytes, so a successful copy must return them")
	assert.Equalf(t, int64(1), host.requests.Load(),
		"the listener was reached %d times rather than once", host.requests.Load())
}

// TestDefaultRematerializeBlobClient_GuardedIsTheDefaultForTheStateMachine pins
// the branch production actually runs, at the place it is actually taken.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at
// every call site holding such a boolean. Here there is no boolean at all —
// blobClient() falls back to the default whenever Rematerializer.Blobs is nil,
// which is every construction in this package's own tests and every one in
// rematerialize_dryrun.go. So the guarded spelling has to be the one the
// fallback uses, and this is where that is checked.
func TestDefaultRematerializeBlobClient_GuardedIsTheDefaultForTheStateMachine(t *testing.T) {
	t.Parallel()

	host := newCountingBlobHost(t)

	fallback := (&Rematerializer{}).blobClient()
	require.NotNil(t, fallback, "a Rematerializer with no injected Blobs must still have a client")

	_, err := fallback.Fetch(context.Background(), host.server.URL,
		rematerializeGuardDID, rematerializeGuardCID)

	assert.Zerof(t, host.requests.Load(),
		"the state machine's DEFAULT blob client reached the listener %d times. Every caller that "+
			"does not inject one — including the dry run — copies blobs through this client",
		host.requests.Load())
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the fallback client is not the guarded one; got: %v", err)
}

// TestDefaultRematerializeBlobClient_PreservesTheFetchTimeout guards a value the
// shared client would otherwise swallow, and this one is not a courtesy.
//
// NewSSRFSafeHTTPClient ships a 15s ceiling. A single blob here may be 100 MiB,
// which is why this site allows two MINUTES — so adopting the shared client
// without re-applying rematerializeBlobFetchTimeout would not "re-time" the
// copy, it would make large-blob copies fail outright, mid-migration, and be
// reported as a flaky PDS.
func TestDefaultRematerializeBlobClient_PreservesTheFetchTimeout(t *testing.T) {
	t.Parallel()

	client, ok := DefaultRematerializeBlobClient(false).(*httpRematerializeBlobClient)
	require.True(t, ok, "DefaultRematerializeBlobClient must return the concrete client these tests drive")
	require.NotNil(t, client.client, "the blob client must hold an HTTP client")

	assert.Equalf(t, rematerializeBlobFetchTimeout, client.client.Timeout,
		"the blob copy runs on a %v timeout instead of rematerializeBlobFetchTimeout (%v). A blob "+
			"here may be 100 MiB; inheriting the shared client's 15s ceiling would fail those "+
			"copies rather than merely hurrying them",
		client.client.Timeout, rematerializeBlobFetchTimeout)
}

// TestDefaultRematerializeBlobClient_PreservesTheCopyCap is the same trap in the
// other dimension, and it is the one that bites silently.
//
// maxRematerializeBlobBytes is 100 MiB. oauth.DefaultMaxResponseBytes is 32 MiB
// — SMALLER — so unlike every other conversion in this remediation, adopting the
// shared client here TIGHTENS the limit unless the cap is raised explicitly.
// oauth.DefaultMaxResponseBytes documents this obligation by name.
//
// The failure would arrive as an error on a blob between 32 and 100 MiB, in a
// tool that has already re-materialized the post and is about to delete the
// legacy record. That is the worst possible moment for a limit nobody chose.
func TestDefaultRematerializeBlobClient_PreservesTheCopyCap(t *testing.T) {
	t.Parallel()

	client, ok := DefaultRematerializeBlobClient(false).(*httpRematerializeBlobClient)
	require.True(t, ok, "DefaultRematerializeBlobClient must return the concrete client these tests drive")

	assert.Equalf(t, maxRematerializeBlobBytes, client.maxBytes,
		"the production client's own copy cap is %d rather than maxRematerializeBlobBytes (%d)",
		client.maxBytes, maxRematerializeBlobBytes)
}

// TestDefaultRematerializeBlobClient_TheTransportDoesNotClampBelowTheCopyCap is
// the assertion the field check above cannot make.
//
// c.maxBytes is what Fetch enforces after the body arrives. The TRANSPORT has a
// second, independent ceiling, and it refuses an announced Content-Length over
// that ceiling before a byte of body is read — so a client whose maxBytes field
// says 100 MiB can still be unable to receive 40.
//
// It is asserted through an announced length rather than by moving 40 MiB
// through a test: the transport's check is `resp.ContentLength > maxResponseBytes`
// in RoundTrip, which is reached without a body. The declared length is short by
// design, so the read fails either way — what this test reads is WHICH failure,
// and ErrResponseTooLarge is the one that means the shared 32 MiB default is
// still in force.
//
// THE ANNOUNCED LENGTH IS ONE BYTE OVER THE COPY CAP, AND THAT IS THE WHOLE
// TEST. Announcing exactly maxRematerializeBlobBytes was the original fixture,
// and it passed against a transport clipping at exactly that number — which is
// the off-by-one that made Fetch's overrun probe unreachable in production.
// maxBytes+1 is the smallest length the two caps disagree about, so it is the
// only one that can tell them apart. See
// TestDefaultRematerializeBlobClient_AnOverrunKeepsItsCIDExplanation for what
// the extra byte is for.
func TestDefaultRematerializeBlobClient_TheTransportDoesNotClampBelowTheCopyCap(t *testing.T) {
	t.Parallel()

	// The hatch is open because the fixture is on loopback; the cap under test is
	// a different control from the address guard, and this is the only way to get
	// a response header from a real transport in the unit tier.
	const announced = maxRematerializeBlobBytes + 1 // the probing byte, which must reach Fetch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(announced))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a short body under a large declared length"))
	}))
	t.Cleanup(server.Close)

	_, err := DefaultRematerializeBlobClient(true).
		Fetch(context.Background(), server.URL, rematerializeGuardDID, rematerializeGuardCID)

	require.Error(t, err, "the declared length is not delivered, so this fetch fails either way")
	assert.NotErrorIsf(t, err, covesoauth.ErrResponseTooLarge,
		"a response declaring maxRematerializeBlobBytes+1 (%d) was refused by the TRANSPORT's own "+
			"ceiling, which defaults to %d — smaller than this site's cap. Two failures wear this "+
			"shape: the shared 32 MiB default still being in force, which fails every blob between "+
			"32 and 100 MiB mid-migration; and a cap set to exactly the copy cap, which clips the byte "+
			"Fetch reads past it to DETECT an overrun and turns the CID-hazard error into a byte "+
			"count; got: %v",
		announced, covesoauth.DefaultMaxResponseBytes, err)
}

// resolvingBlobClient builds the client the way production does and then
// replaces only its NAME RESOLUTION, so the client under test is the real one.
func resolvingBlobClient(t *testing.T, allowPrivateHosts bool, resolvesTo string) RematerializeBlobClient {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(resolvesTo)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", resolvesTo)

	return newGuardedRematerializeBlobClient(allowPrivateHosts, maxRematerializeBlobBytes,
		covesoauth.WithHostResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{ip}, nil
		}))
}

// TestDefaultRematerializeBlobClient_RefusesAWellFormedHostThatResolvesPrivate
// is the assertion a loopback-literal fixture cannot make: the guard's
// CLASSIFICATION pass, on a name that survives every earlier check.
//
// Nothing on this path validates the host's shape — HydrateBlobURL refuses only
// empty strings — so a literal fixture does reach classification here. This case
// still earns its place: it is the one that fails if a future edit satisfies the
// tests above with a literal-only check, which is exactly the mutation the
// jetstream and aggregator sites were caught by.
func TestDefaultRematerializeBlobClient_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	client := resolvingBlobClient(t, false, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	_, err := client.Fetch(context.Background(), rematerializeGuardHost,
		rematerializeGuardDID, rematerializeGuardCID)

	require.Errorf(t, err,
		"%s is a well-formed https host whose DNS answer was 127.0.0.1, and the blob copy fetched "+
			"it anyway. An author's serviceEndpoint is chosen by whoever minted their DID document, "+
			"and they own the zone", rematerializeGuardHost)
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity, or a build where this client was never "+
			"converted looks identical; got: %v", err)
}

// TestDefaultRematerializeBlobClient_ControlTheSameHostIsDialledWithTheHatchOpen
// is the falsifiability control for the case above.
//
// Identical constructor, identical seam, identical host — only the hatch
// differs. With it open the address is no longer refused, so the request
// proceeds to a dial, which fails because nothing is listening on loopback:443.
// That difference is what pins the refusal above to classification rather than to
// this test being unable to make requests at all.
func TestDefaultRematerializeBlobClient_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	client := resolvingBlobClient(t, true, "127.0.0.1") // coves:allow-host-literal: with the hatch open this is dialled and refused by the OS

	_, err := client.Fetch(context.Background(), rematerializeGuardHost,
		rematerializeGuardDID, rematerializeGuardCID)

	require.Error(t, err,
		"nothing listens on loopback:443, so this fetch must fail — if it succeeded, the seam is "+
			"not answering with the address this test gave it")
	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either the gate is not "+
			"reaching the client, or the guarded case above proves nothing: a client that refuses "+
			"every address refuses that case too, for a reason unconnected to classification; got: %v",
		err)
}
