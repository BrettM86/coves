package aggregator

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/users"

	"Coves/internal/atproto/identity"
)

// The registration handler's outbound client, and the one assertion that can
// tell it apart from the validator sitting in front of it.
//
// # WHY THIS IS THE HARDEST SITE TO TEST HONESTLY
//
// Two mechanisms refuse this endpoint's hostile inputs, and they overlap almost
// completely. normalizeDomain refuses `127.0.0.1:5432`, `localhost`,
// `2130706433`, `internal-admin` and every other spelling of an address,
// because none of them is a two-label name with an alphabetic TLD. The SSRF
// guard refuses the same destinations one layer down. So a test that asserts
// only "an error came back" passes whether the guarded client is wired or not —
// and register_domain_test.go's whole rejection table is made of inputs the
// validator eats first.
//
// The only input that separates them is a domain that PASSES validation and
// still resolves to a private address: a well-formed public-looking hostname
// whose owner points it at 127.0.0.1. That is not a hypothetical — it is the
// cheapest SSRF an attacker has here, because they control the zone and the
// validator cannot see the DNS answer. It is also, notably, the one input the
// domain-shape validation alone cannot reject.
//
// # THE RESOLVER SEAM
//
// A hostname cannot be made to resolve to a chosen address hermetically: the
// hermetic tiers block egress, and nothing in the tree can write /etc/hosts.
// oauth.WithHostResolver is therefore the seam these tests drive — the same
// field oauth's own tests already set on the transport, exported so a caller
// package can prove ITS wiring rather than re-proving oauth's.
//
// It cannot open the guard. Whatever the seam answers is classified exactly as
// a real DNS answer would be, and the dial still goes only to vetted addresses;
// the seam decides what gets classified, never whether classification happens.
//
// # WHERE THE "NEVER REACHED" ASSERTION LIVES, AND WHERE IT CANNOT
//
// An earlier version of this comment said flatly that this site could not have
// one. That was half right, and the half it got wrong mattered.
//
// It is true of anything driven through verifyDomainOwnership: a
// validation-passing domain has no port, so the URL is always https://<domain>/…
// on 443, and a test cannot bind 443. For those tests the CONTROL does the
// equivalent work — same client, same seam, same domain, hatch OPEN, and the
// error becomes a dial failure instead of a classification refusal. That
// difference is what proves the guarded refusal came from classifying the
// address rather than from this test being unable to make requests at all.
//
// It is NOT true of the client itself. TestNewRegisterHandler_DefaultClientIsGuarded
// drives handler.httpClient directly at a host the seam resolves, on a port a
// test IS allowed to bind, and asserts a real request counter stayed at zero —
// with a control proving that same listener is reached when the hatch is open.
// That assertion exists because the fence's old form was a reflect.TypeOf
// comparison, which cannot see a bool field inside oauth's transport and so
// could not fail on polarity at all.

// privateHostResolver answers every lookup with one address, and records the
// names it was asked about.
func privateHostResolver(t *testing.T, answer string) func(context.Context, string) ([]net.IP, error) {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as public and the test would pass or fail for reasons unconnected
	// to its subject.
	ip := net.ParseIP(answer)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", answer)

	return func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{ip}, nil
	}
}

