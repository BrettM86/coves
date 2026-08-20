package blobs

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The blob service's UPLOAD half, and the asymmetry that kept it open.
//
// # WHY THIS ONE SURVIVED
//
// NewBlobService builds the DOWNLOAD half's client through the SSRF-safe
// transport, and has since before this effort started. UploadBlob built a fresh
// `&http.Client{Timeout: 30 * time.Second}` a hundred and forty lines further
// down, in the same file, under the same package review. A reader who checks the
// top of the file concludes the service is guarded; a reader who checks the
// bottom concludes it is not; and both stop reading, because the file has
// already answered the question once.
//
// # WHY IT IS THE WORSE HALF
//
// The download half fetches a URL and reads bytes. This half sends
//
//	Authorization: Bearer <accessToken>
//
// to `owner.GetPDSURL()` — a PDS URL that for a federated community arrives on a
// record from another instance. So the primitive is not "make the AppView reach
// an internal address", it is "make the AppView POST a live PDS credential to an
// address I chose", with a blob body attached. A blocked fetch costs an attacker
// a retry. A leaked bearer token is not recoverable, and nothing in the response
// has to come back for the attack to have succeeded — which is why every case
// below asserts the listener was NEVER REACHED rather than that an error was
// returned.

// stubOwner is a BlobOwner pointing at wherever the test says.
type stubOwner struct {
	pdsURL string
	token  string
}

func (o stubOwner) GetPDSURL() string         { return o.pdsURL }
func (o stubOwner) GetPDSAccessToken() string { return o.token }

// countingPDS records whether anything reached it, and what credential it
// carried. The credential is recorded because "the listener was reached" and
// "the token left the process" are the same event here, and naming it in the
// failure message is what makes the severity legible.
type countingPDS struct {
	server   *httptest.Server
	requests atomic.Int64
	tokens   chan string
}

func newCountingPDS(t *testing.T) *countingPDS {
	t.Helper()

	pds := &countingPDS{tokens: make(chan string, 8)}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pds.requests.Add(1)
		select {
		case pds.tokens <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"blob":{"$type":"blob","ref":{"$link":"bafyupload"},` +
			`"mimeType":"image/jpeg","size":3}}`))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// pngBytes is a payload UploadBlob's MIME and size validation both accept, so a
// refusal below is attributable to the address and to nothing else.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// TestUploadBlob_RefusesAPrivatePDSWithoutReachingIt is the binding contract for
// this site.
func TestUploadBlob_RefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	service := NewBlobService(pds.server.URL, PrivateHostOptions(false)...)

	_, err := service.UploadBlob(context.Background(),
		stubOwner{pdsURL: pds.server.URL, token: "secret-pds-access-token"},
		pngBytes, "image/png")

	// THE REACHABILITY CLAIMS COME FIRST, deliberately. They are the security
	// facts; the error is only how the caller learns about them. Asserting the
	// error first with require would abort the test on failure and hide whether
	// the token actually left the process — which is the one thing a reader of
	// this failure most needs to know.
	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. For this half of the service the request LEAVING is the "+
			"whole breach: the Authorization header is already on the wire, and no response is "+
			"needed for the token to have been handed over", pds.requests.Load())

	select {
	case token := <-pds.tokens:
		assert.Failf(t, "a PDS access token was handed to the listener",
			"the listener received %q. This is credential exfiltration and not only SSRF: the "+
				"address came from a federated owner record, and the guard must refuse it BEFORE "+
				"the request carrying the token is sent", token)
	default:
	}

	require.Error(t, err,
		"UploadBlob POSTed a blob and a live PDS bearer token to a loopback address, and reported "+
			"success. The PDS URL comes from the owner record, which for a federated community is "+
			"written by another instance, so this is a stranger choosing where the AppView sends a "+
			"credential")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the upload failed, but not because the guard refused the address. Without the guard's own "+
			"identity in the chain, a build where this half was never converted looks identical — "+
			"an unreachable address fails too; got: %v", err)
}

// TestUploadBlob_ReachesThePDSWhenTheHatchIsOpen is the other direction, and the
// falsifiability control for the case above: a client that could make no request
// at all would satisfy that test just as well.
func TestUploadBlob_ReachesThePDSWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	service := NewBlobService(pds.server.URL, PrivateHostOptions(true)...)

	ref, err := service.UploadBlob(context.Background(),
		stubOwner{pdsURL: pds.server.URL, token: "secret-pds-access-token"},
		pngBytes, "image/png")

	require.NoErrorf(t, err,
		"the hatch is what every fixture in this tree and every dev stack depends on: a service "+
			"built with PrivateHostOptions(true) must reach a loopback PDS; got: %v", err)
	require.NotNil(t, ref, "a successful upload must return a blob ref")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// gate this site does not currently have.
//
// cmd/server/wiring.go writes `if a.cfg.IsDevEnv { … WithPrivateHostsAllowed() }`
// inline for this service. `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes
// the permissive branch, and an inline `if` in wiring is reachable only by
// standing up that wiring with a production config — which nothing in this tree
// does. So the branch production actually runs is, today, evaluated nowhere.
//
// The claim is not "the options returned are safe". It is that there are NONE.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). The production branch — the one "+
			"IS_DEV_ENV=true keeps `make ci` from ever evaluating — must contribute nothing at all, "+
			"so that what production gets is exactly the constructor's own defaults", len(opts))
}

