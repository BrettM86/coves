package oauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two properties of the guard, written as CHARACTERIZATION rather than
// regression tests.
//
// The distinction is worth keeping in view: neither test was written to drive a
// fix. Both properties are load-bearing and *incidental* — each falls out of
// where the vetting happens to sit rather than from anything that states it, so
// a reasonable-looking refactor could remove either and, before these tests
// existed, nothing would have noticed.
//
// Vetting inside `RoundTrip` is what gives per-hop redirect coverage, because
// `http.Client` calls `RoundTrip` once per hop; move the check up into a wrapper
// around `client.Do` and hop 2 goes unguarded while the rest of the suite stays
// green. Likewise the answer loop refuses on ANY private address in the slice,
// which is a `for` over all of them rather than an inspection of `ips[0]`.

// hostRoutedResolver answers by hostname and records what it was asked, which is
// what lets a redirect test distinguish "hop 2 was resolved and refused" from
// "hop 2 was never looked at".
type hostRoutedResolver struct {
	mu      sync.Mutex
	answers map[string][]net.IP
	asked   []string
}

func (r *hostRoutedResolver) lookup(host string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, host)
	if ips, ok := r.answers[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (r *hostRoutedResolver) hostsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// TestSSRFSafeHTTPClient_RevetsEachRedirectHop pins that a redirect target is
// vetted as thoroughly as the URL the caller supplied.
//
// A redirect is an attacker-controlled input arriving from a server the caller
// already agreed to talk to, and it is the classic way to launder an SSRF: hop 1
// is a public host that passes any front-door validation, and its 302 names
// 169.254.169.254. The only thing standing between that and the cloud metadata
// service is that the guard runs again on the second hop.
//
// `TestSSRFSafeHTTPClient_RedirectLimit` does not cover this — it calls
// `client.CheckRedirect` directly with fabricated requests and never performs a
// redirect, so it exercises the hop COUNT and nothing about hop CONTENT.
func TestSSRFSafeHTTPClient_RevetsEachRedirectHop(t *testing.T) {
	t.Parallel()

	// One real listener stands in for both hops. Its handler counts, and the
	// count is the assertion: since the dialler below sends every connection
	// here regardless of destination, an unvetted hop 2 would arrive as a second
	// invocation. Exactly one means the redirect was refused BEFORE the dial.
	var invocations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		invocations.Add(1)
		w.Header().Set("Location", "http://hop2.test/")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	// Checked, not assumed. A typo in either literal makes ParseIP return nil,
	// isPrivateIP(nil) returns false, and hop 2 "vets clean" — and because the
	// substituted dialler below ignores the address it is handed, the request
	// would still land on the same listener and produce a plausible-looking
	// failure. A fixture that can silently become nil can turn this whole test
	// into one that asserts nothing.
	hop1 := net.ParseIP("93.184.216.34")   // public: vets clean
	hop2 := net.ParseIP("169.254.169.254") // link-local: must be refused
	require.NotNil(t, hop1, "the test's own hop-1 address must parse")
	require.NotNil(t, hop2, "the test's own hop-2 address must parse")

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{
		"hop1.test": {hop1},
		"hop2.test": {hop2},
	}}

	client := NewSSRFSafeHTTPClient(false)
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	// The base transport is substituted so hop 1 can vet as PUBLIC while still
	// connecting to a loopback listener — no packet leaves the machine. That
	// deliberately discards the vetted-address-dialling property for the duration
	// of this test, which is safe only because
	// TestSSRFTransport_DialsOnlyTheAddressItVetted owns that property outright.
	// What is left under test here is RoundTrip's per-hop vetting, alone.
	serverAddr := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	transport.base = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, serverAddr)
		},
	}

	resp, err := client.Get("http://hop1.test/")
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "the redirect to a link-local host must be refused")
	assert.Contains(t, err.Error(), "SSRF blocked",
		"the refusal must name the guard that made it, so a transport error cannot be mistaken for a block; got: %v", err)

	assert.Equal(t, int64(1), invocations.Load(),
		"the listener was reached %d times. Every connection this test makes lands on that one server, so a "+
			"second invocation means hop 2 was dialled without being vetted — the redirect target is chosen by "+
			"hop 1, not by the caller, which is precisely how an SSRF gets laundered through a host that passes "+
			"the front door", invocations.Load())

	assert.Equal(t, []string{"hop1.test", "hop2.test"}, resolver.hostsAsked(),
		"both hops must go through resolution: hop 2 being absent would mean it was refused for some reason "+
			"other than what it resolves to, and a repeated hop 1 would mean the redirect target was never read")
}

