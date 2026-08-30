// coves:allow-host-literal-file: the only literal host:port here is 127.0.0.1:1, a never-dialled placeholder in the tests that assert construction-time behaviour (the gate panic, SkipDNSDomainSuffixes) and in the scoping test, whose point is that nothing is dialled at all; every request that IS made goes to an httptest listener.
package identity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Handle verification inside a stack with no DNS and no TLS.
//
// # THE PROBLEM
//
// atProto identity resolution has two legs: read the DID document, then confirm
// the handle it declares resolves BACK to that DID. The second leg is a DNS TXT
// lookup of _atproto.<handle> or a GET https://<handle>/.well-known/atproto-did,
// and the hermetic CI stack can do neither — it has no DNS server and no
// certificate authority, by design. Indigo reports a leg-two failure not as an
// error but by setting Handle to the reserved handle.invalid placeholder, so in
// CI every identity resolves to a non-handle. Nothing that depends on a verified
// handle can be tested there at all, and communities.handle is UNIQUE, so the
// first such community takes the placeholder and every later one collides.
//
// The relay already solved this and the solution is in the compose file
// (docker-compose.ci.yml, the trial-host resolver): a PDS serves
// /.well-known/atproto-did for every handle it hosts, keyed on the HOST HEADER.
// So the handle does not need to resolve in DNS — the request just needs to
// arrive at the right PDS with the right Host. That turns leg two back into
// something an in-network stack can answer, and it is a REAL verification: the
// PDS is the authority for the handles it mints, and it answers with the DID it
// actually issued them to.
//
// # WHY AN OPTION AND NOT A MODE
//
// WithWellKnownHosts is a SCOPED OVERRIDE, matching only the suffixes it is
// given, and every other handle keeps resolving exactly as before. It is not a
// bypass: the DID that comes back is still compared against the DID being looked
// up, so a PDS naming somebody else still fails verification. TestRewrite...
// DoesNotBypassTheBidirectionalCheck below is that assertion, and it is the one
// that keeps this from being a hole.
//
// It is gated on WithPrivateHostsAllowed for the same reason: the whole point is
// to dial an address inside our own network, so a caller who has not already
// declared they are pointing this resolver at their own machine has no business
// enabling it. Production can never reach that combination — cmd/server passes
// PrivateHostOptions(cfg.IsDevEnv), and its false branch returns NO options.
//
// # WHY THIS FILE DRIVES REAL INDIGO
//
// base_resolver_test.go's fakeDirectory answers from a map, which is right for
// the error taxonomy and useless here: the behaviour under test IS indigo's
// resolution path — LookupDID walking DeclaredHandle, ResolveHandle, and the
// resolvedDID != did comparison. A fake would assert that the fixture works.
// Everything here therefore drives real indigo resolution, exactly as
// resolver_guard_test.go does. Most of it does so against real httptest
// listeners; the scoping test instead records the request at the point it leaves
// the rewrite wrapper, because what it has to observe is a request that was NOT
// redirected, and standing up a listener can only show that one arrived
// somewhere.

const (
	// The suffix the CI stack's second PDS mints handles under, and the one
	// .env.ci maps. Real, so the test reads like the config
	// it models.
	wellKnownSuffix = ".pds2.test"
	wellKnownHandle = "alice" + wellKnownSuffix

	// A handle OUTSIDE the mapped namespace, used to prove the override is
	// scoped. Under .test — RFC 6761 reserves it and guarantees it resolves
	// nowhere — because this is the one case that reaches the resolver stack.
	unmappedHandle = "alice.other.test"
)