// guardedRegisterHandler builds the handler the way production does — through
// NewRegisterHandler, from the same allow-private boolean — and replaces only
// its NAME RESOLUTION, by passing the resolver seam as a CONSTRUCTION OPTION.
//
// # THE SEAM GOES THROUGH THE CONSTRUCTOR, AND THAT IS THE POINT
//
// An earlier version of this fixture built the handler exactly as above and then
// assigned `handler.httpClient = registerHTTPClient(allowPrivateHosts, seam)`
// over the result. That second line threw the first one away, and with it the
// only thing these tests are here to prove. h.allowPrivateHosts is read in ONE
// place in this tree — NewRegisterHandler's `registerHTTPClient(h.allowPrivateHosts,
// h.transportOptions...)` — and a build with that argument replaced by a
// constant `true` left every test in this file green, `make ci` green and
// `make ssrf-audit` at zero. What was under test was registerHTTPClient, which
// is one call frame short of the wiring.
//
// So the client under test is now the handler's own, built by the constructor
// production calls, and nothing here overwrites it. Neither setHTTPClient nor a
// direct field assignment appears: both replace the client wholesale, which is
// precisely what makes a test unable to say WHOSE client it exercised.
func guardedRegisterHandler(t *testing.T, allowPrivateHosts bool, resolvesTo string) *RegisterHandler {
	t.Helper()

	userService := &fakeUserService{registered: map[string]*users.User{}}
	resolver := &fakeIdentityResolver{identities: map[string]*identity.Identity{
		registrantDID: {DID: registrantDID, Handle: registrantHandle, PDSURL: registrantPDS},
	}}

	// PrivateHostOptions is what production passes, so the hatch reaches the
	// handler by the production route; the seam is appended to it rather than
	// replacing it.
	opts := append(PrivateHostOptions(allowPrivateHosts),
		withTransportOptions(covesoauth.WithHostResolver(privateHostResolver(t, resolvesTo))))

	return NewRegisterHandler(userService, resolver, opts...)
}

// guardTestDomain passes normalizeDomain — two labels, an alphabetic TLD, no
// port, no path, no userinfo — and is exactly what an attacker registers when
// the validator is the only control. `.example` is reserved by RFC 2606, so
// nothing resolves it for real if the seam is ever bypassed.
const guardTestDomain = "aggregator.example"

// TestVerifyDomainOwnership_RefusesAValidationPassingHostThatResolvesPrivate is
// the assertion this cycle exists for.
//
// The domain is well-formed, so normalizeDomain accepts it and cannot be what
// refuses the request. Only the transport can. Asserting BOTH halves — that the
// guard's sentinel is present and ErrDomainInvalid is not — is what makes the
// two mechanisms separately visible: delete either one and exactly one of these
// two assertions changes.
func TestVerifyDomainOwnership_RefusesAValidationPassingHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	handler := guardedRegisterHandler(t, false, "127.0.0.1")

	err := handler.verifyDomainOwnership(context.Background(), registrantDID, guardTestDomain)

	require.Errorf(t, err,
		"%q passed validation and its DNS answer was 127.0.0.1, and the handler fetched it anyway. "+
			"This is the SSRF the domain validator cannot reach: the attacker owns the zone, so the "+
			"name is well-formed and the address is chosen after validation has already run",
		guardTestDomain)

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity. Without this, a build where the guarded client "+
			"was never wired looks identical — some other error would still come back, and every "+
			"assertion that only says 'it failed' would still pass; got: %v", err)

	assert.NotErrorIsf(t, err, ErrDomainInvalid,
		"the validator refused a hostname it is supposed to accept, which means this test is no longer "+
			"exercising the transport at all: %q is a two-label name with an alphabetic TLD. Fix the "+
			"fixture rather than the assertion — a guard-path test whose input the validator eats is "+
			"the exact false pass this case was written to close; got: %v", guardTestDomain, err)
}

// TestVerifyDomainOwnership_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control, and without it the test above is unfalsifiable.
//
// Identical handler, identical seam, identical domain — only the hatch differs.
// With it open the address is no longer refused, so the request proceeds to a
// dial, which fails because nothing is listening on loopback:443. The error is
// therefore NOT the guard's.
//
// That is what pins the refusal above to CLASSIFICATION. Without this control,
// a client that could not make any request at all — a broken seam, a transport
// wired to nothing — would satisfy the guarded case just as well.
func TestVerifyDomainOwnership_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	handler := guardedRegisterHandler(t, true, "127.0.0.1")

	err := handler.verifyDomainOwnership(context.Background(), registrantDID, guardTestDomain)

	require.Errorf(t, err,
		"nothing listens on loopback:443, so this fetch must fail — if it succeeded, the seam is not "+
			"answering with the address this test gave it")

	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either PrivateHostOptions "+
			"is not reaching the client, or the guarded case above proves nothing: a client that refuses "+
			"every address refuses the guarded case too, for a reason that has nothing to do with "+
			"classification; got: %v", err)
}