// TestSSRFSafeHTTPClient_RefusesAMixedLookupAnswer pins that one private address
// anywhere in a lookup answer refuses the whole request.
//
// A hostname resolves to a SET, and the caller does not choose which member the
// dialler picks. An attacker who can publish two A records — one public, one
// 127.0.0.1 — needs the guard to inspect only `ips[0]`, or to be satisfied by
// "at least one answer is public", to get a coin-flip at a local service on every
// request. Refusing wholesale is the only answer that does not depend on which
// element the connection happens to use.
//
// Both orderings are pinned because a check that reads only the first answer
// passes one of them.
func TestSSRFSafeHTTPClient_RefusesAMixedLookupAnswer(t *testing.T) {
	t.Parallel()

	// Checked for the same reason as the redirect test's fixtures: a nil from a
	// typo'd literal is classified as public, which would quietly delete the
	// mixed-answer premise this test is built on.
	public := net.ParseIP("93.184.216.34")
	private := net.ParseIP("127.0.0.1")
	require.NotNil(t, public, "the test's own public address must parse")
	require.NotNil(t, private, "the test's own private address must parse")

	tests := []struct {
		name   string
		answer []net.IP
	}{
		{"public answer first", []net.IP{public, private}},
		{"private answer first", []net.IP{private, public}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolver := &hostRoutedResolver{answers: map[string][]net.IP{"mixed.test": tt.answer}}

			client := NewSSRFSafeHTTPClient(false)
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup

			// A dialler that records and then fails. Recording is the point: an
			// error alone cannot distinguish "the guard refused" from "the dial
			// was attempted and failed", and only the former is the property.
			var dialled atomic.Bool
			transport.base = &http.Transport{
				DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
					dialled.Store(true)
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
				},
			}

			resp, err := client.Get("http://mixed.test/")
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err, "an answer containing a private address must be refused")
			assert.Contains(t, err.Error(), "SSRF blocked",
				"the refusal must name the guard that made it; got: %v", err)
			assert.Contains(t, err.Error(), private.String(),
				"the refusal must name the address that caused it, so an operator reading the log knows which "+
					"of the answers was the problem; got: %v", err)

			assert.False(t, dialled.Load(),
				"a connection was attempted despite a private address in the answer. The dialler picks which "+
					"member of the set to use and the caller has no say, so any private member has to refuse the "+
					"whole request rather than hoping a public one is chosen")
		})
	}
}

// TestSSRFSafeTransport_BaseTransportFailsClosedWhenBypassed pins the dialler's
// refusal to connect when nothing has vetted the destination.
//
// The dial reads its approved addresses off the request context, where RoundTrip
// put them. Reaching the dialler with that value absent means the base transport
// was driven directly instead of through RoundTrip — the unguarded path the
// wrapper exists to prevent — and the dialler's answer is to refuse rather than
// to fall back on the address in `addr`. A fallback would be worse than no guard
// at all, because it would look guarded.
//
// Bypassing RoundTrip is therefore the thing under test, and it is why this test
// reaches into `transport.base` rather than calling `client.Get`. There is no
// way to exercise the branch through the public API: going through the front
// door is precisely what populates the context value.
//
// The branch had no coverage before this test, which matters more than it sounds
// now that `transport.base` substitution is an established pattern in this
// package (see the two tests above). A refactor that dropped the fail-closed
// check — or replaced it with a dial of `addr` — would leave the entire suite
// green while removing the last barrier on a code path that reaches any address
// a caller names.
func TestSSRFSafeTransport_BaseTransportFailsClosedWhenBypassed(t *testing.T) {
	t.Parallel()

	client := NewSSRFSafeHTTPClient(false)
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

	// Port 9 is discard. The point is that nothing should get far enough to find
	// out whether anything is listening: the refusal must come from the guard,
	// before a socket is opened.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9/", nil)
	require.NoError(t, err, "building the request")

	resp, err := transport.base.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "the base transport must refuse a request that carries no vetted address")
	assert.Contains(t, err.Error(), "the SSRF-safe transport was bypassed",
		"the refusal must say that the wrapper was skipped rather than report a generic dial failure — an "+
			"operator seeing this in a log needs to know a caller reached the base transport directly; got: %v", err)
	assert.Contains(t, err.Error(), "SSRF blocked",
		"the refusal must name the guard that made it; got: %v", err)
}