// TestPrivateHostOptions_BindTheGateToTheConstructor pins the other direction
// through the state the constructor ends up in, so a helper that returns nothing
// in BOTH directions — which satisfies the length check above perfectly — is
// caught here instead.
func TestPrivateHostOptions_BindTheGateToTheConstructor(t *testing.T) {
	t.Parallel()

	guarded, ok := NewBlobService("https://pds.example", PrivateHostOptions(false)...).(*blobService)
	require.True(t, ok, "NewBlobService must return the concrete *blobService these tests drive")
	assert.False(t, guarded.allowPrivateHosts,
		"a service built from PrivateHostOptions(false) has the SSRF hatch open. This is the branch "+
			"production runs and CI never does")

	hatched, ok := NewBlobService("https://pds.example", PrivateHostOptions(true)...).(*blobService)
	require.True(t, ok, "NewBlobService must return the concrete *blobService these tests drive")
	assert.True(t, hatched.allowPrivateHosts,
		"a service built from PrivateHostOptions(true) is still guarded, so the dev hatch does "+
			"nothing and a developer's local PDS cannot be uploaded to")
}

// TestUploadBlob_PreservesTheConfiguredTimeout guards the setting the shared
// client would otherwise swallow.
//
// NewSSRFSafeHTTPClient ships a 15s ceiling of its own and this upload has always
// allowed 30s — a blob POST carries up to 6 MB of body, so it is the request in
// this service most likely to need the headroom. NewBlobService already restores
// the download half's for the same reason.
func TestUploadBlob_PreservesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	service, ok := NewBlobService("https://pds.example").(*blobService)
	require.True(t, ok, "NewBlobService must return the concrete *blobService these tests drive")

	require.NotNil(t, service.uploadClient, "the service must hold an upload client")
	assert.Equalf(t, 30*time.Second, service.uploadClient.Timeout,
		"the upload client runs on a %v timeout instead of the 30s this POST has always allowed. "+
			"The shared SSRF client ships a 15s ceiling, so adopting it without re-applying this "+
			"value silently re-times every blob upload — on the path that carries the largest body "+
			"in the service", service.uploadClient.Timeout)
}

// # WHY THE GUARD NEEDS ITS OWN FIXTURE HERE TOO
//
// Nothing else in this file separates the guard from the rest of UploadBlob's
// validation, because every address a loopback fixture can offer is refused on
// SHAPE (an IP literal) one branch before classification runs. A mutation that
// disabled classification while leaving the literal check would fail nothing.
//
// So these two drive newBlobUploadClient — the function the constructor calls,
// with the same allowPrivateHosts boolean — and pass the resolver seam through
// it, exactly as jetstream's community-consumer tests do after the same gap was
// found there by mutation.

// uploadGuardHost passes every shape check UploadBlob applies and is a name
// rather than an address, so classification is the only thing left that can
// refuse it. `.example` is reserved by RFC 2606, so nothing resolves it for real
// if the seam is ever bypassed.
const uploadGuardHost = "https://community-pds.example"

// resolvingUploadService builds the service the way production does and then
// replaces only its NAME RESOLUTION, so the client under test is the real one.
func resolvingUploadService(t *testing.T, allowPrivateHosts bool, resolvesTo string) *blobService {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(resolvesTo)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", resolvesTo)

	service, ok := NewBlobService(uploadGuardHost, PrivateHostOptions(allowPrivateHosts)...).(*blobService)
	require.True(t, ok, "NewBlobService must return the concrete *blobService these tests drive")

	service.uploadClient = newBlobUploadClient(allowPrivateHosts,
		covesoauth.WithHostResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{ip}, nil
		}))
	return service
}

// TestUploadBlob_RefusesAWellFormedHostThatResolvesPrivate is the assertion a
// literal-shaped fixture cannot make.
func TestUploadBlob_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	service := resolvingUploadService(t, false, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	_, err := service.UploadBlob(context.Background(),
		stubOwner{pdsURL: uploadGuardHost, token: "secret-pds-access-token"},
		pngBytes, "image/png")

	require.Errorf(t, err,
		"%s is a well-formed https host whose DNS answer was 127.0.0.1, and the upload went ahead. "+
			"A federated community's PDS URL is chosen by whoever wrote the record, and they own "+
			"the zone — so the name looks ordinary and the address is decided after every shape "+
			"check has already passed", uploadGuardHost)

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity. Without it, a build where this half was never "+
			"wired to the guarded client looks identical; got: %v", err)
}

// TestUploadBlob_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control for the case above.
//
// Identical service, identical seam, identical host — only the hatch differs.
// With it open the address is no longer refused, so the request proceeds to a
// dial, which fails because nothing is listening on loopback:443. The error is
// therefore NOT the guard's, which is what pins the refusal above to
// CLASSIFICATION rather than to this test being unable to make requests at all.
func TestUploadBlob_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	service := resolvingUploadService(t, true, "127.0.0.1") // coves:allow-host-literal: with the hatch open this is dialled and refused by the OS

	_, err := service.UploadBlob(context.Background(),
		stubOwner{pdsURL: uploadGuardHost, token: "secret-pds-access-token"},
		pngBytes, "image/png")

	require.Error(t, err,
		"nothing listens on loopback:443, so this upload must fail — if it succeeded, the seam is "+
			"not answering with the address this test gave it")

	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either PrivateHostOptions "+
			"is not reaching the client, or the guarded case above proves nothing: a client that "+
			"refuses every address refuses the guarded case too, for a reason that has nothing to "+
			"do with classification; got: %v", err)
}
