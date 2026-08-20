package oauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/idna"
)

// The guard resolves the hostname net/http would NEVER have dialled.
//
// # THE DEFECT
//
// RoundTrip takes req.URL.Hostname() raw and hands that exact string to the
// resolver. net/http does not: canonicalAddr runs the host through
// idnaASCIIFromURL → idna.Lookup.ToASCII before it becomes a dial address, a
// connection-pool key or a TLS ServerName (net/http/transport.go's
// canonicalAddr, and idnaASCII in net/http/request.go).
//
// So for https://bücher.example/xrpc/... the guard asks for the literal UTF-8
// string. On the PRODUCTION build (CGO_ENABLED=0) the pure-Go resolver refuses
// it inside the process: goLookupIPCNAMEOrder gates on isDomainName, which
// permits only [A-Za-z0-9._-], so a byte ≥ 0x80 falls through to its default
// case. Verified by probe — `lookup bücher.example: no such host` comes back
// with NO server named, while the A-label spelling names the resolver it
// actually queried.
//
// The consequence is an availability regression rather than a bypass: every
// atProto PDS on a non-ASCII domain became unreachable from this AppView at
// every site that adopted this client, and it fails closed, so nothing in CI
// noticed. No fixture in this tree uses an IDN host.
//
// # WHY THE FIX IS ORDERED THE WAY IT IS
//
// Normalization runs BEFORE the IP-literal shape check, and that ordering is
// security-critical rather than tidy — see
// TestSSRFSafeHTTPClient_NormalizesBeforeTheLiteralCheck, which is the row that
// proves it. IDNA does not merely punycode: its mapping table folds fullwidth
// and ideographic forms to ASCII, so a host that is not an IP literal on the way
// in can BE one on the way out.
//
// # WHY THE ASCII SHORT-CIRCUIT IS NOT AN OPTIMISATION
//
// TestSSRFSafeHTTPClient_ASCIIHostsAreUnaffectedByNormalization. The Lookup
// profile applies ValidateLabels, CheckHyphens and the BidiRule, and refuses
// ASCII hostnames that Go's own resolver resolves happily. Running every host
// through ToASCII would trade this availability bug for a wider one.

// dialRecorder stands in for the base transport and records the address
// net/http computed for the dial.
//
// That address is canonicalAddr's output, which is the SAME string net/http
// uses as the connection-pool key and — with the port stripped — as the TLS
// ServerName. So recording it here is how a test asks "is the name the guard
// vetted the name net/http would have connected to", which is the question the
// whole normalization change exists to answer yes to.
//
// It always refuses the connection, so nothing here can leave the machine even
// if every guard above it were deleted.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialRecorder) transport() *http.Transport {
	return &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			d.mu.Lock()
			d.addrs = append(d.addrs, addr)
			d.mu.Unlock()
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
		},
	}
}

func (d *dialRecorder) dialled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

// dialledHosts returns the recorded addresses with their ports stripped, which
// is the form to compare against what the resolver was asked.
func (d *dialRecorder) dialledHosts(t *testing.T) []string {
	t.Helper()
	hosts := make([]string, 0, len(d.addrs))
	for _, addr := range d.dialled() {
		host, _, err := net.SplitHostPort(addr)
		require.NoErrorf(t, err, "net/http handed the dialler %q, which carries no port", addr)
		hosts = append(hosts, host)
	}
	return hosts
}

