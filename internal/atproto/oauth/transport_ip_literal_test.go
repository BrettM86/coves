package oauth

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ipLiteralHosts are the URL host spellings that must be recognised as
// addresses rather than names. One table, shared by the hatch-closed and
// hatch-open tests below, so the two cannot drift apart — a spelling refused in
// production but also refused in dev is an outage, and the pairing is the point.
//
// urlHost is what goes in the URL (brackets and all, for IPv6); hostname is what
// url.Hostname() yields from it, which is what the check actually sees.
var ipLiteralHosts = []struct {
	name     string
	urlHost  string
	hostname string
}{
	// A PUBLIC dotted quad, and the row that carries this test. Every other
	// spelling here is one the classifier would refuse anyway once it resolved,
	// so only this one can distinguish "refused because it is a literal" from
	// "refused because of what it points at". The fake dialler ensures the real
	// public address is never contacted.
	{"public dotted quad", "8.8.8.8", "8.8.8.8"},

	// Bracketed public IPv6. The brackets are URL syntax
	// rather than part of the address: url.Hostname() strips them, so a check
	// written against req.URL.Host instead of req.URL.Hostname() sees
	// "[2600::1]" and net.ParseIP returns nil for it.
	{"bracketed IPv6", "[2600::1]", "2600::1"},

	// An uppercase-hex IPv4-mapped spelling of 127.0.0.1. net.ParseIP normalises
	// it — verified, not assumed — which is exactly why the check must stay a
	// ParseIP call. A hand-rolled "does it look like a dotted quad" string test,
	// the obvious optimisation for a per-request hot path, misses this and every
	// other IPv6 spelling.
	{"uppercase-hex mapped loopback", "[::FFFF:7F00:1]", "::FFFF:7F00:1"},
}

// literalProbe is a client whose resolver and dialler are both recorded and
// neither of which touches the network.
//
// Both seams are needed, and for different reasons. The RESOLVER records whether
// the transport asked a question it had already been told the answer to — a
// literal is the address, so a lookup is a second decision on an
// attacker-influenced answer. The DIALLER records whether the request got out at
// all, which is what separates a refusal from a connection that merely failed.
type literalProbe struct {
	client   *http.Client
	resolver *hostRoutedResolver
	dialled  *atomic.Bool
}

func newLiteralProbe(t *testing.T, allowPrivate bool, hostname string) *literalProbe {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) returns false, so a typo'd literal
	// would be classified as public and the row would pass or fail for reasons
	// unconnected to its subject.
	answer := net.ParseIP(hostname)
	require.NotNil(t, answer, "the test's own host %q must parse as an IP address", hostname)

	// The resolver answers the literal with itself, which is what the real
	// resolver does for a literal. So if the literal check is missing, the
	// request proceeds exactly as it would in production — through
	// classification and on to a dial — and the recorders below capture it.
	resolver := &hostRoutedResolver{answers: map[string][]net.IP{hostname: {answer}}}

	client := NewSSRFSafeHTTPClient(PrivateAddressOptions(allowPrivate)...)
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	// Substituting the base transport discards the dials-only-vetted-addresses
	// property for the duration of these tests, which is safe only because
	// TestSSRFTransport_DialsOnlyTheAddressItVetted owns that property outright.
	// What it buys is that no packet can leave the machine even when the guard
	// is expected NOT to refuse — the hatch-open case below.
	var dialled atomic.Bool
	transport.base = &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialled.Store(true)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
		},
	}

	return &literalProbe{client: client, resolver: resolver, dialled: &dialled}
}