// declaringPLC is a PLC directory that serves one DID document declaring one
// handle, which is what makes indigo attempt leg two at all.
//
// Contrast newCountingPLC in resolver_guard_test.go, which deliberately declares
// NO handle so that its lookup stays on the machine. That comment names the
// constraint this whole feature exists to lift.
func declaringPLC(t *testing.T, did, declaredHandle, pdsURL string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+did {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
			`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
			did, declaredHandle, pdsURL)
	}))
	t.Cleanup(server.Close)
	return server
}

// wellKnownPDS answers /.well-known/atproto-did for exactly one handle, keyed on
// the HOST HEADER and nothing else.
//
// Keying on Host is the entire mechanism, so the fixture must be strict about
// it: a server that answered every request would pass this test with no rewrite
// implemented at all, because indigo would have reached it by accident of the
// URL alone. Recording the hosts it saw is how the test says which name the
// request was made in.
type wellKnownPDS struct {
	server   *httptest.Server
	wantHost string
	answer   string

	mu    sync.Mutex
	hosts []string
}

func newWellKnownPDS(t *testing.T, wantHost, answer string) *wellKnownPDS {
	t.Helper()

	pds := &wellKnownPDS{wantHost: wantHost, answer: answer}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pds.mu.Lock()
		pds.hosts = append(pds.hosts, r.Host)
		pds.mu.Unlock()

		if r.URL.Path != "/.well-known/atproto-did" || r.Host != pds.wantHost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, pds.answer)
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// hostPort is what the option is configured with: a bare host:port, no scheme.
//
// No scheme because the CI stack speaks plain HTTP between containers while
// indigo always builds an https:// URL for leg two — so the override has to
// carry the request across that gap, and a value with a scheme in it would let a
// caller ask for something the stack cannot serve.
func (p *wellKnownPDS) hostPort() string {
	return strings.TrimPrefix(p.server.URL, "http://")
}

func (p *wellKnownPDS) hostsSeen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// baseResolverOf digs out the base resolver, the layer that turns indigo's
// identity into ours.
//
// Driving that rather than the resolver NewResolver returns skips the caching
// wrapper, which would otherwise reach a postgresCache built over a nil *sql.DB.
// It is also the same choice resolver_guard_test.go makes and for the same
// second reason: a warm cache answers without a fetch and asserts nothing about
// the network path under test.
func baseResolverOf(t *testing.T, resolver Resolver) *baseResolver {
	t.Helper()

	caching, ok := resolver.(*cachingResolver)
	require.Truef(t, ok, "NewResolver must return the caching resolver, got %T", resolver)
	base, ok := caching.base.(*baseResolver)
	require.Truef(t, ok, "the caching resolver must wrap the base resolver, got %T", caching.base)
	return base
}

// TestWellKnownHosts_VerifiesAHandleThatDNSCannotResolve is the behaviour the
// option exists for.
//
// alice.pds2.test does not resolve anywhere and never will. The DID document
// declares it, the mapped suffix sends leg two to the listener standing in for
// pds2, the listener recognises the Host and answers with the DID it minted the
// handle for — and indigo, seeing the answer match the DID it was looking up,
// accepts the handle as verified. That is a genuine bidirectional verification
// carried out entirely inside the network, which is exactly what CI needs and
// cannot otherwise have.
func TestWellKnownHosts_VerifiesAHandleThatDNSCannotResolve(t *testing.T) {
	t.Parallel()

	pds := newWellKnownPDS(t, wellKnownHandle, testDID)
	plc := declaringPLC(t, testDID, wellKnownHandle, "https://pds.example.invalid")

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{wellKnownSuffix: pds.hostPort()}),
	)
	config.PLCURL = plc.URL

	got, err := baseResolverOf(t, NewResolver(nil, config)).Resolve(context.Background(), testDID)
	require.NoError(t, err)

	assert.Equal(t, wellKnownHandle, got.Handle,
		"the handle came back as %q. Leg two never completed, so every identity in the hermetic stack "+
			"is a non-handle — and communities.handle is UNIQUE, so the first one takes the placeholder "+
			"and every later community collides with it", got.Handle)
	assert.NotEqual(t, InvalidHandle, got.Handle)

	assert.Equalf(t, []string{wellKnownHandle}, pds.hostsSeen(),
		"the request must arrive AT the override address but IN the handle's name: a PDS serves "+
			"/.well-known/atproto-did per handle and has no other way to know which one is being asked "+
			"about. Rewriting the Host along with the address asks it about itself; got %v", pds.hostsSeen())
}

// recordingTransport stands in for the network: it answers the two requests a
// DID lookup makes, and remembers exactly what it was handed.
//
// It goes in through withHTTPClient, so the rewrite transport wraps IT — which
// is what makes it the right instrument here. Everything above observes the
// override by standing up the listener it points AT, which can only ever show
// that a rewritten request arrived somewhere. What has to be observed for an
// UNMAPPED handle is the opposite: that the request left the transport
// unchanged, still addressed to a name that resolves nowhere. Watching it at the
// point it exits the wrapper says that directly, and says it without a DNS query
// or a dial.
type recordingTransport struct {
	// did is the subject of the DID document served for PLC lookups.
	did string
	// declaredHandle is the alsoKnownAs alias in that document, which is what
	// sends indigo on to the well-known leg.
	declaredHandle string

	mu        sync.Mutex
	wellKnown []*http.Request
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == wellKnownPath {
		r.mu.Lock()
		r.wellKnown = append(r.wellKnown, req)
		r.mu.Unlock()

		// 404 is how a host says "I do not serve this handle", and indigo turns
		// it into ErrHandleNotFound and then the placeholder — which is exactly
		// what an unmapped, unresolvable handle should produce.
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
			Request:    req,
		}, nil
	}

	body := fmt.Sprintf(`{"id":%q,"alsoKnownAs":["at://%s"],`+
		`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
		r.did, r.declaredHandle, "https://pds.example.invalid")
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (r *recordingTransport) wellKnownRequests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.wellKnown...)
}