// TestSSRFSafeHTTPClient_ResolvesTheALabelOfAnIDNHost is the regression
// reproducing.
//
// The assertion that carries the weight is the one about what the RESOLVER WAS
// ASKED, not the one about whether the request succeeded. A test that only
// checked for an error would pass against the broken transport, since the
// broken transport does produce an error — the wrong one, for the wrong reason,
// at the wrong layer.
func TestSSRFSafeHTTPClient_ResolvesTheALabelOfAnIDNHost(t *testing.T) {
	t.Parallel()

	const unicodeHost = "bücher.example"
	const aLabel = "xn--bcher-kva.example"

	// The premise, checked rather than assumed: if x/net/idna ever stopped
	// producing this A-label the test would be demanding the wrong string, and
	// would say so here rather than failing obscurely below.
	got, err := idna.Lookup.ToASCII(unicodeHost)
	require.NoError(t, err, "the Lookup profile must accept an ordinary IDN host")
	require.Equal(t, aLabel, got, "this is the A-label net/http computes for the same host")

	// A public answer, so the request survives classification and the test is
	// measuring normalization rather than the guard refusing for its own
	// reasons. It answers ONLY for the A-label: a transport that asks for the
	// raw UTF-8 string gets a DNS failure, which is exactly what production
	// gets.
	publicAnswer := net.ParseIP("93.184.216.34")
	require.NotNil(t, publicAnswer, "the fixture address must parse; a nil IP classifies as public and greens this test for nothing")

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{aLabel: {publicAnswer}}}
	recorder := &dialRecorder{}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup
	transport.base = recorder.transport()

	resp, err := client.Get("http://" + unicodeHost + "/xrpc/com.atproto.repo.getRecord")
	if err == nil {
		_ = resp.Body.Close()
	}

	// THE DISCRIMINATOR.
	assert.Equal(t, []string{aLabel}, resolver.hostsAsked(),
		"the guard asked the resolver for the hostname verbatim. net/http punycodes it before the string "+
			"becomes a dial address, so on the production build (CGO_ENABLED=0) the pure-Go resolver refuses the "+
			"raw UTF-8 inside the process and every PDS on a non-ASCII domain is unreachable through this client")

	// And it must have got PAST the guard rather than merely reaching it: a
	// normalization that resolved the A-label and then refused it would be the
	// same outage wearing a different error.
	assert.NotErrorIs(t, err, ErrBlockedAddress,
		"an IDN host resolving to a public address must not be refused; got: %v", err)
	assert.Equal(t, []string{aLabel}, recorder.dialledHosts(t),
		"the request must reach the dial, at the A-label, having been classified on the way")
}

// TestSSRFSafeHTTPClient_StillRefusesAnIDNHostResolvingToAPrivateAddress is the
// security-critical half: normalization must feed the classifier, never replace
// it.
//
// The listener is REAL and bound, and the assertion is that its handler was
// never invoked. Mutation testing on this package has already produced an
// implementation that classified correctly, rendered a byte-identical error and
// refused the request AFTER delivering it; every message assertion passed
// against it and only "was it actually reached" caught it.
func TestSSRFSafeHTTPClient_StillRefusesAnIDNHostResolvingToAPrivateAddress(t *testing.T) {
	t.Parallel()

	const unicodeHost = "bücher.example"
	const aLabel = "xn--bcher-kva.example"

	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	listenerHost, listenerPort, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err, "splitting the test server address")
	loopback := net.ParseIP(listenerHost)
	require.NotNil(t, loopback, "the test server address %q must parse as an IP", listenerHost)

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{aLabel: {loopback}}}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	// The IDN host at the REAL listener's port, so a transport that resolved
	// the A-label and failed to classify it would genuinely connect.
	resp, err := client.Get("http://" + unicodeHost + ":" + listenerPort + "/")
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "an IDN host resolving to loopback must be refused")
	assert.ErrorIs(t, err, ErrBlockedAddress,
		"the refusal must carry the guard's identity. A resolution failure that happens to stop the request "+
			"is a different control and does not hold once the name resolves; got: %v", err)

	// Proves the refusal is the classifier acting on the NORMALIZED name rather
	// than the resolver choking on the raw one — without this the test above
	// passes against the unfixed transport for entirely the wrong reason.
	assert.Equal(t, []string{aLabel}, resolver.hostsAsked(),
		"the classifier must have been handed the A-label")

	assert.False(t, reached,
		"the request reached the listener. Normalizing an IDN host must not become a way to reach a private "+
			"address — the whole point of doing it before vetting is that vetting still applies to what it produces")
}

