package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/validation"
	"Coves/tests/domaincorpus"
)

// The community consumer's DID-document fetch, against a domain a stranger
// published.
//
// # THE CALL SITE
//
//	didDocURL := fmt.Sprintf("https://%s/.well-known/did.json", domain)
//
// `domain` is extracted from a community record written to its author's own
// repository and carried to us over the firehose, so anyone federated with this
// instance chooses it. The line is BYTE-IDENTICAL IN SHAPE to the one the
// aggregator registration fix closed, and it stayed open for exactly one reason:
// `normalizeDomain` was unexported in internal/api/handlers/aggregator, so the
// fix was unreachable from this package even for someone who noticed.
//
// # TWO MECHANISMS, AND WHY BOTH ARE NEEDED
//
// A guarded dialler closes the ADDRESS half: it refuses to connect to private,
// loopback and link-local addresses, and it dials only what it vetted. It has no
// opinion whatsoever about paths, and that is the other half:
//
//	internal-admin/v1/secrets?x=y#
//
// names no private address. It names whatever `internal-admin` resolves to —
// which on a split-horizon resolver is an ordinary-looking answer — and then
// requests `/v1/secrets?x=y` from it, because the trailing `#` turns the
// `.well-known/did.json` suffix into a fragment that is never sent. So the
// attacker picks the path and the query as well as the host, and a guarded
// client watches it happen.
//
// The validator closes that half. Neither mechanism subsumes the other, which is
// why the tests below assert them SEPARATELY: a validation refusal must carry
// validation.ErrDomainInvalid and NOT the guard's sentinel, and an address
// refusal must carry the guard's sentinel and NOT ErrDomainInvalid. Without that
// separation, deleting either mechanism leaves the suite green — the survivor
// catches most of the same inputs and the assertions cannot tell which one
// fired.

const (
	// consumerTestDID is the DID a community record claims.
	consumerTestDID = "did:web:example.com"

	// consumerTestHandle is the handle the DID document must claim back, and
	// consumerTestDomain is the domain half of it. `example.com` is a hostname,
	// so it passes validation — which is what makes it usable as the CONTROL in
	// every table below.
	consumerTestHandle = "example.com"
	consumerTestDomain = "example.com"
)

// recordingWellKnown answers nothing and remembers whether it was asked to.
//
// A RoundTripper rather than a dialer, because RoundTrip is the earliest moment
// the consumer can reach the network: http.Client.Do calls it before any name is
// resolved, so a call recorded here means the attacker's host was already on its
// way out whether or not a packet followed. Recording the URL is what makes a
// failure readable — the message names the request that would have been sent,
// which for the fragment payload is a path nobody wrote down anywhere.
type recordingWellKnown struct {
	mu        sync.Mutex
	requested []string
}

func (r *recordingWellKnown) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requested = append(r.requested, req.URL.String())
	return nil, errors.New("recordingWellKnown never answers: the consumer should not have reached it")
}

func (r *recordingWellKnown) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requested...)
}

// newRecordingConsumer builds a consumer whose .well-known client cannot talk to
// anything and says so afterwards.
func newRecordingConsumer(t *testing.T) (*CommunityEventConsumer, *recordingWellKnown) {
	t.Helper()

	transport := &recordingWellKnown{}
	consumer := NewCommunityEventConsumer(
		nil, // repo: verifyDIDDocument never touches it
		"did:web:coves.social",
		false, // skipVerification MUST be false or verifyDIDDocument returns before doing anything
		nil,   // identityResolver: unused on this path
		withWellKnownHTTPClient(&http.Client{Transport: transport}),
	)
	return consumer, transport
}

