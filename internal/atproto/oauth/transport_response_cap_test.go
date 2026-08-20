package oauth

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeHTTPClient_ProtectsACallerThatValidatesNothing is the binding
// acceptance contract for the hardened transport.
//
// # THE CALLER THIS IS WRITTEN FOR
//
// Nine fetch sites are about to be pointed at this client, and every one of them
// was written by someone who assumed the URL was trustworthy: no address check,
// no size limit, `io.ReadAll` on whatever comes back. That assumption is false at
// all nine — the URL is a DID document's `serviceEndpoint`, a community record's
// domain, a user's `PDSURL` — and rewriting nine call sites to validate for
// themselves is nine chances to get it wrong and nine places for the next one to
// forget.
//
// So the property is not "the transport offers protections". It is that the
// transport ALONE protects a caller that caps nothing and validates nothing. The
// two subtests are the two halves of that: where the connection is allowed to go,
// and how much is allowed back.
//
// # WHY THIS IS WRITTEN AGAINST THE CONSTRUCTOR
//
// Everything here goes through `NewSSRFSafeHTTPClient` and a real listener,
// never against `isPrivateIP`. A predicate returning true is evidence; a service
// that was never touched, and a read that failed rather than lied, are the
// properties. The unit tests in this package own the classifier's rows — this
// file owns what a caller experiences.
func TestSSRFSafeHTTPClient_ProtectsACallerThatValidatesNothing(t *testing.T) {
	t.Parallel()

	// (a) THE CONNECTION. A legitimate atProto endpoint is always a hostname —
	// the handle spec forbids IP literals and the DID spec requires an HTTPS URL
	// with a hostname — so a caller-supplied URL whose host is a dotted quad is
	// never a destination this AppView has business reaching, and refusing it
	// outright is cheaper than classifying it.
	//
	// THE DISCRIMINATOR IS "WAS THE NAME EVER LOOKED UP", NOT "WAS IT REFUSED".
	// A loopback literal is already refused today by the classifier, so a test
	// that points at 127.0.0.1 and asserts an error proves the PREVIOUS PR and
	// nothing about this one. What is new is WHERE the refusal happens: a literal
	// must be turned away before `resolveHost` runs at all.
	t.Run("an IP-literal URL is refused before the name is ever resolved", func(t *testing.T) {
		t.Parallel()

		// A genuinely public address, so the classifier cannot green this case
		// and the assertion below can only be satisfied by the literal check. The
		// test resolver still maps it to loopback, so no packet can leave.
		const publicLiteral = "8.8.8.8"

		t.Run("hatch closed: refused without a lookup", func(t *testing.T) {
			t.Parallel()

			var reached atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			// The listener's own address, read back rather than written down.
			// The resolver below answers the public literal with it, which is
			// what keeps every packet on loopback AND what arms the handler
			// tripwire: a guard that permitted this request would dial the
			// vetted address and land on this listener for real.
			listenerHost, port, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(t, err, "splitting the test listener address %q", server.Listener.Addr())
			listenerIP := net.ParseIP(listenerHost)
			require.NotNil(t, listenerIP,
				"the test's own listener address %q must parse. isPrivateIP(nil) returns false, so a nil here "+
					"would be classified as public and this case would assert nothing", listenerHost)

			resolver := &hostRoutedResolver{answers: map[string][]net.IP{
				publicLiteral: {listenerIP},
			}}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup

			target := "http://" + net.JoinHostPort(publicLiteral, port) + "/"
			resp, err := client.Get(target)
			if err == nil {
				_ = resp.Body.Close()
			}

			// The discriminating assertion. Everything below it passes today.
			assert.Empty(t, resolver.hostsAsked(),
				"the transport sent %q to the resolver (hosts asked: %v). A dotted quad IS the address that "+
					"will be dialled, so resolving it asks a question the URL had already answered — and the "+
					"answer comes back from a resolver the attacker may control, which is one more chance for "+
					"the destination to become something other than what was classified. With the hatch closed "+
					"the literal must be refused before resolveHost runs",
				publicLiteral, resolver.hostsAsked())

			// Standing anti-cheat, not the discriminator: it passes before and
			// after. Mutation testing on the previous PR produced an
			// implementation that classified correctly, emitted a byte-identical
			// error, and refused the request AFTER delivering it — every
			// error-message assertion passed against it and only this one caught
			// it.
			assert.False(t, reached.Load(),
				"GET %s was delivered to the listener. A caller that hands this client an attacker-chosen URL "+
					"performs no check of its own, so refusing after the request lands is not a refusal at all — "+
					"the service has already acted on it", target)

			require.Error(t, err, "GET %s must be refused", target)
			assert.Contains(t, err.Error(), "SSRF blocked",
				"transport_unspecified_address_test.go:78 establishes that prefix as the refusal contract, and "+
					"the literal refusal must keep it so a transport error cannot be mistaken for a block; got: %v", err)
		})

		// The gating, and it is a regression fence rather than decoration.
		// internal/core/blobs/fetch_guard_test.go and
		// internal/core/blueskypost/service_test.go both point this client at an
		// httptest server — which is to say at a loopback IP literal — with the
		// hatch open, because every integration fixture in the tree is served
		// from loopback. A literal check that is not gated on allowPrivate takes
		// both of those suites down with it.
		t.Run("hatch open: the literal is dialled", func(t *testing.T) {
			t.Parallel()

			var reached atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			// server.URL is a real IP literal — httptest listens on loopback —
			// which is exactly the shape the case above refuses.
			client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())
			resp, err := client.Get(server.URL)
			require.NoError(t, err,
				"the dev hatch must still reach a loopback listener addressed by IP literal. Every integration "+
					"fixture in this tree is served that way, so a literal check that ignores allowPrivate is an "+
					"outage in the test suite and in dev; got: %v", err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode, "the hatch-open request must complete normally")
			assert.True(t, reached.Load(),
				"the request never reached the listener even with allowPrivate set, so the literal refusal is "+
					"unconditional rather than gated on the hatch")
		})
	})

	// (b) THE RESPONSE. A destination that passes the address check still
	// chooses how much it sends, and an unbounded `io.ReadAll` at the call site
	// is an out-of-memory the remote host triggers at will.
	//
	// THE FAILURE MODE THIS PINS IS NOT "no cap" — IT IS A CAP THAT TRUNCATES.
	// A wrapper that stops at the limit by returning io.EOF reads, to every
	// caller in the tree, as a complete body: `io.ReadAll` treats io.EOF as the
	// clean end of the stream and returns `err == nil`, so the caller gets a
	// short body it has no way to know is short. For the unfurl provider that
	// means parsing half a document; for a signature check it means verifying a
	// prefix. Silent truncation is worse than no cap, because no cap at least
	// fails loudly.
	//
	// The response is deliberately sent WITHOUT a Content-Length: the header is
	// chosen by the same attacker as the body, so the cap that matters is the
	// one on the bytes actually read.
	t.Run("an over-large body fails the read instead of truncating", func(t *testing.T) {
		t.Parallel()

		const (
			maxResponseBytes = 64 << 10 // the cap this client is given
			payloadBytes     = 1 << 20  // what the server sends: sixteen times the cap
		)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Chunked, and the write error is the exit condition rather than a
			// failure: once the cap engages the client drops the connection, and
			// the handler noticing that is the expected end of this response.
			chunk := make([]byte, 4<<10)
			for sent := 0; sent < payloadBytes; sent += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		defer server.Close()

		// allowPrivate, because the listener is on loopback and subtest (a) is
		// the proof that a production client would refuse it. What is under test
		// here is the cap alone.
		client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed(), WithMaxResponseBytes(maxResponseBytes))

		resp, err := client.Get(server.URL)
		require.NoError(t, err, "the request itself must succeed — the cap belongs to the body, not the dial")
		defer func() { _ = resp.Body.Close() }()

		// io.ReadAll is exactly what the nine call sites do, which is why it is
		// what this asserts through. It is also the detector: it swallows io.EOF,
		// so a truncating wrapper arrives here as `err == nil` and a short body.
		body, readErr := io.ReadAll(resp.Body)

		require.Error(t, readErr,
			"io.ReadAll returned no error after reading %d bytes through a %d-byte cap. Either nothing is "+
				"enforcing the cap, or the cap ends the stream cleanly — and a clean end is the worse of the "+
				"two: the caller believes it holds the whole body", len(body), int64(maxResponseBytes))
		assert.NotErrorIs(t, readErr, io.EOF,
			"the cap reported itself as io.EOF (possibly wrapped), which every reader in the standard library "+
				"treats as the clean end of the body rather than as a failure. The error must be distinguishable "+
				"from a complete response; got: %v", readErr)
		assert.LessOrEqual(t, int64(len(body)), int64(maxResponseBytes),
			"the caller obtained %d bytes through a %d-byte cap, so the limit is advisory rather than "+
				"enforced — a remote host still decides how much memory this process allocates",
			len(body), int64(maxResponseBytes))
	})
}