// TestSSRFSafeHTTPClient_NormalizesBeforeTheLiteralCheck pins the ORDERING, and
// it is the row that makes the ordering a security question rather than a
// stylistic one.
//
// # IDNA MAPPING PRODUCES IP LITERALS
//
// The Lookup profile maps before it validates, and its mapping table folds
// fullwidth digits (U+FF10..U+FF19) and the ideographic full stop (U+3002) onto
// their ASCII equivalents. Verified by probe:
//
//	url.Parse("http://１２７。０。０。１:8080/").Hostname() == "１２７。０。０。１"
//	netip.ParseAddr of that                            → error, not a literal
//	idna.Lookup.ToASCII of that                        → "127.0.0.1"
//
// So a host that is NOT an IP literal when the shape check would see it becomes
// one after normalization. Put normalization after the literal check and the
// check inspects a string that is not yet the address it is about to become.
//
// # WHY THE PUBLIC ROW IS THE ONE THAT MATTERS
//
// For the loopback spelling, classification is a backstop: normalize late and
// the resolver is handed "127.0.0.1", which resolves to itself and is refused a
// few lines further down. The wrong answer, at the wrong layer, but refused.
//
// ８.８.８.８ has no backstop. It maps to a PUBLIC address, so classification
// cannot refuse it and the literal check is the only control there is — and the
// literal check exists precisely because a caller-supplied literal naming a
// public address is still a destination this AppView has no business reaching.
// Normalize late and that check is bypassable by respelling the digits.
//
// # THIS IS NOT LIVE TODAY
//
// It is a trap in the FIX, not a pre-existing hole: with no normalization at
// all the raw fullwidth string goes to the resolver and fails closed on both
// resolver modes (verified on pure-Go and cgo). What this test forbids is
// converting that closed failure into an open one while fixing the IDN outage.
func TestSSRFSafeHTTPClient_NormalizesBeforeTheLiteralCheck(t *testing.T) {
	t.Parallel()

	// A real listener, addressed in fullwidth digits, so the private row is a
	// reachability claim and not a shape claim.
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err, "splitting the test server address")

	tests := []struct {
		name string
		host string
		port string
		maps string
		why  string
	}{
		{
			name: "fullwidth loopback, in front of a real listener",
			host: "１２７。０。０。１",
			port: port,
			maps: "127.0.0.1",
			why: "classification would eventually refuse this one, but only after an attacker-supplied address " +
				"had been sent to a resolver — the step the literal check exists to skip",
		},
		{
			name: "fullwidth public literal",
			host: "８.８.８.８",
			port: "80",
			maps: "8.8.8.8",
			why: "PUBLIC, so classification cannot refuse it and the literal check is the only control. This is " +
				"the row where normalizing after the shape check has consequences",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The premise: not a literal as it arrives, a literal once mapped.
			// If either half stopped holding this test would be pinning
			// something that is no longer true.
			mapped, mapErr := idna.Lookup.ToASCII(tt.host)
			require.NoError(t, mapErr, "IDNA must map %q rather than refuse it", tt.host)
			require.Equal(t, tt.maps, mapped, "IDNA must fold %q onto an IP literal", tt.host)

			// An empty answer table: anything that reaches the resolver dies
			// there, so the assertion below is about what was ASKED and never
			// about whether the request failed.
			resolver := &hostRoutedResolver{answers: map[string][]net.IP{}}
			recorder := &dialRecorder{}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup
			transport.base = recorder.transport()

			resp, err := client.Get("http://" + tt.host + ":" + tt.port + "/")
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err, "a fullwidth spelling of an IP literal must be refused")
			assert.ErrorIsf(t, err, ErrBlockedAddress,
				"the refusal must be the literal check's, matchable by identity. %s; got: %v", tt.why, err)
			assert.Containsf(t, err.Error(), "is an address written where a hostname belongs",
				"the refusal must be the LITERAL wording, which is what says the check fired before resolution "+
					"rather than the classifier catching it afterwards; got: %v", err)

			// THE DISCRIMINATOR between the two orderings. A transport that
			// normalizes after the shape check hands the mapped literal to the
			// resolver; one that normalizes before it never asks at all.
			assert.Emptyf(t, resolver.hostsAsked(),
				"the transport sent %v to the resolver. %q maps to %s, which is an address written where a "+
					"hostname belongs, and it must be refused on shape before anything is resolved",
				resolver.hostsAsked(), tt.host, tt.maps)

			assert.Emptyf(t, recorder.dialled(),
				"the request reached the dialler at %v; the destination was chosen by whoever supplied the URL",
				recorder.dialled())
		})
	}

	assert.False(t, reached,
		"a fullwidth spelling of the loopback literal reached the listener")
}

