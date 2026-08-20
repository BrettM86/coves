package oauth

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeTransport_ASingleFailedAddressKeepsItsTimeoutSignal pins that
// aggregating one error is not aggregating at all.
//
// # WHAT errors.Join COSTS HERE
//
// url.Error.Timeout — the method every caller of this client reaches, because
// http.Client wraps every RoundTrip error in a *url.Error — is implemented as a
// DIRECT TYPE ASSERTION, not errors.As:
//
//	func (e *Error) Timeout() bool {
//	    t, ok := e.Err.(interface{ Timeout() bool })
//	    return ok && t.Timeout()
//	}
//
// net.Error is discovered the same way at every call site that matters:
// `if ne, ok := err.(net.Error); ok && ne.Timeout()`. errors.Join returns a
// *errors.joinError, which implements Unwrap() []error and nothing else — so
// wrapping a single *net.OpError in one severs Timeout() even though the
// information is still in the chain. Verified: the joined error does not
// implement the interface at all.
//
// # WHY THE SINGLE-ADDRESS CASE IS THE ONE TO FIX
//
// Aggregation exists so an operator debugging a host with both an A and an AAAA
// record can see both failures — that is
// TestSSRFSafeTransport_DialErrorAccountsForEveryVettedAddress, and it must go
// on passing. With ONE address there is nothing to aggregate: the join adds no
// information and costs the timeout signal, which is the signal retry and
// circuit-breaker logic is built on. A caller that cannot tell a timeout from a
// refusal either retries what it should not or gives up on what it should.
func TestSSRFSafeTransport_ASingleFailedAddressKeepsItsTimeoutSignal(t *testing.T) {
	t.Parallel()

	// Checked, not assumed.
	loopback := net.ParseIP("127.0.0.1")
	require.NotNil(t, loopback, "the test's own address must parse")

	// A port claimed and released: well formed, and nothing is listening.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "binding a throwaway listener to claim a port")
	addr := probe.Addr().String()
	require.NoError(t, probe.Close(), "releasing the claimed port so nothing is listening on it")

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

	// A deadline ALREADY IN THE PAST, which is how a timeout is produced without
	// waiting for one. net.Dialer checks the context before it does anything
	// else and returns a *net.OpError carrying os.ErrDeadlineExceeded, whose
	// Timeout() reports true — the same error shape a real dial timeout
	// produces, arrived at deterministically.
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
	defer cancel()

	// Exactly ONE vetted address. That is the whole premise: with one address
	// the join has nothing to combine.
	ctx = context.WithValue(ctx, vettedAddrsKey, []net.IP{loopback})

	conn, err := transport.base.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err, "a dial under an expired deadline must fail")

	// Asserted the way url.Error and every retry helper in the wild does it: a
	// direct type assertion, not errors.As. Using errors.As here would pass
	// against the broken implementation and prove nothing, because the
	// information IS still in the chain — what is lost is its reachability by
	// the mechanism callers actually use.
	timeouter, ok := err.(interface{ Timeout() bool })
	assert.Truef(t, ok,
		"the dial error is a %T, which does not implement Timeout() bool. url.Error.Timeout uses a direct "+
			"type assertion rather than errors.As, so every caller of this client — they all go through "+
			"http.Client, which wraps RoundTrip errors in *url.Error — now sees a timeout as an ordinary "+
			"failure. With a single vetted address the aggregation buys nothing and costs exactly this; got: %v",
		err, err)
	if ok {
		assert.True(t, timeouter.Timeout(),
			"the dial error implements Timeout() and reports false for a dial that expired against its "+
				"deadline; got: %v", err)
	}

	// The detail must survive as well as the signal: a fix that replaced the
	// join with a bare "dial failed" string would satisfy the assertion above
	// and lose what an operator reads.
	var opErr *net.OpError
	assert.ErrorAsf(t, err, &opErr,
		"the underlying *net.OpError is no longer reachable with errors.As, so the fix traded the timeout "+
			"signal for the diagnostic instead of keeping both; got: %v", err)
	assert.ErrorIsf(t, err, context.DeadlineExceeded,
		"the expired-deadline cause is no longer reachable in the error chain. net's timeout error reports "+
			"itself as context.DeadlineExceeded, and a fix that flattens the dial failures into a string "+
			"would lose that as well as Timeout(); got: %v", err)
}