// TestVerifyDIDDocument_ValidatesTheDomainBeforeTouchingTheNetwork is the
// binding contract for the half a guarded dialler cannot close.
//
// Every payload is one the consumer currently sends: each parses as a URL once
// concatenated, so http.NewRequestWithContext succeeds and Do is reached. That
// is deliberate — an input Go's URL parser rejects would never touch the
// transport even with no validation at all, and would prove nothing here.
//
// The corpus is shared with the aggregator's identical ordering test so the two
// call sites cannot drift; see tests/domaincorpus.
func TestVerifyDIDDocument_ValidatesTheDomainBeforeTouchingTheNetwork(t *testing.T) {
	t.Parallel()

	for _, tt := range domaincorpus.InjectionPayloads() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			consumer, transport := newRecordingConsumer(t)

			err := consumer.verifyDIDDocument(
				context.Background(), consumerTestDID, tt.Domain, consumerTestHandle)

			require.Errorf(t, err,
				"the consumer accepted the domain %q and went on to fetch a DID document from it. "+
					"This domain came off a community record published by anyone on the network",
				tt.Domain)

			assert.ErrorIsf(t, err, validation.ErrDomainInvalid,
				"the fetch failed, but not because the domain was refused as malformed: the error does "+
					"not match validation.ErrDomainInvalid. A guarded client that merely failed to reach "+
					"%q looks identical from here, and it is NOT the same control — the guard sees an "+
					"address, and this payload is a path injection against whatever the host resolves "+
					"to; got: %v", tt.Domain, err)

			assert.Emptyf(t, transport.seen(),
				"the consumer asked its HTTP client for %v before refusing the domain %q. Validation "+
					"must run FIRST: by the time the client is called the host has been chosen by a "+
					"stranger, and resolving it is already the SSRF",
				transport.seen(), tt.Domain)
		})
	}

	// THE CONTROL, and without it every row above is unfalsifiable.
	//
	// The whole table asserts that a recorder stayed EMPTY. An empty recorder is
	// also what you get when the recorder was never installed — and
	// withWellKnownHTTPClient is the only thing that installs it. So one case has
	// to drive a domain that PASSES validation and assert the recorder WAS
	// reached. It is the only assertion in this file that can tell "the consumer
	// refused before the client" from "the client under observation was not the
	// client the consumer uses".
	t.Run("control: a valid domain does reach the injected client", func(t *testing.T) {
		t.Parallel()

		consumer, transport := newRecordingConsumer(t)

		err := consumer.verifyDIDDocument(
			context.Background(), consumerTestDID, consumerTestDomain, consumerTestHandle)

		require.Errorf(t, err,
			"the recorder answers nothing, so a domain that reached it must fail the fetch")
		require.NotEmptyf(t, transport.seen(),
			"a domain that passes validation (%q) never reached the recording transport. Every row "+
				"above asserts on that recorder, so if the consumer is not using it those rows prove "+
				"nothing — they would pass just as well against a consumer sending every request "+
				"through a client this test cannot see. Either validation is refusing a legitimate "+
				"hostname, or withWellKnownHTTPClient is not installing the client it is given",
			consumerTestDomain)
		require.Equalf(t,
			[]string{"https://" + consumerTestDomain + "/.well-known/did.json"}, transport.seen(),
			"the consumer asked for %v; the DID-document fetch must go to the domain the record named, "+
				"over HTTPS, at the well-known path and nothing else", transport.seen())
	})
}

// TestVerifyDIDDocument_TheTwoMechanismsAreSeparatelyObservable is requirement
// four, and it is the assertion that keeps this fix from collapsing into one
// mechanism.
//
// Most inputs are refused by both. If every test only asserted "an error came
// back", a build with the validator deleted would look identical to a correct
// one — the guard would catch the residue, the suite would stay green, and the
// path-injection half would be wide open. So each subtest names the mechanism it
// expects AND denies the other.
func TestVerifyDIDDocument_TheTwoMechanismsAreSeparatelyObservable(t *testing.T) {
	t.Parallel()

	t.Run("a path injection is refused by the validator and not by the guard", func(t *testing.T) {
		t.Parallel()

		// A GUARDED client, so both mechanisms are present and the assertion is
		// genuinely about which one fired.
		consumer := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
			withWellKnownHTTPClient(covesoauth.NewSSRFSafeHTTPClient()))

		const payload = "internal-admin/v1/secrets?x=y#"

		err := consumer.verifyDIDDocument(context.Background(), consumerTestDID, payload, consumerTestHandle)

		require.Error(t, err, "a path injection must be refused")
		assert.ErrorIsf(t, err, validation.ErrDomainInvalid,
			"%q must be refused by the VALIDATOR. The guard cannot refuse it: `internal-admin` is not "+
				"an address, and the payload's damage is the path and query it smuggles past the "+
				"fragment, which a safe dialler has no opinion about; got: %v", payload, err)
		assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
			"%q was refused by the address guard rather than the validator. That is the mechanism "+
				"collapse this test exists to catch: it means the refusal depends on what "+
				"`internal-admin` happens to resolve to on this machine, and on a resolver that "+
				"answers with a public address the request goes out; got: %v", payload, err)
	})

}