// TestHandleRegister_SeparatesTheTwoRefusalsAtTheHTTPLayer pins the same split
// where an operator and a caller actually see it.
//
// The handler already answers the two mechanisms differently — 400 InvalidDID
// for a malformed domain, 401 DomainVerificationFailed for a fetch that was
// attempted and failed — and that distinction is worth keeping precisely
// because it is the only externally visible evidence of which control fired.
// Unlike the image proxy's status codes, this pair is not an oracle: both
// outcomes are things the caller told the server, and neither reports anything
// about the AppView's network.
func TestHandleRegister_SeparatesTheTwoRefusalsAtTheHTTPLayer(t *testing.T) {
	t.Parallel()

	t.Run("a validation-passing host that resolves private is a verification failure", func(t *testing.T) {
		t.Parallel()

		handler := guardedRegisterHandler(t, false, "127.0.0.1")

		rec := postRegister(t, handler, registrantDID, guardTestDomain)

		require.Equalf(t, http.StatusUnauthorized, rec.Code,
			"a well-formed domain whose address the guard refused must answer as a failed verification, "+
				"not as a malformed request: the caller's input WAS well-formed, and reporting otherwise "+
				"tells them their domain is syntactically wrong when it is not. Body: %s", rec.Body.String())

		var body XRPCError
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body is not valid JSON")
		assert.Equal(t, "DomainVerificationFailed", body.Error,
			"the error code is the only signal distinguishing the guard's refusal from the validator's")
	})

	t.Run("a malformed domain is still refused by the validator", func(t *testing.T) {
		t.Parallel()

		// The guard would refuse this destination too, one layer down — which is
		// the whole difficulty. Pinning the validator's own sentinel separately
		// is what keeps a future change that deletes normalizeDomain from
		// looking like a passing suite: the guard sees an ADDRESS, and
		// `internal-admin/v1/secrets?x=y#` is a path injection against whatever
		// internal-admin resolves to — not something a classifier can catch.
		handler := guardedRegisterHandler(t, false, "127.0.0.1")

		rec := postRegister(t, handler, registrantDID, "internal-admin/v1/secrets?x=y#")

		require.Equalf(t, http.StatusBadRequest, rec.Code,
			"a domain that is not a hostname must be refused as malformed, before any client is "+
				"touched. Body: %s", rec.Body.String())

		var body XRPCError
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body is not valid JSON")
		assert.Equal(t, "InvalidDID", body.Error,
			"the validator's refusal must stay distinguishable from the guard's")
	})
}

// countingListener is a server that answers 200 and counts how many requests
// actually arrived.
//
// It is PLAIN HTTP, not TLS, and that is a deliberate narrowing. The claim under
// test is about ADDRESS CLASSIFICATION — did the transport dial, or refuse
// before dialling — and a TLS listener puts a certificate check between the dial
// and the handler. httptest's cert carries SANs [example.com, *.example.com], so
// a guard-test hostname would fail the handshake and the handler would record
// zero requests EVEN WITH THE HATCH OPEN, which quietly turns the control below
// into another vacuous assertion. Plain HTTP removes the only other thing that
// can keep the counter at zero, so a zero means what this test says it means.
type countingListener struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingListener(t *testing.T) *countingListener {
	t.Helper()

	listener := &countingListener{}
	listener.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		listener.requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(registrantDID))
	}))
	t.Cleanup(listener.server.Close)
	return listener
}

