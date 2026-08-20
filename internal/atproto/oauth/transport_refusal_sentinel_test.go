package oauth

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ErrBlockedAddress says of itself that it is "the sentinel every address
// refusal matches". Three refusals in the dial path say "SSRF blocked" in their
// message and match nothing.
//
// # WHY A MESSAGE THAT SAYS "SSRF BLOCKED" IS NOT ENOUGH
//
// The sentinel exists precisely so callers stop matching on the message —
// transport_blocked_error_test.go:28 sets out the argument at length, and the
// nine fetch sites being wired onto this client are supposed to use errors.Is
// to choose between a retry and a security log. A refusal that renders the
// right words and fails errors.Is gives those callers the WORST of both: the
// string looks like a block to a human reading a log, and classifies as an
// ordinary network failure to the code deciding what to do about it.
//
// # THE ONE THAT MATTERS MOST
//
// The fail-closed refusal is the guard announcing that it was BYPASSED —
// something reached the base transport without going through RoundTrip, on a
// code path that would otherwise dial any address a caller names. A caller
// doing `if errors.Is(err, ErrBlockedAddress) { alert() } else { retry() }`
// currently retries it. Retrying a bypass is retrying the thing the wrapper
// exists to prevent.
//
// # WHY THIS DRIVES THE BASE TRANSPORT DIRECTLY
//
// Both refusals below are unreachable through client.Get by construction:
// going through the front door is what populates the vetted-address context
// value and what guarantees the dial address is well formed.
// TestSSRFSafeTransport_BaseTransportFailsClosedWhenBypassed established the
// pattern and owns the message assertions; this test owns the identity.
//
// # THE THIRD REFUSAL IS NOT HERE, AND THAT IS A FINDING RATHER THAN AN OMISSION
//
// The dial loop's "no vetted address was attempted for %s" carries the same
// defect and CANNOT be reached from a test. It fires only when the loop body
// never runs, which needs an empty vetted slice, which the guard exercised
// below refuses twenty lines earlier — transport.go says so itself ("THIS IS
// LATENT, NOT LIVE"). There is no seam to drive it through, so an honest test
// cannot exist for it until one is introduced. It must still be fixed: the
// cheapest fix that covers all three is a single constructor for these refusals
// so the sentinel cannot be forgotten by the next one added.
func TestSSRFSafeTransport_EveryDialRefusalMatchesTheSentinel(t *testing.T) {
	t.Parallel()

	// A port claimed and released, so the address below is well formed and
	// nothing is listening on it. Written down as a literal it would be both a
	// guess and a test-audit violation.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "binding a throwaway listener to claim a port")
	addr := probe.Addr().String()
	require.NoError(t, probe.Close(), "releasing the claimed port so nothing is listening on it")

	loopback := net.ParseIP("127.0.0.1")
	require.NotNil(t, loopback, "the test's own address must parse")

	t.Run("the transport was bypassed", func(t *testing.T) {
		t.Parallel()

		client := NewSSRFSafeHTTPClient()
		transport, ok := client.Transport.(*ssrfSafeTransport)
		require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

		// No vetted addresses on the context: the signature of a caller that
		// reached the base transport without going through RoundTrip.
		conn, err := transport.base.DialContext(t.Context(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}

		require.Error(t, err, "the dial must refuse a destination nothing vetted")
		assert.ErrorIs(t, err, ErrBlockedAddress,
			"the guard announced its own bypass with an error that does not match ErrBlockedAddress. This is "+
				"the single most important refusal in the package to classify correctly — it means the wrapper "+
				"was skipped on a path that reaches any address a caller names — and a caller branching on "+
				"errors.Is treats it as an ordinary network failure and RETRIES it; got: %v", err)
	})

	t.Run("the dial address carries no port", func(t *testing.T) {
		t.Parallel()

		client := NewSSRFSafeHTTPClient()
		transport, ok := client.Transport.(*ssrfSafeTransport)
		require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

		// Vetted addresses present, so the refusal below can only come from the
		// address being unparseable — not from the fail-closed guard above it.
		ctx := context.WithValue(t.Context(), vettedAddrsKey, []net.IP{loopback})
		conn, err := transport.base.DialContext(ctx, "tcp", "a-destination-with-no-port")
		if conn != nil {
			_ = conn.Close()
		}

		require.Error(t, err, "the dial must refuse an address it cannot read a port from")
		assert.ErrorIs(t, err, ErrBlockedAddress,
			"a dial address the guard could not parse produced an error that does not match "+
				"ErrBlockedAddress, while rendering as 'SSRF blocked'. The two halves disagree, and code "+
				"reads the half that is wrong; got: %v", err)
	})

	// The fence, and it is the reason the fix has to be applied per refusal
	// rather than by wrapping everything the dial returns. An ordinary failed
	// connection is not a block: transport_blocked_error_test.go:113 pins the
	// same boundary on the resolution side, and this is its dial-side twin.
	t.Run("fence: a connection that simply failed is not a block", func(t *testing.T) {
		t.Parallel()

		client := NewSSRFSafeHTTPClient()
		transport, ok := client.Transport.(*ssrfSafeTransport)
		require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

		ctx := context.WithValue(t.Context(), vettedAddrsKey, []net.IP{loopback})
		conn, err := transport.base.DialContext(ctx, "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}

		require.Error(t, err, "nothing is listening on the claimed port, so the dial must fail")
		assert.NotErrorIs(t, err, ErrBlockedAddress,
			"a refused TCP connection to an address the guard ALREADY APPROVED reports itself as a blocked "+
				"address. The sentinel would then mean 'the request failed' rather than 'the guard refused a "+
				"destination', which is exactly the genericisation transport.go's doc comment warns against; "+
				"got: %v", err)
	})
}