// TestWellKnownHosts_IsScopedToTheSuffixesItWasGiven is the half that stops this
// being a blanket bypass.
//
// The map is an allowlist of namespaces we operate, not a switch that turns
// verification into trust. A handle outside it must resolve exactly as it always
// did. An implementation that rewrote every request to the single configured
// host, or that treated "any override configured" as "skip verification", would
// pass the test above and fail this one.
//
// # WHY THIS ONE WATCHES THE TRANSPORT
//
// An earlier version let indigo try to resolve alice.other.test for real and
// asserted the placeholder came back. That worked, and it was not a T0 test: a
// name outside the map is by definition one nothing has redirected, so "resolve
// it and see" means a system DNS query and an outbound connection attempt for
// every run. .test is reserved by RFC 6761 and never resolves, so nothing was
// reached — but the tier's rule is that nothing leaves the process, and a
// machine behind a captive portal or a wildcard resolver could answer anyway.
//
// The property was never really about what the network says. It is about what
// the transport DOES: pass the request through untouched. Recording it at the
// point it leaves the wrapper observes that directly, and the assertion gets
// sharper rather than weaker — the old version could only say "verification
// failed", which a dozen unrelated faults also produce, while this says the URL
// was still addressed to the handle and the request never went anywhere else.
func TestWellKnownHosts_IsScopedToTheSuffixesItWasGiven(t *testing.T) {
	t.Parallel()

	network := &recordingTransport{did: testDID, declaredHandle: unmappedHandle}

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{wellKnownSuffix: "127.0.0.1:1"}),
		withHTTPClient(&http.Client{Transport: network}),
	)
	config.PLCURL = "http://plc.invalid:3002"

	got, err := baseResolverOf(t, NewResolver(nil, config)).Resolve(context.Background(), testDID)
	require.NoError(t, err,
		"an unverifiable handle is not a resolution failure: the DID document was read and the PDS "+
			"endpoint is usable, so indigo reports the placeholder rather than an error")

	assert.Equal(t, InvalidHandle, got.Handle,
		"%q is not under any mapped suffix, so nothing may have redirected its verification. A verified "+
			"handle here means the override fired for a namespace we do not operate", unmappedHandle)

	requests := network.wellKnownRequests()
	require.Lenf(t, requests, 1,
		"expected exactly one well-known fetch for %s, got %d — without it the assertions below are "+
			"vacuous", unmappedHandle, len(requests))

	// The whole property, in two fields. A rewrite replaces the URL's authority
	// with the mapped host:port and downgrades the scheme to http; leaving both
	// as indigo built them is what "untouched" means.
	sent := requests[0]
	assert.Equalf(t, unmappedHandle, sent.URL.Host,
		"the request for %s was re-addressed to %q. The map is an allowlist of namespaces we operate, "+
			"and %s is not in it — redirecting it sends another domain's handle verification to a host "+
			"we chose", unmappedHandle, sent.URL.Host, unmappedHandle)
	assert.Equalf(t, "https", sent.URL.Scheme,
		"the scheme was downgraded to %q, which only the rewrite does", sent.URL.Scheme)
}