// TestSSRFSafeHTTPClient_RefusesIPLiteralHostsWithTheHatchClosed pins that an
// address written where a name belongs is turned away before anything is
// resolved.
//
// # WHY A LITERAL IS REFUSABLE AT ALL
//
// A legitimate atProto endpoint is always a hostname: the handle specification
// forbids IP literals, and a DID document's serviceEndpoint is an HTTPS URL with
// a hostname. So there is no traffic to lose, and refusing the whole shape is
// cheaper and more total than classifying whatever it points at.
//
// # WHY BEFORE RESOLUTION RATHER THAN AFTER
//
// The dotted-quad row is public, so classification would wave it through — that
// is the case the classifier cannot help with, and it is the reason this check
// exists rather than being another range in the list. For the spellings the
// classifier WOULD catch, refusing first still matters: resolving a literal asks
// a question whose answer the URL already gave, and the answer comes back from a
// resolver an attacker may control.
//
// # WHAT THIS DELIBERATELY DOES NOT COVER
//
// The obfuscated forms — 0x7f.0.0.1, 2130706433, 127.1, 127.0.0.1. — are NOT
// here. net.ParseIP returns nil for all four, so they are not literals as far as
// this check is concerned and they fall through to the resolver. They are
// refused today on both platforms, but by different mechanisms (a cgo build
// resolves them to loopback and IsLoopback catches them; the pure-Go resolver on
// the production build rejects them as malformed hostnames), so a test asserting
// the mechanism would be platform-dependent. In either case a resolved address
// still passes through classification before dialing; the deterministic
// production resolver policy is documented in docs/SSRF_SECURITY.md.
func TestSSRFSafeHTTPClient_RefusesIPLiteralHostsWithTheHatchClosed(t *testing.T) {
	t.Parallel()

	for _, tt := range ipLiteralHosts {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newLiteralProbe(t, false, tt.hostname)

			resp, err := probe.client.Get("http://" + tt.urlHost + "/")
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err, "GET http://%s/ must be refused with the hatch closed", tt.urlHost)

			assert.ErrorIs(t, err, ErrBlockedAddress,
				"the refusal must carry the guard's sentinel so a caller can tell it from a network failure; "+
					"got: %v", err)
			assert.Contains(t, err.Error(), "SSRF blocked",
				"the refusal must keep the prefix the rest of this package asserts on; got: %v", err)

			assert.Empty(t, probe.resolver.hostsAsked(),
				"the transport resolved %q (hosts asked: %v). The URL already names the address that will be "+
					"dialled, so a lookup here is a second decision taken on an answer the caller does not "+
					"control — the literal has to be refused before resolveHost runs",
				tt.hostname, probe.resolver.hostsAsked())

			assert.False(t, probe.dialled.Load(),
				"a connection was attempted to %s. An error returned after the dial is not a refusal: the "+
					"packet has already gone, and for a literal naming a local service that is the whole of "+
					"the SSRF", tt.urlHost)
		})
	}
}

// TestSSRFSafeHTTPClient_AllowsIPLiteralHostsWithTheHatchOpen is the regression
// fence around the test suite itself.
//
// Every integration fixture in this tree is served from an httptest listener,
// which is to say from a loopback IP literal, and two suites drive THIS client
// at one: internal/core/blobs/fetch_guard_test.go via WithPrivateHostsAllowed,
// and internal/core/blueskypost/service_test.go via allowPrivateHost. A literal
// check that ignores allowPrivate does not merely fail those tests — it makes
// the dev environment unable to reach anything local, which is what a dev
// environment is for.
//
// The assertion is that the request reaches the DIALLER, not that it succeeds:
// the fake dialler above always fails, so success is not available and is not
// the property. Getting as far as the dial is proof the guard did not refuse.
func TestSSRFSafeHTTPClient_AllowsIPLiteralHostsWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	for _, tt := range ipLiteralHosts {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newLiteralProbe(t, true, tt.hostname)

			resp, err := probe.client.Get("http://" + tt.urlHost + "/")
			if err == nil {
				_ = resp.Body.Close()
			}

			assert.True(t, probe.dialled.Load(),
				"GET http://%s/ never reached the dialler with allowPrivate set, so the literal check is "+
					"unconditional rather than gated on the hatch. Every httptest fixture in this tree is "+
					"addressed by IP literal", tt.urlHost)

			assert.NotErrorIs(t, err, ErrBlockedAddress,
				"the request was refused by the guard despite the hatch being open; got: %v", err)
		})
	}
}
