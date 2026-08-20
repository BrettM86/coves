package oauth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeHTTPClient_RefusesTheUnspecifiedAddress pins that 0.0.0.0 is
// treated as the loopback address it behaves like.
//
// # WHY 0.0.0.0 REACHES A LOCAL SERVICE
//
// 0.0.0.0 is RFC 1122's "this host on this network", a wildcard that means
// something only in a bind() call. In a connect() call the kernel does not treat
// it as a wildcard — on Linux and on darwin alike it substitutes the local host,
// so connecting to 0.0.0.0:5432 lands on whatever is listening on
// 127.0.0.1:5432.
//
// That is what makes it dangerous to a guard built by enumerating blocks. An
// address classifier that listed loopback, link-local, RFC1918 and the IPv6
// private ranges — which is what this one did before this test — would find
// 0.0.0.0 in none of them and call it public, and its classification and the
// kernel's behaviour would then disagree about where the packet goes. The kernel
// is the one that opens the socket. The result would be a complete bypass rather
// than a gap: `http://0.0.0.0:5432/` is a URL an attacker types, it passes such a
// check on its face, and it reaches Postgres. Every input that reaches this
// transport is chosen by a stranger — a DID document's PDS endpoint, an
// acceptance record's subject — so nothing upstream constrains the literal to be
// one we would have picked.
//
// The predicate now recognises it (via IsUnspecified, which also covers :: and
// ::ffff:0.0.0.0), and this test is what holds that in place.
//
// It asserts through NewSSRFSafeHTTPClient rather than against `isPrivateIP`,
// because the contract is the client's refusal to CONNECT. A predicate returning
// true is evidence; a service that was never touched is the property.
//
// # WHY THE TARGET IS A HOSTNAME AND NOT THE LITERAL 0.0.0.0
//
// It used to be `http://0.0.0.0:PORT/` — the URL an attacker actually types —
// and that is now the wrong instrument. The transport refuses an IP-literal host
// outright, before it resolves anything, so a literal target would make this
// test pass on that check alone and never reach `isPrivateIP` at all. A green
// that cannot fail when the thing under test breaks is worse than no test: this
// file's whole subject is the CLASSIFIER's handling of 0.0.0.0, so the request
// has to reach the classifier. The host is therefore a name, and the lookup seam
// answers it with 0.0.0.0 — which is also a faithful model of the real attack,
// since a DID document's endpoint is a hostname and its zone is the attacker's.
//
// The literal check has its own coverage and does not need this file's:
// transport_ip_literal_test.go for the unit rows, and the outer acceptance
// contract in transport_response_cap_test.go end to end.
//
// EVERYTHING ELSE IS UNCHANGED, DELIBERATELY. The listener is real and the base
// transport is NOT substituted, so the kernel substitution this test is about
// still happens for real: a guard that classified 0.0.0.0 as public would hand
// that address to the dialler, the kernel would turn it into the local host at
// connect time, and `reached` would flip. That assertion is the point of the
// file and it is not weakened here.
func TestSSRFSafeHTTPClient_RefusesTheUnspecifiedAddress(t *testing.T) {
	t.Parallel()

	// A real listener on loopback, so the test can tell "refused" from "the port
	// happened to be closed". The handler flag is the load-bearing assertion: an
	// implementation that connects and only then reports an error still delivered
	// the request to a local service, and must fail here.
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err, "splitting the test server address %q", server.URL)

	// The port is the server's; the host is a name the seam below answers with
	// the unspecified address. Rebuilding the URL rather than hardcoding a port
	// is what keeps the target a listener this test owns.
	target := "http://" + net.JoinHostPort("unspecified.test", port) + "/"

	// Checked, not assumed. A typo here makes net.ParseIP return nil,
	// isPrivateIP(nil) returns false, and the address under test silently becomes
	// "no address at all" — a fixture that can turn the test into one which
	// asserts nothing.
	unspecified := net.ParseIP("0.0.0.0")
	require.NotNil(t, unspecified, "the test's own unspecified address must parse")

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = (&hostRoutedResolver{answers: map[string][]net.IP{
		"unspecified.test": {unspecified},
	}}).lookup

	resp, err := client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
	}

	assert.False(t, reached.Load(),
		"GET %s reached the loopback listener: the host resolved to 0.0.0.0, that address passed the "+
			"private-address check, and the kernel then substituted the local host at connect time. The guard "+
			"classified an address it never actually dialled, so an attacker-supplied endpoint resolving to "+
			"0.0.0.0 hits whatever is listening on 127.0.0.1 at that port",
		target)
	require.Error(t, err, "GET %s must be refused", target)
	assert.Contains(t, err.Error(), "SSRF blocked",
		"the refusal must name the guard that made it, so a closed port cannot green this test; got: %v", err)
}
