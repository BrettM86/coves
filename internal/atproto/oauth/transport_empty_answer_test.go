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

// TestSSRFSafeHTTPClient_RefusesAnEmptyLookupAnswer covers RoundTrip's
// `len(ips) == 0` guard, which had none.
//
// # WHY A GUARD WITH NO COVERAGE MATTERS HERE MORE THAN USUAL
//
// It is not an isolated branch. The dial loop twenty lines further down
// documents THIS guard as the reason its own fail-closed check is latent
// ("an empty vetted slice cannot reach here through the public API, because the
// guard above refuses it"). So the two are coupled: delete this guard and an
// empty answer reaches a dial with nothing vetted, where errors.Join of nothing
// would be nil — a connection that does not exist and no error explaining it,
// which is not a result net/http is written to survive. Two branches, one of
// them dead by assumption, and until this test neither had a line of coverage.
//
// # WHERE AN EMPTY, NON-ERROR ANSWER COMES FROM
//
// It is not hypothetical. A resolver answering NOERROR with no records — a
// name with only CNAME or TXT data, a split-horizon server, a filtering
// resolver stripping AAAA — returns success and nothing. net.LookupIPAddr
// normally converts that to an error, but this transport's lookup is a FIELD:
// dev_resolver.go substitutes it, tests substitute it, and any future
// substitution is one `return nil, nil` away from handing RoundTrip an empty
// slice.
//
// # WHAT IS ASSERTED, AND WHAT IS DELIBERATELY NOT
//
// That the request fails and NOTHING IS DIALLED. Not that it matches
// ErrBlockedAddress: an answer with no addresses is a resolution outcome, not a
// classification, and transport_blocked_error_test.go:113 pins the rule that a
// resolution failure must not report itself as a block. Deciding otherwise here
// would quietly widen the sentinel to mean "something went wrong".
func TestSSRFSafeHTTPClient_RefusesAnEmptyLookupAnswer(t *testing.T) {
	t.Parallel()

	const host = "answers-nothing.test"

	// A NON-NIL, EMPTY slice with a NIL error: success, with no addresses. A nil
	// slice would be the same to len() but this spelling says what is being
	// modelled — the resolver answered, and the answer was empty.
	resolver := &hostRoutedResolver{answers: map[string][]net.IP{host: {}}}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	// The base transport is substituted by a recording dialler, and that choice
	// is what makes this test discriminating. The constructor's own base
	// contains the fail-closed guard, so leaving it in place would mean an empty
	// answer is refused there instead — and the test would pass with RoundTrip's
	// guard deleted. Replacing it removes the second net, so `dialled` reports
	// on RoundTrip alone.
	var dialled atomic.Bool
	transport.base = &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialled.Store(true)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
		},
	}

	resp, err := client.Get("http://" + host + "/")
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err,
		"a lookup that answered with no addresses must fail the request. There is nothing to connect to and "+
			"nothing to classify, so continuing means dialling a destination the guard never saw")

	assert.False(t, dialled.Load(),
		"the transport dialled after a lookup returned no addresses. Nothing was vetted, so whatever the "+
			"dialler connected to was chosen by the address string rather than by the guard — which is the "+
			"check-then-dial window this design closes")

	assert.NotErrorIs(t, err, ErrBlockedAddress,
		"an empty lookup answer reported itself as a blocked address. It is a resolution outcome, not a "+
			"classification: transport_blocked_error_test.go pins that the sentinel must mean 'the guard "+
			"refused a destination' and not 'the request failed'; got: %v", err)
}