// # WHY THE GUARD NEEDS ITS OWN FIXTURE AND NOT AN INJECTED CLIENT
//
// An earlier version of this file proved the guard by building an oauth client
// with WithHostResolver and injecting it through withWellKnownHTTPClient. That
// was worthless, and a mutation said so: setting PrivateAddressOptions(true) in
// newWellKnownClient — disabling the guard for every production consumer —
// failed ZERO tests. The injected client is not the client the constructor
// builds, so mutating the constructor could not touch it.
//
// The validator shadows the guard completely for every other input in this file:
// `localhost`, `127.0.0.1:5432`, `2130706433` and `internal-admin` are all
// refused by NormalizeDomain one branch before a transport is reached. So the
// ONLY input that separates the two mechanisms is a domain that PASSES
// validation and still resolves to a private address — a well-formed
// public-looking hostname whose owner points it at 127.0.0.1. That is the
// cheapest SSRF available here, because the attacker owns the zone and the
// validator cannot see a DNS answer.
//
// THE FIX FOR THAT WAS ITSELF ONE CALL FRAME SHORT, AND THIS IS THE SECOND
// CORRECTION. Moving the seam onto newWellKnownClient made the guard provable,
// but the fixture went on to assign that client over the one the CONSTRUCTOR
// built — so applyWellKnownClient, the single place c.allowPrivateHosts is ever
// read, stayed uncovered. Mutation confirmed it: `newWellKnownClient(true)` on
// that line left this file green, `make ci` green and `make ssrf-audit` at zero.
//
// These two tests therefore build through NewCommunityEventConsumer, with the
// same allowPrivateHosts boolean production passes, and hand the resolver seam
// to the constructor via withTransportOptions. Nothing overwrites the client
// afterwards. Do not reintroduce an assignment here, in either spelling — a
// fixture that replaces the consumer's client is a fixture that cannot say whose
// client it exercised, which is the failure this comment has now recorded twice.

// consumerGuardDomain passes validation.NormalizeDomain — two labels, an
// alphabetic TLD, no port, no path, no userinfo — and is exactly what a hostile
// instance publishes when the validator is the only control. `.example` is
// reserved by RFC 2606, so nothing resolves it for real if the seam is ever
// bypassed.
const consumerGuardDomain = "community.example"

// privateAnsweringResolver answers every lookup with one address.
func privateAnsweringResolver(t *testing.T, answer string) func(context.Context, string) ([]net.IP, error) {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and this test would certify the guard against nothing.
	ip := net.ParseIP(answer)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", answer)

	return func(context.Context, string) ([]net.IP, error) {
		return []net.IP{ip}, nil
	}
}

// guardedConsumer builds a consumer the way production does — through
// NewCommunityEventConsumer, from the same allow-private boolean — and replaces
// only its NAME RESOLUTION, by passing the resolver seam as a CONSTRUCTION
// OPTION.
//
// withTransportOptions rather than withWellKnownHTTPClient, and rather than an
// assignment onto the unexported field: both of those REPLACE the consumer's
// client, and a replaced client is one the constructor's wiring never has to
// produce. withTransportOptions configures the client applyWellKnownClient
// builds, so `newWellKnownClient(c.allowPrivateHosts, c.transportOptions...)` is
// the line these tests run through — which is exactly the line that was
// unobservable before. See the comment block above.
func guardedConsumer(t *testing.T, allowPrivateHosts bool, resolvesTo string) *CommunityEventConsumer {
	t.Helper()

	// PrivateHostOptions is what production passes, so the hatch reaches the
	// consumer by the production route; the seam is appended to it rather than
	// replacing it.
	opts := append(PrivateHostOptions(allowPrivateHosts),
		withTransportOptions(covesoauth.WithHostResolver(privateAnsweringResolver(t, resolvesTo))))

	return NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil, opts...)
}