// TestWellKnownHosts_DoesNotBypassTheBidirectionalCheck is the security
// assertion, and the reason the override is a redirection rather than a
// concession.
//
// Sending the request somewhere we chose does not make the answer true. The PDS
// here is reachable, recognises the Host, and answers confidently — with a
// DIFFERENT DID. That is what a handle reassigned to another account looks like,
// and it is what a compromised or misconfigured PDS in our own network looks
// like. Indigo compares the answer to the DID it was looking up and rejects it,
// and the override must not interfere with that comparison.
//
// Without this case the feature reads as "in CI, believe the PDS", which would
// make every handle-binding test in the tree prove nothing.
func TestWellKnownHosts_DoesNotBypassTheBidirectionalCheck(t *testing.T) {
	t.Parallel()

	// The PDS answers about a different account entirely.
	pds := newWellKnownPDS(t, wellKnownHandle, fixtureDID)
	plc := declaringPLC(t, testDID, wellKnownHandle, "https://pds.example.invalid")
	require.NotEqual(t, testDID, fixtureDID, "the two DIDs must differ or this asserts nothing")

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{wellKnownSuffix: pds.hostPort()}),
	)
	config.PLCURL = plc.URL

	got, err := baseResolverOf(t, NewResolver(nil, config)).Resolve(context.Background(), testDID)
	require.NoError(t, err)

	assert.Equal(t, InvalidHandle, got.Handle,
		"the PDS said %s owns %s while we were looking up %s, and the handle was accepted anyway. "+
			"The override redirects WHERE the question is asked; it must never change whether the "+
			"answer is checked", fixtureDID, wellKnownHandle, testDID)
	assert.NotEmpty(t, pds.hostsSeen(),
		"the PDS was never asked, so this passed without exercising the comparison at all")
}

// TestWellKnownHosts_RequiresThePrivateHostHatch pins the gate that keeps this
// out of production.
//
// The option's only purpose is to dial an address inside our own network, so it
// is meaningless without WithPrivateHostsAllowed and dangerous without the
// declaration that option represents: a resolver that redirected handle
// verification to an operator-supplied host WITHOUT anyone having said "this
// resolver points at my own machine" is an SSRF primitive wearing a config flag.
//
// Refusing loudly at construction rather than quietly ignoring the option is the
// point. A silently-dropped override fails much later, as handles that will not
// verify in an environment somebody believed they had configured, and the reason
// would be invisible. There is nowhere to return an error to — DefaultConfig
// returns a Config and NewResolver returns a Resolver, both without one — so the
// refusal is a panic, which is correct for a construction-time misconfiguration
// that must never be recoverable in a running server.
//
// Both constructors, because a hand-built Config that applies the option
// directly reaches NewResolver's own defaulting branch without passing through
// DefaultConfig — the same pair TestWithHTTPClient_IsHonouredByEveryConstructor
// covers, for the same reason.
func TestWellKnownHosts_RequiresThePrivateHostHatch(t *testing.T) {
	t.Parallel()

	hosts := map[string]string{wellKnownSuffix: "127.0.0.1:1"}

	t.Run("through DefaultConfig", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { DefaultConfig(WithWellKnownHosts(hosts)) },
			"a resolver was built that redirects handle verification to an operator-named host, with "+
				"nobody having declared it points at their own machine. Production reaches this "+
				"constructor through PrivateHostOptions(false), which returns NO options")
	})

	t.Run("through NewResolver", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() {
			config := Config{PLCURL: "http://plc.invalid:3002"}
			WithWellKnownHosts(hosts)(&config)
			NewResolver(nil, config)
		}, "the option was applied to a hand-built Config, which never passes through DefaultConfig's "+
			"check — NewResolver must make the same refusal or the gate has a way around it")
	})

	t.Run("together with the hatch it is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NotPanics(t, func() {
			DefaultConfig(WithPrivateHostsAllowed(), WithWellKnownHosts(hosts))
		}, "the legitimate combination must build, or the gate has swallowed the feature")
	})
}