// TestSSRFSafeHTTPClient_RefusesAHostItCannotNormalize covers the error branch,
// and the thing it pins is WHOSE error comes back.
//
// net/http's own idnaASCIIFromURL swallows the failure and falls back to the raw
// host, which is the right call there and the wrong one here: a guard that
// cannot determine the name it is about to vet must not proceed to vet
// something else. Failing closed is the easy half. The half worth a test is that
// the refusal carries ErrBlockedAddress, so a caller can tell "the guard refused
// this" from "DNS is having a bad day" — the two want opposite handling, and a
// name that cannot be normalized is not a transient condition to retry.
func TestSSRFSafeHTTPClient_RefusesAHostItCannotNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		why  string
	}{
		{
			name: "a label the CONTEXTJ rules refuse",
			host: "a‍b.example",
			why:  "a ZERO WIDTH JOINER outside the contexts that permit one",
		},
		{
			name: "a name the BidiRule refuses",
			host: "1ا.example",
			why:  "an ASCII digit leading a right-to-left label",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The premise: this host is one ToASCII genuinely refuses. Without
			// this the test could pass against a transport that never
			// normalizes, simply because the name does not resolve.
			_, mapErr := idna.Lookup.ToASCII(tt.host)
			require.Errorf(t, mapErr, "the Lookup profile must refuse %q (%s), or this row pins nothing", tt.host, tt.why)

			resolver := &hostRoutedResolver{answers: map[string][]net.IP{}}
			recorder := &dialRecorder{}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup
			transport.base = recorder.transport()

			resp, err := client.Get("http://" + tt.host + "/xrpc/x")
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err, "a host that cannot be normalized must be refused")
			assert.ErrorIsf(t, err, ErrBlockedAddress,
				"the refusal must name the guard that made it. Falling back to the raw host the way "+
					"net/http does would vet a string that is not the name anything would dial, and reporting "+
					"this as a resolution failure tells a caller to retry a name that can never work; got: %v", err)

			// A resolution failure is what this MUST NOT look like, asserted at
			// the resolver rather than by matching on the error's shape: if the
			// resolver was never asked, the failure cannot have come from it.
			assert.Emptyf(t, resolver.hostsAsked(),
				"the transport sent %v to the resolver after failing to normalize it", resolver.hostsAsked())
			assert.Emptyf(t, recorder.dialled(), "the request reached the dialler at %v", recorder.dialled())
		})
	}
}

// TestSSRFSafeHTTPClient_ASCIIHostsAreUnaffectedByNormalization is the test that
// pins the ASCII SHORT-CIRCUIT, and it is the reason the fix is four lines
// rather than two.
//
// # THE TRAP
//
// The obvious implementation runs every host through idna.Lookup.ToASCII. The
// Lookup profile is not a punycode encoder — it applies ValidateLabels,
// CheckHyphens and the BidiRule — so it REFUSES ASCII hostnames that Go's own
// resolver resolves and that a bare http.Client reaches today. Doing that would
// trade an availability bug for a wider one.
//
// net/http does not have this problem because idnaASCII short-circuits on
// ascii.Is before it ever calls ToASCII, and the TODO above that check says in
// as many words that skipping it "may be possible to have two IDNs that appear
// identical to the user where the ASCII-only version causes an error
// downstream". The short-circuit is what keeps the ASCII path byte-identical to
// what this AppView does today.
//
// # THE ROWS ARE REAL, NOT CONSTRUCTED
//
// Both are names Go's pure-Go resolver — the production one — accepts and
// QUERIES. Verified by probe against a live resolver: each came back naming the
// DNS server it was sent to, which is what distinguishes "asked and answered no"
// from "rejected inside the process" the way the raw UTF-8 host is.
//
//   - An underscore label. isDomainName permits '_' deliberately, citing
//     golang.org/issue/12421 and SRV-style names, and _atproto is the label
//     atProto's own handle resolution is built on. UTS#46 disallows U+005F
//     outright.
//   - Hyphens in the third and fourth positions. Ordinary, RFC-valid, resolvable
//     DNS, and exactly the shape CheckHyphens rejects because it is where a
//     punycode prefix lives.
//
// The leading- and trailing-hyphen spellings (-foo.example.com, foo-.example.com)
// are ALSO refused by the Lookup profile, and they are deliberately not asserted
// here: isDomainName rejects both as well, so they name nothing production could
// reach and pinning them would be pinning a difference with no consequence.
func TestSSRFSafeHTTPClient_ASCIIHostsAreUnaffectedByNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		why  string
	}{
		{
			name: "an underscore label",
			host: "_atproto.example.com",
			why:  "UTS#46 disallows U+005F; net.isDomainName permits it on purpose",
		},
		{
			name: "hyphens in the third and fourth positions",
			host: "aa--bb.example.com",
			why:  "CheckHyphens refuses it; it is ordinary resolvable DNS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The premise, and the whole reason the row is here: an
			// unconditional ToASCII would refuse this host.
			_, mapErr := idna.Lookup.ToASCII(tt.host)
			require.Errorf(t, mapErr,
				"the Lookup profile must refuse %q (%s). If it stops refusing it, this row no longer pins the "+
					"short-circuit and a different ASCII shape has to be found", tt.host, tt.why)

			publicAnswer := net.ParseIP("93.184.216.34")
			require.NotNil(t, publicAnswer, "the fixture address must parse; a nil IP classifies as public")

			// Keyed on the host EXACTLY as written. A transport that normalizes
			// this one asks for something else and gets a DNS failure.
			resolver := &hostRoutedResolver{answers: map[string][]net.IP{tt.host: {publicAnswer}}}
			recorder := &dialRecorder{}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup
			transport.base = recorder.transport()

			resp, err := client.Get("http://" + tt.host + "/xrpc/x")
			if err == nil {
				_ = resp.Body.Close()
			}

			assert.NotErrorIsf(t, err, ErrBlockedAddress,
				"%q was refused by the guard. It is an ASCII hostname this AppView reaches today and Go's "+
					"resolver queries; normalization must not narrow what ASCII names are acceptable, and the "+
					"ASCII short-circuit is what stops it doing so; got: %v", tt.host, err)

			assert.Equalf(t, []string{tt.host}, resolver.hostsAsked(),
				"an all-ASCII host must reach the resolver byte-identically. Running it through ToASCII "+
					"rewrites or refuses it — %s", tt.why)

			assert.Equalf(t, []string{tt.host}, recorder.dialledHosts(t),
				"%q must still reach the dial", tt.host)
		})
	}
}