// TestVerifyDIDDocument_RefusesAValidationPassingHostThatResolvesPrivate is the
// assertion this cycle exists for.
//
// The domain is well-formed, so NormalizeDomain accepts it and cannot be what
// refuses the request. Only the transport can. Asserting BOTH halves — that the
// guard's sentinel is present and ErrDomainInvalid is not — is what makes the
// two mechanisms separately visible: delete either one and exactly one of these
// two assertions changes.
func TestVerifyDIDDocument_RefusesAValidationPassingHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	consumer := guardedConsumer(t, false, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial

	err := consumer.verifyDIDDocument(
		context.Background(), consumerTestDID, consumerGuardDomain, consumerTestHandle)

	require.Errorf(t, err,
		"%q passed validation and its DNS answer was 127.0.0.1, and the consumer fetched it anyway. "+
			"This is the SSRF the domain validator cannot reach: the community record's author owns "+
			"the zone, so the name is well-formed and the address is chosen after validation has "+
			"already run", consumerGuardDomain)

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity. Without this, a build where the guarded client "+
			"was never wired looks identical — some other error would still come back, and every "+
			"assertion that only says 'it failed' would still pass; got: %v", err)

	assert.NotErrorIsf(t, err, validation.ErrDomainInvalid,
		"the validator refused a hostname it is supposed to accept, which means this test is no "+
			"longer exercising the transport at all: %q is a two-label name with an alphabetic TLD. "+
			"Fix the fixture rather than the assertion — a guard-path test whose input the validator "+
			"eats is the exact false pass this case was written to close; got: %v",
		consumerGuardDomain, err)
}

// TestVerifyDIDDocument_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control, and without it the test above is unfalsifiable.
//
// Identical consumer, identical seam, identical domain — only the hatch differs.
// With it open the address is no longer refused, so the request proceeds to a
// dial, which fails because nothing is listening on loopback:443. The error is
// therefore NOT the guard's.
//
// That is what pins the refusal above to CLASSIFICATION. Without this control, a
// client that could not make any request at all — a broken seam, a transport
// wired to nothing — would satisfy the guarded case just as well.
//
// It is also why there is no "the listener was never reached" assertion in THIS
// test: a validation-passing domain has no port, so verifyDIDDocument always
// builds https://<domain>/… on 443, and a test cannot bind 443. This control
// does the equivalent work for anything driven through verifyDIDDocument.
//
// The client itself is a different matter, and does get a real one —
// TestNewCommunityEventConsumer_BuildsAGuardedClientWithItsSettingsPreserved
// drives consumer.httpClient at a port a test may bind and asserts a request
// counter stayed at zero.
func TestVerifyDIDDocument_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	consumer := guardedConsumer(t, true, "127.0.0.1") // coves:allow-host-literal: the address the seam answers with; with the hatch open it is dialled and refused by the OS

	err := consumer.verifyDIDDocument(
		context.Background(), consumerTestDID, consumerGuardDomain, consumerTestHandle)

	require.Errorf(t, err,
		"nothing listens on loopback:443, so this fetch must fail — if it succeeded, the seam is not "+
			"answering with the address this test gave it")

	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either PrivateHostOptions "+
			"is not reaching the client, or the guarded case above proves nothing: a client that "+
			"refuses every address refuses the guarded case too, for a reason that has nothing to do "+
			"with classification; got: %v", err)
}