// port returns the port the listener bound, so a request can name a host the
// seam resolves AND a port a test is allowed to bind.
func (l *countingListener) port(t *testing.T) string {
	t.Helper()

	_, port, err := net.SplitHostPort(l.server.Listener.Addr().String())
	require.NoError(t, err, "the listener's address must split into host and port")
	return port
}

// defaultClientRequest drives a handler's OWN client at a host the seam resolves
// to the listener, and reports the error and whether the listener was reached.
//
// It calls handler.httpClient directly rather than verifyDomainOwnership, and
// the reason is the constraint documented at the top of this file: a
// validation-passing domain has no port, so verifyDomainOwnership always builds
// https://<domain>/… on 443, and a test cannot bind 443. Naming the port here is
// what makes a reachable listener possible at all — and it costs nothing this
// test was claiming, because the subject is the CLIENT the constructor
// installed, not the URL the handler builds. NormalizeDomain's separate refusal
// of ports is pinned by register_domain_test.go and by the HTTP-layer split
// above.
func defaultClientRequest(t *testing.T, handler *RegisterHandler, listener *countingListener) (reached int64, err error) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+guardTestDomain+":"+listener.port(t)+wellKnownPath, nil)
	require.NoError(t, err)

	resp, doErr := handler.httpClient.Do(req)
	if doErr == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return listener.requests.Load(), doErr
}