// TestWellKnownHosts_SkipsDNSForMappedSuffixes is the hermeticity requirement,
// and it is not merely an optimisation.
//
// Indigo tries DNS BEFORE the well-known fetch. In the stack this feature exists
// for there is no DNS server at all, so that attempt is at best a wasted
// round trip and at worst a hang on every identity resolved — and the T0 tier
// forbids leaving the machine, which is precisely why newCountingPLC in
// resolver_guard_test.go declares no handle. Skipping DNS for the suffixes we
// have an override for is what makes the whole path local.
//
// This asserts the mechanism rather than the outcome because the absence of a
// DNS query is not observable through any seam this package exposes.
// SkipDNSDomainSuffixes is indigo's own mechanism for it; if a later
// implementation achieves the same thing another way — a stub net.Resolver, say
// — rewrite this assertion rather than reverting the property.
func TestWellKnownHosts_SkipsDNSForMappedSuffixes(t *testing.T) {
	t.Parallel()

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{wellKnownSuffix: "127.0.0.1:1"}),
	)
	config.PLCURL = "http://plc.invalid:3002"

	dir, ok := baseResolverOf(t, NewResolver(nil, config)).directory.(*indigoIdentity.BaseDirectory)
	require.Truef(t, ok, "the base resolver must hold an indigo BaseDirectory, got %T",
		baseResolverOf(t, NewResolver(nil, config)).directory)

	assert.Containsf(t, dir.SkipDNSDomainSuffixes, wellKnownSuffix,
		"handle verification for %s still attempts DNS first. The stack this option exists for has no "+
			"DNS server, so that is a wasted round trip at best and a hang on every resolution at worst",
		wellKnownSuffix)
}

// TestWellKnownHosts_PrefersTheLongestMatchingSuffix pins WHICH host answers
// when more than one suffix matches.
//
// Overlapping suffixes are refused by config parsing, so this is about the
// option itself, which anything in-process may call with any map — the CI stack
// wiring, a future second instance, a test. The transport walks its map and
// takes the first suffix that matches, and Go randomises map iteration ORDER ON
// PURPOSE, so with `.test` and `.pds2.test` both configured, `alice.pds2.test`
// goes to a host chosen by the runtime afresh on every request.
//
// That is the worst failure mode available: not "wrong", but "wrong sometimes".
// Handle verification would succeed and fail alternately for the same identity,
// the difference would vanish under any attempt to reproduce it, and nothing in
// the resolver's output says which host it picked.
//
// The longest match is the right rule and not merely a deterministic one. A more
// specific suffix is a deliberate carve-out from a broader one — that is the only
// reason to configure both — so the specific entry is the operator's actual
// intent for the names it covers.
//
// # WHY THE LOOP
//
// One resolution proves nothing. Map iteration is randomised per range
// statement, so a single pass has an even chance of picking correctly by
// accident; a test that ran once would fail about half the time and be written
// off as flaky. Repeating collapses that to a vanishing probability and turns
// the assertion into a statement about determinism rather than about one draw.
func TestWellKnownHosts_PrefersTheLongestMatchingSuffix(t *testing.T) {
	t.Parallel()

	// The broader suffix answers with the WRONG DID. Choosing it does not merely
	// send the request elsewhere — it fails verification, which is what makes a
	// misroute visible in the Handle rather than only in the request log.
	broad := newWellKnownPDS(t, wellKnownHandle, fixtureDID)
	specific := newWellKnownPDS(t, wellKnownHandle, testDID)
	plc := declaringPLC(t, testDID, wellKnownHandle, "https://pds.example.invalid")

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{
			".test":         broad.hostPort(),
			wellKnownSuffix: specific.hostPort(),
		}),
	)
	config.PLCURL = plc.URL
	resolver := baseResolverOf(t, NewResolver(nil, config))

	const attempts = 20
	for attempt := range attempts {
		got, err := resolver.Resolve(context.Background(), testDID)
		require.NoErrorf(t, err, "attempt %d", attempt)
		require.Equalf(t, wellKnownHandle, got.Handle,
			"attempt %d resolved to %q. %q matches both %q and %q, and the broader entry answers about "+
				"another DID — so the handle verifies or does not depending on which one the map "+
				"iteration happened to reach first",
			attempt, got.Handle, wellKnownHandle, ".test", wellKnownSuffix)
	}

	assert.Emptyf(t, broad.hostsSeen(),
		"the broader %q host was dialled %d of %d times. A more specific suffix is a carve-out from a "+
			"broader one — that is the only reason to configure both — so it must win for the names it "+
			"covers, every time and not most of the time", ".test", len(broad.hostsSeen()), attempts)
	assert.Lenf(t, specific.hostsSeen(), attempts,
		"the %q host answered %d of %d resolutions", wellKnownSuffix, len(specific.hostsSeen()), attempts)
}