// TestVerifyDIDDocument_AcceptsAValidDomainOverTheGuardedPath is the other
// direction: refusing everything is not a fix.
//
// The dial is pinned to a local listener while the URL still names
// `example.com`, the way internal/api/handlers/aggregator's stubClient does it,
// because the validator refuses IP literals INDEPENDENTLY OF THE HATCH — so a
// fixture cannot simply point this call site at `127.0.0.1:PORT` the way the
// unfurl and imageproxy fixtures can. That is worth knowing before writing any
// future fixture for this path.
//
// This case uses a pinned plain client on purpose and proves nothing about the
// guard; the guarded path is the subtest above. What it proves is that a
// legitimate federated instance still verifies.
func TestVerifyDIDDocument_AcceptsAValidDomainOverTheGuardedPath(t *testing.T) {
	t.Parallel()

	var requests int64
	var mu sync.Mutex
	stub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()

		if r.URL.Path != "/.well-known/did.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          consumerTestDID,
			"alsoKnownAs": []string{"at://" + consumerTestHandle},
		})
	}))
	t.Cleanup(stub.Close)

	// stub.Client() trusts exactly this server's certificate, which is narrower
	// than disabling verification. Only the ADDRESS is decided here; the name in
	// the URL is still checked against the certificate.
	client := stub.Client()
	transport, ok := client.Transport.(*http.Transport)
	require.Truef(t, ok,
		"httptest client transport is %T, want *http.Transport — the dial cannot be pinned, so this "+
			"test would be talking to whatever example.com resolves to", client.Transport)
	addr := stub.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}

	consumer := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
		withWellKnownHTTPClient(client))

	err := consumer.verifyDIDDocument(
		context.Background(), consumerTestDID, consumerTestDomain, consumerTestHandle)

	require.NoErrorf(t, err,
		"a legitimate federated instance failed verification. The validation being added must refuse "+
			"hosts this AppView should not fetch, not the ordinary hostnames every real instance "+
			"runs under; got: %v", err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equalf(t, int64(1), requests,
		"the DID document was fetched %d times rather than once", requests)
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// single most important assertion for this call site.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// T0+T1+T2 — runs the PERMISSIVE branch here and at every other site holding
// such a boolean. A green merge gate therefore proves nothing whatsoever about
// whether this consumer is guarded in production. This function is the one place
// in the repository where the production branch is ever evaluated, which is why
// the gate must be a pure function and not an `if cfg.IsDevEnv` in
// cmd/server/wiring.go.
//
// The claim is not "the options returned are safe". It is that there are NONE:
// length zero, nothing applied, the constructor's own defaults left untouched.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). The production branch — the one "+
			"IS_DEV_ENV=true keeps `make ci` from ever evaluating — must contribute nothing at all, "+
			"so that what production gets is exactly the constructor's own defaults", len(opts))
}

// TestPrivateHostOptions_BindTheGateToTheConstructor pins the other direction
// through the state the constructor actually ends up in.
//
// A length check on the false branch is worthless on its own: a helper returning
// the wrong single-element slice satisfies it while leaving every developer
// unable to reach anything local, and a helper returning nothing in BOTH
// directions satisfies it while silently guaranteeing the guard can never be
// opened.
func TestPrivateHostOptions_BindTheGateToTheConstructor(t *testing.T) {
	t.Parallel()

	guarded := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
		PrivateHostOptions(false)...)
	assert.False(t, guarded.allowPrivateHosts,
		"a consumer built from PrivateHostOptions(false) has the SSRF hatch open. This is the branch "+
			"production runs and CI never does")

	hatched := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
		PrivateHostOptions(true)...)
	assert.True(t, hatched.allowPrivateHosts,
		"a consumer built from PrivateHostOptions(true) is still guarded, so the dev hatch does "+
			"nothing and a developer's local stack cannot be verified against")
}

