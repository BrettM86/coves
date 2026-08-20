package oauth

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeTransport_AnAllTimeoutAggregateStillReportsTimeout is the other
// half of TestSSRFSafeTransport_ASingleFailedAddressKeepsItsTimeoutSignal, and
// it covers the case that is COMMON rather than the case that is rare.
//
// # THE SINGLE-ADDRESS FIX LEFT THE ORDINARY HOST BROKEN
//
// Returning a lone dial error bare preserves Timeout() for a host with one
// address. An ordinary dual-stack host has two — an A record and an AAAA record
// — so it takes the errors.Join branch, and *errors.joinError implements
// Unwrap() []error AND NOTHING ELSE. url.Error.Timeout is a direct type
// assertion for `interface{ Timeout() bool }`, and every retry helper in the
// wild is `if ne, ok := err.(net.Error); ok && ne.Timeout()`. So a caller that
// retries on timeouts stops seeing them exactly when a host is dual-stack,
// which is the common case and not the corner one.
//
// # net.Error IS BOTH METHODS OR IT IS NOTHING
//
// Implementing Timeout() alone would satisfy url.Error and still fail every
// `err.(net.Error)` assertion, because net.Error requires Temporary() too. The
// aggregate therefore answers both, and answers them the only way an aggregate
// honestly can: true when EVERY joined error says true.
func TestSSRFSafeTransport_AnAllTimeoutAggregateStillReportsTimeout(t *testing.T) {
	t.Parallel()

	// Two loopback addresses, checked rather than assumed. Two is the premise:
	// with one the bare-return branch above already handles it.
	first := net.ParseIP("127.0.0.1")
	second := net.ParseIP("127.0.0.2")
	require.NotNil(t, first, "the test's own first address must parse")
	require.NotNil(t, second, "the test's own second address must parse")

	// A port claimed and released: well formed, and nothing is listening.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "binding a throwaway listener to claim a port")
	addr := probe.Addr().String()
	require.NoError(t, probe.Close(), "releasing the claimed port so nothing is listening on it")

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

	// A deadline ALREADY IN THE PAST makes both dials time out deterministically,
	// without waiting for a real timeout and without touching the network.
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
	defer cancel()
	ctx = context.WithValue(ctx, vettedAddrsKey, []net.IP{first, second})

	conn, err := transport.base.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err, "a dial under an expired deadline must fail on both addresses")

	// Asserted as url.Error does it — a direct type assertion, never errors.As —
	// because the information IS still in the chain and reachability by the
	// mechanism callers use is the entire property under test.
	timeouter, ok := err.(interface{ Timeout() bool })
	assert.Truef(t, ok,
		"the aggregated dial error is a %T, which does not implement Timeout() bool. A dual-stack host "+
			"(A + AAAA) takes the aggregation branch, so every caller of this client loses the timeout "+
			"signal on the COMMON shape of host while keeping it on the rare one; got: %v",
		err, err)
	if ok {
		assert.Truef(t, timeouter.Timeout(),
			"the aggregate implements Timeout() and reports false although EVERY address failed with a "+
				"deadline error. An aggregate over all-timeouts is a timeout; got: %v", err)
	}

	// net.Error is the interface retry and circuit-breaker logic asserts on, and
	// it is not satisfied by Timeout() alone.
	netErr, ok := err.(net.Error)
	assert.Truef(t, ok,
		"the aggregated dial error is a %T, which does not satisfy net.Error. `if ne, ok := err.(net.Error); "+
			"ok && ne.Timeout()` is the shape retry logic is written in, and it needs Temporary() as well as "+
			"Timeout(); got: %v", err, err)
	if ok {
		assert.Truef(t, netErr.Timeout(),
			"net.Error.Timeout() is false on an aggregate whose every member timed out; got: %v", err)
	}

	// Aggregation's own reason for existing must survive the fix: an operator
	// debugging a dual-stack host needs BOTH symptoms, so both errors stay
	// reachable through the tree.
	var opErr *net.OpError
	assert.ErrorAsf(t, err, &opErr,
		"the underlying *net.OpError is no longer reachable with errors.As, so the fix traded the "+
			"diagnostic for the signal instead of keeping both; got: %v", err)
	assert.ErrorIsf(t, err, context.DeadlineExceeded,
		"the expired-deadline cause is no longer reachable in the error chain; got: %v", err)

	unwrapped, ok := err.(interface{ Unwrap() []error })
	assert.Truef(t, ok,
		"the aggregate no longer implements Unwrap() []error, so errors.Is/As can no longer walk to the "+
			"per-address failures; got: %T", err)
	if ok {
		assert.Lenf(t, unwrapped.Unwrap(), 2,
			"the aggregate must account for EVERY vetted address that was attempted — two here — so an "+
				"operator sees both symptoms rather than whichever failed last; got: %v", err)
	}
}

