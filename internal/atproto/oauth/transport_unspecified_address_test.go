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

	// The port is the server's; the host is rewritten to the unspecified address.
	// Rebuilding the URL rather than hardcoding a port is what keeps the target a
	// listener this test owns.
	target := "http://" + net.JoinHostPort("0.0.0.0", port) + "/"

	client := NewSSRFSafeHTTPClient(false)
	resp, err := client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
	}

	assert.False(t, reached.Load(),
		"GET %s reached the loopback listener: 0.0.0.0 passed the private-address check, and the kernel "+
			"then resolved it to the local host at connect time. The guard classified an address it never "+
			"actually dialled, so an attacker-supplied URL like http://0.0.0.0:5432/ hits a local service",
		target)
	require.Error(t, err, "GET %s must be refused", target)
	assert.Contains(t, err.Error(), "SSRF blocked",
		"the refusal must name the guard that made it, so a closed port cannot green this test; got: %v", err)
}