// TestNewCommunityEventConsumer_BuildsAGuardedClientWithItsSettingsPreserved is
// the conversion's own fence.
//
// # WHAT CARRIES THE CONTRACT, AND WHAT USED TO
//
// This fence used to be the timeout plus `_, bare := …Transport.(*http.Transport)`.
// THAT TYPE ASSERTION CANNOT FAIL ON POLARITY. The hatch is a bool field INSIDE
// oauth's transport, not a different type, so a consumer built with the guard
// OPEN is just as much "not a bare *http.Transport" as one built with it shut —
// the check sees that the conversion happened and nothing about which way the
// switch is set. A build whose constructor hardcoded the hatch open passed this
// fence unchanged.
//
// So the contract is carried by REACHABILITY below, and the type and timeout
// checks are kept as the cheap secondary they always were.
//
// # THE TIMEOUT
//
// NewSSRFSafeHTTPClient ships a 15s ceiling of its own and this consumer runs
// on wellKnownTimeout (5s since the consumer trust audit; 10s before it).
// Adopting the shared client without restoring the caller's value LOOSENS
// every .well-known fetch to the shared ceiling — a change nobody asked for,
// arriving as part of an SSRF fix, on a firehose path where a slow remote
// host holds up event processing. blobs.NewBlobService,
// imageproxy.NewPDSFetcher and unfurl.NewService all restore their own for the
// same reason.
func TestNewCommunityEventConsumer_BuildsAGuardedClientWithItsSettingsPreserved(t *testing.T) {
	t.Parallel()

	t.Run("the default client refuses a private address without reaching the listener", func(t *testing.T) {
		t.Parallel()

		listener := newCountingWellKnown(t)
		// No hatch option: PrivateHostOptions(false) is nil, so this is exactly
		// what cmd/server constructs in production, plus a seam that sets no
		// policy.
		consumer := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
			append(PrivateHostOptions(false),
				withTransportOptions(covesoauth.WithHostResolver(
					privateAnsweringResolver(t, "127.0.0.1"))))...) // coves:allow-host-literal: the address the seam answers with; the guard must refuse it before any dial

		reached, err := wellKnownClientRequest(t, consumer, listener)

		// THE REACHABILITY CLAIM COMES FIRST, deliberately. It is the security
		// property, and asserting it before the error means a regression reports
		// "the listener was reached" rather than the far less useful "an error
		// was expected but got nil".
		assert.Zerof(t, reached,
			"the listener was reached %d time(s), so the consumer's own .well-known client dialled a "+
				"host whose DNS answer was 127.0.0.1 and delivered the request. That domain comes off "+
				"a community record published by anyone federated with this instance. This is the "+
				"assertion the old *http.Transport type check could not make, and the one that dies "+
				"if applyWellKnownClient's hatch argument is ever replaced by a constant", reached)

		require.Error(t, err,
			"the request SUCCEEDED against a private address, so the default client is not guarded "+
				"at all")
		assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
			"the refusal must carry the guard's identity, or a client that simply cannot make requests "+
				"satisfies this case too; got: %v", err)
	})

	t.Run("control: the same listener IS reached with the hatch open", func(t *testing.T) {
		t.Parallel()

		listener := newCountingWellKnown(t)
		consumer := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
			append(PrivateHostOptions(true), // coves:allow-ssrf-hatch: the control half; without it the guarded case above is unfalsifiable
				withTransportOptions(covesoauth.WithHostResolver(
					privateAnsweringResolver(t, "127.0.0.1"))))...) // coves:allow-host-literal: the address the seam answers with; with the hatch open it must be dialled

		reached, err := wellKnownClientRequest(t, consumer, listener)

		require.NoErrorf(t, err,
			"the hatch was open and the request still failed, so the guarded case above proves nothing: "+
				"a client that reaches nothing refuses a private address for reasons unconnected to "+
				"classification; got: %v", err)
		assert.Equalf(t, int64(1), reached,
			"the listener was reached %d time(s) with the hatch open, want exactly 1. The zero asserted "+
				"above only means something if this listener is reachable when classification permits it",
			reached)
	})

	t.Run("secondary: the timeout and the transport are the guarded builder's", func(t *testing.T) {
		t.Parallel()

		consumer := NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
			PrivateHostOptions(false)...)

		require.NotNil(t, consumer.httpClient, "the consumer must hold an HTTP client")

		assert.Equalf(t, wellKnownTimeout, consumer.httpClient.Timeout,
			"the .well-known client runs on a %v timeout instead of wellKnownTimeout (%v). The shared "+
				"SSRF client ships a 15s ceiling, so a call site that adopts it without re-applying its "+
				"own value silently re-times every federated verification",
			consumer.httpClient.Timeout, wellKnownTimeout)

		// A TYPE CHECK AND NOTHING MORE. It catches "the conversion never
		// happened"; it is blind to which way the hatch is set, which is why the
		// subtests above exist.
		_, bare := consumer.httpClient.Transport.(*http.Transport)
		assert.Falsef(t, bare,
			"the consumer's .well-known client still uses a bare *http.Transport, which resolves and "+
				"dials whatever a federated community record names. It must be the SSRF-safe transport "+
				"from internal/atproto/oauth, which vets the resolved addresses and then dials only "+
				"those — closing the check-then-dial window a naive guard leaves open")
	})
}

// countingWellKnown is a server that answers a DID document and counts how many
// requests actually arrived.
//
// PLAIN HTTP, not TLS, and deliberately so. The claim is about ADDRESS
// CLASSIFICATION — did the transport dial, or refuse before dialling — and a TLS
// listener puts a certificate check between the dial and the handler. httptest's
// cert carries SANs [example.com, *.example.com], so a guard-test hostname would
// fail the handshake and the handler would record zero requests EVEN WITH THE
// HATCH OPEN, quietly turning the control above into another vacuous assertion.
// Plain HTTP removes the only other thing that can hold the counter at zero.
type countingWellKnown struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingWellKnown(t *testing.T) *countingWellKnown {
	t.Helper()

	listener := &countingWellKnown{}
	listener.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		listener.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          consumerTestDID,
			"alsoKnownAs": []string{"at://" + consumerTestHandle},
		})
	}))
	t.Cleanup(listener.server.Close)
	return listener
}