// TestJoinDialErrors_ClassifiesTheAggregateFromItsMembers pins the classification
// rule itself, at the one place it can be stated without a network: an aggregate
// is a timeout only when EVERY member is one.
//
// # WHY "ALL" AND NOT "ANY"
//
// The caller's question is "should I retry this the way I retry a timeout?", and
// a host whose IPv6 address timed out while its IPv4 address was REFUSED has
// given a definite answer on one of them. Reporting that as a timeout tells a
// retry loop to keep waiting on a destination that already said no. "All" is the
// only reading under which the aggregate's answer is true of the whole thing it
// aggregates.
func TestJoinDialErrors_ClassifiesTheAggregateFromItsMembers(t *testing.T) {
	t.Parallel()

	plain := errors.New("connection refused")

	tests := []struct {
		name          string
		errs          []error
		wantTimeout   bool
		wantTemporary bool
	}{
		{
			name:          "every member timed out",
			errs:          []error{stubNetError{timeout: true}, stubNetError{timeout: true}},
			wantTimeout:   true,
			wantTemporary: false,
		},
		{
			name:          "one member did not time out",
			errs:          []error{stubNetError{timeout: true}, stubNetError{}},
			wantTimeout:   false,
			wantTemporary: false,
		},
		{
			name: "a member that is not a net.Error at all",
			// The honest answer is false: an aggregate cannot claim a property
			// of a member that cannot report it.
			errs:          []error{stubNetError{timeout: true}, plain},
			wantTimeout:   false,
			wantTemporary: false,
		},
		{
			name:          "every member is temporary",
			errs:          []error{stubNetError{temporary: true}, stubNetError{temporary: true}},
			wantTimeout:   false,
			wantTemporary: true,
		},
		{
			name: "a wrapped timeout still counts",
			// errors.As, not a direct assertion, INSIDE the aggregate: a
			// *net.OpError carrying a timeout is what the dialer actually
			// returns, and the members are ours to inspect properly even though
			// our callers cannot.
			errs: []error{
				&net.OpError{Op: "dial", Err: stubNetError{timeout: true}},
				stubNetError{timeout: true},
			},
			wantTimeout:   true,
			wantTemporary: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			joined := joinDialErrors(tt.errs)
			require.Error(t, joined, "joining %d errors must produce one", len(tt.errs))

			netErr, ok := joined.(net.Error)
			require.Truef(t, ok,
				"the aggregate is a %T and does not satisfy net.Error; retry logic discovers timeouts "+
					"through that interface", joined)

			assert.Equalf(t, tt.wantTimeout, netErr.Timeout(),
				"Timeout() on an aggregate of %v", tt.errs)
			assert.Equalf(t, tt.wantTemporary, netErr.Temporary(),
				"Temporary() on an aggregate of %v", tt.errs)

			// Every member stays reachable however the aggregate classifies.
			for _, member := range tt.errs {
				assert.ErrorIsf(t, joined, member,
					"member %v is no longer reachable through the aggregate", member)
			}
		})
	}
}

// TestJoinDialErrors_ReturnsALoneErrorBare re-states the single-address contract
// at the function that now owns it, so a refactor of the dial loop cannot lose
// it without a unit test failing. The end-to-end proof stays in
// TestSSRFSafeTransport_ASingleFailedAddressKeepsItsTimeoutSignal.
func TestJoinDialErrors_ReturnsALoneErrorBare(t *testing.T) {
	t.Parallel()

	only := stubNetError{timeout: true}
	joined := joinDialErrors([]error{only})

	assert.Equal(t, error(only), joined,
		"a single dial failure must be returned EXACTLY as it arrived. Wrapping it is aggregation with "+
			"nothing to aggregate, and any wrapper is one more thing between a caller and the concrete "+
			"error type it may be matching on")
}

// stubNetError is a net.Error whose two answers are set by the test.
type stubNetError struct {
	timeout   bool
	temporary bool
}

func (e stubNetError) Error() string   { return "stub net error" }
func (e stubNetError) Timeout() bool   { return e.timeout }
func (e stubNetError) Temporary() bool { return e.temporary }