// TestSSRFSafeHTTPClient_VetsTheNameNetHTTPWouldDial closes the question the fix
// raises but does not answer on its own: the guard normalizes the host it
// RESOLVES, and rewrites nothing in the URL, so three other consumers of that
// URL compute their own spelling of the host.
//
//   - The DIAL ADDRESS is unaffected either way, because this transport's
//     DialContext ignores the address it is handed and connects to the vetted
//     IPs, taking only the port from it.
//   - The CONNECTION-POOL KEY is connectMethodKey.addr, which is
//     cm.targetAddr, which is canonicalAddr(url) — punycoded by net/http
//     itself.
//   - The TLS SERVERNAME is cm.tlsHost(), the same targetAddr with its port
//     stripped — likewise punycoded by net/http itself.
//
// All three therefore agree with the guard rather than diverging from it,
// because both sides reach the A-label through the same function with the same
// short-circuit. This test states that as a property rather than as a claim in
// a comment: whatever net/http ends up dialling, it is the name the guard asked
// the resolver about.
//
// The one deliberate divergence is the error branch. net/http's
// idnaASCIIFromURL swallows a ToASCII failure and falls back to the raw host;
// the guard refuses. There is no discrepancy in that case either, for the
// blunt reason that the request never reaches net/http's transport at all —
// TestSSRFSafeHTTPClient_RefusesAHostItCannotNormalize is where that is pinned.
func TestSSRFSafeHTTPClient_VetsTheNameNetHTTPWouldDial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
	}{
		{name: "an IDN host", host: "bücher.example"},
		{name: "an ordinary ASCII host", host: "pds.example.com"},
		{name: "an ASCII host in mixed case", host: "PDS.Example.COM"},
		{name: "a host already in its A-label form", host: "xn--bcher-kva.example"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publicAnswer := net.ParseIP("93.184.216.34")
			require.NotNil(t, publicAnswer, "the fixture address must parse; a nil IP classifies as public")

			// permissiveResolver answers anything, so the request always
			// reaches the dial and the two spellings can be compared. What is
			// under test is agreement, not reachability.
			resolver := &permissiveResolver{answer: publicAnswer}
			recorder := &dialRecorder{}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup
			transport.base = recorder.transport()

			resp, err := client.Get("http://" + tt.host + "/xrpc/x")
			if err == nil {
				_ = resp.Body.Close()
			}

			asked := resolver.hostsAsked()
			require.Len(t, asked, 1, "the host must be resolved exactly once per request (err: %v)", err)

			assert.Equal(t, asked, recorder.dialledHosts(t),
				"the guard vetted %v while net/http computed %v for the dial address, the connection-pool key "+
					"and the TLS ServerName. A guard that approves one name while the connection is keyed and "+
					"authenticated under another has approved a host the connection never went to",
				asked, recorder.dialledHosts(t))
		})
	}
}

// permissiveResolver answers every name with the same address and records what
// it was asked. It exists because hostRoutedResolver's table is keyed on the
// host, and a test about WHICH host was asked cannot pre-key a table on the
// answer it is trying to discover.
type permissiveResolver struct {
	mu     sync.Mutex
	answer net.IP
	asked  []string
}

func (r *permissiveResolver) lookup(_ context.Context, host string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, host)
	return []net.IP{r.answer}, nil
}

func (r *permissiveResolver) hostsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}