// wellKnownClientRequest drives a consumer's OWN client at a host the seam
// resolves to the listener, and reports the error and whether it was reached.
//
// It calls consumer.httpClient directly rather than verifyDIDDocument, for the
// constraint the guardedConsumer comment block records: a validation-passing
// domain has no port, so verifyDIDDocument always builds https://<domain>/… on
// 443, and a test cannot bind 443. Naming the port here is what makes a
// reachable listener possible at all — and it gives up nothing this test claims,
// because the subject is the CLIENT the constructor installed, not the URL
// verifyDIDDocument builds. NormalizeDomain's separate refusal of ports is
// pinned by TestVerifyDIDDocument_RefusesEveryCorpusDomain.
func wellKnownClientRequest(t *testing.T, consumer *CommunityEventConsumer, listener *countingWellKnown) (reached int64, err error) {
	t.Helper()

	_, port, err := net.SplitHostPort(listener.server.Listener.Addr().String())
	require.NoError(t, err, "the listener's address must split into host and port")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+consumerGuardDomain+":"+port+"/.well-known/did.json", nil)
	require.NoError(t, err)

	resp, doErr := consumer.httpClient.Do(req)
	if doErr == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return listener.requests.Load(), doErr
}

// TestVerifyDIDDocument_RefusesEveryCorpusDomain is the wide net.
//
// The ordering test above drives only the payloads that parse into a URL,
// because those are the ones that prove ordering. This one drives the WHOLE
// shared corpus and asserts the same sentinel, so a domain shape added to the
// corpus for the aggregator's sake is automatically a domain shape this call
// site is tested against. That is the entire reason the corpus is a package.
func TestVerifyDIDDocument_RefusesEveryCorpusDomain(t *testing.T) {
	t.Parallel()

	for _, tt := range domaincorpus.Invalid() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			consumer, transport := newRecordingConsumer(t)

			err := consumer.verifyDIDDocument(
				context.Background(), consumerTestDID, tt.Domain, consumerTestHandle)

			require.Errorf(t, err, "the consumer accepted the domain %q", tt.Domain)
			assert.ErrorIsf(t, err, validation.ErrDomainInvalid,
				"domain %q must be refused with validation.ErrDomainInvalid. Some rows in this corpus "+
					"are also refused by net/http or by the guard, and that is exactly why the sentinel "+
					"is asserted rather than the mere presence of an error: an incidental refusal is "+
					"one a dependency upgrade can take away; got: %v", tt.Domain, err)
			assert.Emptyf(t, transport.seen(),
				"the consumer reached its HTTP client for %v while handling domain %q",
				transport.seen(), tt.Domain)
		})
	}
}

// TestVerifyDIDDocument_TheInjectionShapeIsWhatItLooksLike documents, in an
// executable place, the string this whole file exists because of.
//
// It is not testing fmt.Sprintf. It pins the SHAPE that makes the injection
// work, so a reader of a future failure can see in one line why
// `internal-admin/v1/secrets?x=y#` reaches `/v1/secrets?x=y` and why no
// address-level control can prevent it. If this test ever fails, the payload has
// stopped being dangerous and the corpus row can be re-evaluated — which is a
// conclusion nobody should reach by reasoning about it in their head.
func TestVerifyDIDDocument_TheInjectionShapeIsWhatItLooksLike(t *testing.T) {
	t.Parallel()

	const payload = "internal-admin/v1/secrets?x=y#"

	parsed, err := url.Parse(fmt.Sprintf("https://%s/.well-known/did.json", payload))
	require.NoError(t, err, "the payload must still parse as a URL, or it would never reach a client")

	assert.Equal(t, "internal-admin", parsed.Host,
		"the host the request would go to is the attacker's first label, not a domain they own")
	assert.Equal(t, "/v1/secrets", parsed.Path,
		"the path is the attacker's, not /.well-known/did.json — the concatenation was supposed to "+
			"decide this and the fragment took it away")
	assert.Equal(t, "x=y", parsed.RawQuery, "the query is the attacker's too")
	assert.Equal(t, "/.well-known/did.json", parsed.Fragment,
		"the suffix this call site appends became a fragment, and fragments are never sent — which "+
			"is why this is a full request-forgery primitive and not an SSRF to one fixed path")
}