// TestWellKnownHosts_LowercasesTheSuffixesItWasGiven pins that a suffix given in
// any case works, and works in BOTH places the map is consumed.
//
// DNS names are case-insensitive, so `.PDS2.TEST` and `.pds2.test` name the same
// namespace and an operator may reasonably write either. Both consumers compare
// against an already-lowered hostname — the transport lowers the handle before
// matching, and indigo normalises a handle before testing it against
// SkipDNSDomainSuffixes — so an uppercase key matches nothing in either.
//
// The two are asserted together because they fail INDEPENDENTLY and the second
// failure is invisible. Lowercasing only inside the transport leaves DNS
// unskipped for the namespace: verification still succeeds, so nothing looks
// wrong, while every resolution first waits on a DNS server the hermetic stack
// does not have. That is the hang TestWellKnownHosts_SkipsDNSForMappedSuffixes
// exists to prevent, reintroduced through a path that test does not cover
// because it passes its suffix already lowercased.
func TestWellKnownHosts_LowercasesTheSuffixesItWasGiven(t *testing.T) {
	t.Parallel()

	pds := newWellKnownPDS(t, wellKnownHandle, testDID)
	plc := declaringPLC(t, testDID, wellKnownHandle, "https://pds.example.invalid")

	config := DefaultConfig(
		WithPrivateHostsAllowed(),
		WithWellKnownHosts(map[string]string{".PDS2.TEST": pds.hostPort()}),
	)
	config.PLCURL = plc.URL
	base := baseResolverOf(t, NewResolver(nil, config))

	got, err := base.Resolve(context.Background(), testDID)
	require.NoError(t, err)
	assert.Equal(t, wellKnownHandle, got.Handle,
		"the rewrite did not fire for %q against the suffix %q. The transport lowers the handle before "+
			"matching, so a key that kept its case matches nothing — and the failure is silent, because "+
			"an unrewritten request just fails verification the way it always did",
		wellKnownHandle, ".PDS2.TEST")

	dir, ok := base.directory.(*indigoIdentity.BaseDirectory)
	require.Truef(t, ok, "the base resolver must hold an indigo BaseDirectory, got %T", base.directory)
	assert.Containsf(t, dir.SkipDNSDomainSuffixes, wellKnownSuffix,
		"SkipDNSDomainSuffixes carries %v rather than the lowercased %q. Indigo normalises a handle "+
			"before testing it against these, so an uppercase entry never matches and DNS is attempted "+
			"for the one namespace we configured precisely because DNS cannot answer for it",
		dir.SkipDNSDomainSuffixes, wellKnownSuffix)
}
