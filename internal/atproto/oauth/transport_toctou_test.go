package oauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The window between checking an address and connecting to it.
//
// # WHY A SECOND LOOKUP IS A SECOND DECISION
//
// RoundTrip resolves the hostname, walks the answers, and refuses the request if
// any of them is private. Then it hands the URL — the HOSTNAME, not the vetted
// address — to the base transport, which resolves it AGAIN before dialling.
// Nothing binds the second answer to the first.
//
// That is a real capability, not a theoretical one, and DNS rebinding is the
// name for it: an attacker controls the zone, answers the first query with a
// public address and the second with 169.254.169.254, and the guard approves a
// host it never connects to. Every input that reaches this transport is chosen
// by a stranger — a DID document's PDS endpoint, an acceptance record's subject
// — so the attacker also picks when to flip.
//
// The fix is to stop passing a name to the dialler. Vet the addresses once, then
// dial ONE OF THE VETTED ADDRESSES, ignoring the hostname at connect time.
//
// This is asserted through the lookup seam rather than against real DNS, because
// the property is precisely about the SECOND resolution: a test that could not
// make the two answers differ could not tell a fixed transport from a broken one.

// flippingResolver answers safely the first time and privately afterwards —
// the smallest rebinding attack there is.
type flippingResolver struct {
	mu      sync.Mutex
	safe    net.IP
	private net.IP
	calls   int
}

func (r *flippingResolver) lookup(context.Context, string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 1 {
		return []net.IP{r.safe}, nil
	}
	return []net.IP{r.private}, nil
}

func (r *flippingResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestSSRFTransport_DialsOnlyTheAddressItVetted(t *testing.T) {
	// The "safe" address is the loopback the test server actually listens on.
	// Using a real listener means a transport that dials the vetted address
	// genuinely connects, so the test distinguishes "refused" from "connected to
	// the right place" rather than only observing failures.
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting the test server address: %v", err)
	}
	safe := net.ParseIP(host)
	if safe == nil {
		t.Fatalf("the test server address %q is not an IP", host)
	}

	resolver := &flippingResolver{safe: safe, private: net.ParseIP("169.254.169.254")}

	// allowPrivate is TRUE, which looks backwards and is the only way to state
	// the property in isolation. With the guard enabled, loopback is refused on
	// the first lookup and the test can never reach the second one — so it would
	// pass against a transport with the bug still in it. Disabling the private
	// check leaves exactly one thing under test: whether the address that was
	// vetted is the address that gets dialled.
	client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())
	transport, ok := client.Transport.(*ssrfSafeTransport)
	if !ok {
		t.Fatalf("NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	}
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://rebind.test:" + port + "/")
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
	}

	if !reached {
		t.Fatalf("the request never reached the address that was vetted (lookups: %d, err: %v). "+
			"RoundTrip resolved the hostname, approved the answer, and then handed the NAME to the base transport, "+
			"which resolved it again and dialled somewhere else — the approval described a host the connection never went to",
			resolver.lookups(), err)
	}

	// One lookup, and that is the assertion rather than an optimisation note: a
	// second resolution IS the vulnerability, because nothing constrains it to
	// agree with the first. A transport that pins the vetted address has no
	// reason to ask again.
	if got := resolver.lookups(); got != 1 {
		t.Errorf("the hostname was resolved %d times; the dial must reuse the address RoundTrip already vetted, "+
			"since any later answer is one the guard never saw", got)
	}
}

func TestSSRFTransport_StillRefusesAPrivateFirstAnswer(t *testing.T) {
	// The guard the pin above deliberately switches off, asserted on its own so
	// that closing the rebinding window cannot quietly widen what is allowed
	// through it.
	resolver := &flippingResolver{
		safe:    net.ParseIP("169.254.169.254"),
		private: net.ParseIP("169.254.169.254"),
	}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	if !ok {
		t.Fatalf("NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	}
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://metadata.test/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a hostname resolving to a link-local address must be refused")
	}
	if !strings.Contains(err.Error(), "SSRF blocked") {
		t.Errorf("the refusal must name the guard that made it, got: %v", err)
	}
}