// TestNewRegisterHandler_DefaultClientIsGuarded is the fence for every caller
// who never thinks about this.
//
// # WHY THIS TEST IS SHAPED THE WAY IT IS, AND WHAT IT USED TO MEASURE
//
// It used to consist of `reflect.TypeOf(registerHTTPClient(false).Transport)`
// compared against the handler's, plus the timeout. THAT ASSERTION CANNOT FAIL
// ON POLARITY. The hatch is a bool field INSIDE oauth's transport, not a
// different type, so registerHTTPClient(true) and registerHTTPClient(false)
// return an identical reflect.TypeOf — and a build whose constructor hardcoded
// the hatch OPEN passed this test while its name said DefaultClientIsGuarded and
// its comment said "forgetting is safe". A type comparison can see that the
// conversion happened; it can see nothing about which way the switch is set.
//
// So the contract is now carried by REACHABILITY, and the type and timeout
// checks are kept as the cheap secondary they always were.
//
// # THE HATCH OPTION IS ABSENT, WHICH IS THE POINT
//
// The guarded subtest passes NO PrivateHostOptions and NO WithPrivateHostsAllowed
// — only the resolver seam, which sets no policy and cannot open the guard. That
// is bit-for-bit the state a caller who never thought about any of this leaves
// the handler in, and it is the state cmd/server ships.
func TestNewRegisterHandler_DefaultClientIsGuarded(t *testing.T) {
	t.Parallel()

	newHandler := func(t *testing.T, listener *countingListener, opts ...RegisterHandlerOption) *RegisterHandler {
		t.Helper()

		seam := withTransportOptions(covesoauth.WithHostResolver(privateHostResolver(t, "127.0.0.1")))
		return NewRegisterHandler(
			&fakeUserService{registered: map[string]*users.User{}},
			&fakeIdentityResolver{identities: map[string]*identity.Identity{}},
			append(opts, seam)...,
		)
	}

	t.Run("the default client refuses a private address without reaching the listener", func(t *testing.T) {
		t.Parallel()

		listener := newCountingListener(t)
		handler := newHandler(t, listener) // no hatch option: the production default

		reached, err := defaultClientRequest(t, handler, listener)

		// THE REACHABILITY CLAIM COMES FIRST, deliberately. It is the security
		// property, and asserting it before the error means a regression reports
		// "the listener was reached" rather than the far less useful "an error
		// was expected but got nil".
		assert.Zerof(t, reached,
			"the listener was reached %d time(s), so NewRegisterHandler's own client dialled a host "+
				"whose DNS answer was 127.0.0.1 and delivered the request. Registration is "+
				"unauthenticated and rate-limited only 10/10min, so this client dials whatever "+
				"hostname a stranger posts. This is the assertion the old type comparison could not "+
				"make, and the one that dies if the constructor's hatch argument is ever replaced by "+
				"a constant", reached)

		require.Errorf(t, err,
			"the request SUCCEEDED against a private address, so the default client is not guarded "+
				"at all")
		assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
			"the refusal must carry the guard's identity, or a client that simply cannot make requests "+
				"satisfies this case too; got: %v", err)
	})

	t.Run("control: the same listener IS reached with the hatch open", func(t *testing.T) {
		t.Parallel()

		listener := newCountingListener(t)
		handler := newHandler(t, listener, WithPrivateHostsAllowed()) // coves:allow-ssrf-hatch: the control half; without it the guarded case above is unfalsifiable

		reached, err := defaultClientRequest(t, handler, listener)

		require.NoErrorf(t, err,
			"the hatch was open and the request still failed, so the guarded case above proves nothing: "+
				"a client that reaches nothing refuses a private address for reasons unconnected to "+
				"classification; got: %v", err)
		assert.Equalf(t, int64(1), reached,
			"the listener was reached %d time(s) with the hatch open, want exactly 1. The zero asserted "+
				"above only means something if this listener is reachable when classification permits it",
			reached)
	})

	t.Run("secondary: the transport and the timeout are the guarded builder's", func(t *testing.T) {
		t.Parallel()

		handler := NewRegisterHandler(
			&fakeUserService{registered: map[string]*users.User{}},
			&fakeIdentityResolver{identities: map[string]*identity.Identity{}},
		)

		require.NotNil(t, handler.httpClient, "the handler must carry an HTTP client")
		require.NotNil(t, handler.httpClient.Transport,
			"the handler's client uses http.DefaultTransport, which is the unguarded stdlib client")

		// A TYPE COMPARISON AND NOTHING MORE. It catches "the conversion never
		// happened"; it is blind to which way the hatch is set, which is why the
		// subtests above exist. Kept because the transport type is unexported in
		// oauth, so this is the strongest structural claim available from here.
		guarded := registerHTTPClient(false)
		assert.Equalf(t, reflect.TypeOf(guarded.Transport), reflect.TypeOf(handler.httpClient.Transport),
			"the default client's transport is %T, but the guarded builder produces %T",
			handler.httpClient.Transport, guarded.Transport)

		assert.Equal(t, 10*time.Second, handler.httpClient.Timeout,
			"the handler's own 10s ceiling must survive adopting the shared client, which ships 15s")
	})
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// single most important assertion for this call site.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch
// here and everywhere else that holds such a boolean. This function is the one
// place in the repository where the production branch is evaluated, which is why
// it is a pure function rather than an `if` in wiring — and RegisterAggregatorRoutes
// currently has no config access at all, so the alternative would be threading a
// boolean through three call layers to reach an inline conditional CI never runs.
//
// The claim is that there are NO options, not that the options are safe.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). What production gets must be exactly "+
			"NewRegisterHandler's own defaults, with nothing applied on top", len(opts))
}

// TestPrivateHostOptions_DisallowedHandlerIsGuarded is the behavioural half:
// zero options has to also MEAN a guarded client. The length check alone would
// still pass if the constructor's default regressed to permissive — the helper
// would be correctly returning nothing, onto a base that no longer refuses.
func TestPrivateHostOptions_DisallowedHandlerIsGuarded(t *testing.T) {
	t.Parallel()

	handler := guardedRegisterHandler(t, false, "169.254.169.254")

	err := handler.verifyDomainOwnership(context.Background(), registrantDID, guardTestDomain)

	require.Error(t, err,
		"a handler built from PrivateHostOptions(false) fetched from a domain resolving to the cloud "+
			"metadata endpoint. This is the branch production runs and CI never does")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"169.254.169.254 answers credential requests to anything that can reach it, and the refusal "+
			"must come from the guard rather than from the request happening to fail; got: %v", err)
}

// TestSetHTTPClientSeam_DoesNotSilentlyDowngradeAHandlerThatWasNeverGiven is the
// footgun, stated as the property that can actually be pinned.
//
// setHTTPClient replaces the client wholesale, and three fixtures in this
// package depend on that. Two of them — register_test.go's and
// register_users_row_test.go's, both via stubClient — install a transport with a
// pinned dialer so a request for example.com reaches an httptest listener;
// newRecordingHandler installs a RoundTripper that answers nothing at all. "Wrap
// rather than replace" would break every one of them — a wrapped guard would
// refuse example.com's real address, or fail to resolve it at all under an
// egress-blocked CI — so the seam has to keep replacing.
//
// What can be pinned HERE is that a handler nobody called it on is guarded,
// which is the state every production handler is in. The other half — a
// production caller reaching for the seam at all — is closed by the method being
// unexported, and asserted as a property rather than a name by
// TestNoExportedSeamCanReplaceTheGuardedClient.
func TestSetHTTPClientSeam_DoesNotSilentlyDowngradeAHandlerThatWasNeverGiven(t *testing.T) {
	t.Parallel()

	handler := NewRegisterHandler(
		&fakeUserService{registered: map[string]*users.User{}},
		&fakeIdentityResolver{identities: map[string]*identity.Identity{}},
	)
	// The client POINTER, not its transport: in a build where the handler's
	// client is not yet guarded the transport is nil, and testify's Same refuses
	// nil with "both arguments must be pointers" — a failure that reports the
	// assertion's own mechanics rather than the property under test.
	guardedClient := handler.httpClient

	// A second handler, built identically, is the one that gets overridden. The
	// first must be unaffected — a shared or package-level client would make one
	// fixture's override everyone's.
	other := NewRegisterHandler(
		&fakeUserService{registered: map[string]*users.User{}},
		&fakeIdentityResolver{identities: map[string]*identity.Identity{}},
	)
	other.setHTTPClient(&http.Client{Transport: &recordingTransport{}})

	assert.Samef(t, guardedClient, handler.httpClient,
		"overriding one handler's client changed another's. The client must be per-handler state: a "+
			"package-level default would let a single test fixture unguard the production handler for "+
			"the rest of the process")
	assert.NotNil(t, other.httpClient.Transport, "the override must install what it was given")
}

// TestRegisterHTTPClient_HatchIsPerConstruction pins that the builder's argument
// is what decides, at the level the gate helper feeds.
func TestRegisterHTTPClient_HatchIsPerConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		allowPrivate bool
		wantBlocked  bool
	}{
		{name: "guarded refuses a private answer", allowPrivate: false, wantBlocked: true},
		{name: "hatched permits it", allowPrivate: true, wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Loopback rather than an RFC1918 address on purpose: the permissive
			// row has to DIAL, and a dial to 10.0.0.1:443 hangs until the client's
			// 10s ceiling on a machine with no route to it, while loopback:443
			// answers "connection refused" immediately. Both are private, so the
			// guarded row is unaffected by the choice.
			client := registerHTTPClient(tt.allowPrivate,
				covesoauth.WithHostResolver(privateHostResolver(t, "127.0.0.1")))

			req, err := http.NewRequestWithContext(context.Background(),
				http.MethodGet, "https://"+guardTestDomain+wellKnownPath, nil)
			require.NoError(t, err)

			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err, "nothing answers at loopback:443, so this request must fail either way")

			if tt.wantBlocked {
				assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
					"a client built for the guarded branch must refuse an RFC1918 answer by "+
						"classification; got: %v", err)
				return
			}
			assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
				"a client built with the hatch open must get past classification and fail at the "+
					"dial instead; got: %v", err)
		})
	}
}
