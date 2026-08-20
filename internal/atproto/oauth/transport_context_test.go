package oauth

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeHTTPClient_ResolutionHonoursTheRequestContext pins that a
// cancelled request stops the name lookup instead of running it to the
// resolver's own timeout.
//
// # WHAT WAS BROKEN (fixed by the change this test drove)
//
// resolveHost used to call net.LookupIP, which takes no context. So a caller that
// cancels — a user who closed the connection, a handler whose deadline expired,
// a shutdown — releases nothing: the lookup keeps a goroutine and a socket alive
// until the resolver's own unbounded timeout, and every one of the nine fetch
// sites about to use this client sits behind a request-scoped context that means
// nothing to it. CLAUDE.md names a missing context.Context as a red flag for
// exactly this reason.
//
// # WHY THIS DRIVES THE DEFAULT PATH AND NOT THE lookupIP SEAM
//
// The seam is the wrong instrument here, and the distinction matters enough to
// write down. RoundTrip handing req.Context() to resolveHost is PLUMBING —
// whether a fake resolver receives a cancelled context is decided by that one
// argument, so a test built on the seam measures the wiring rather than the
// behavior, and it would pass the moment the signature changed even with
// net.LookupIP still underneath. Driving the real path is what distinguishes
// "the context arrives" from "the context is obeyed", and only the second is the
// property.
//
// # AND WHY THE HOST IS A NAME RATHER THAN AN IP LITERAL
//
// A dotted quad would make this test assert nothing at all. Both the old and the
// new resolver short-circuit a literal and return it BEFORE consulting the
// context, so a cancelled-context test pointed at 127.0.0.1 succeeds either way.
// The host has to be one that reaches the resolver proper.
//
// `cancelled.invalid` is that host, and .invalid is chosen over the .test names
// used elsewhere in this package deliberately: RFC 6761 §6.4 guarantees it is
// never resolvable, where a developer's local dnsmasq may well map *.test to
// loopback. Note what the assertions do NOT depend on, though — once the fix
// lands, a cancelled context means NO DNS QUERY IS MADE AT ALL, so the green
// path touches no resolver. Only the red path performs a lookup, and it fails
// this test on every possible answer to it (NXDOMAIN, timeout, or even a
// successful one), because none of them is a cancellation.
func TestSSRFSafeHTTPClient_ResolutionHonoursTheRequestContext(t *testing.T) {
	t.Parallel()

	// No seam installed. transport.lookupIP stays nil so resolveHost takes its
	// production path, which is the thing under test.
	client := NewSSRFSafeHTTPClient()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cancelled.invalid/", nil)
	require.NoError(t, err, "building the request")

	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "a request whose context is already cancelled must fail")

	// The anti-vacuity assertion, and it has to come first because it is what
	// makes the next one mean something. "failed to resolve host" is RoundTrip's
	// own wrapper, so its presence proves the failure came from THIS transport's
	// resolution step rather than from http.Client noticing the dead context and
	// short-circuiting before RoundTrip ever ran. Without it, a client-level
	// short-circuit would satisfy the cancellation assertion below while the
	// lookup stayed exactly as context-blind as it is today.
	assert.Contains(t, err.Error(), "failed to resolve host",
		"the failure did not come from the transport's own resolution step, so this test cannot say anything "+
			"about whether that step honours the context; got: %v", err)

	assert.ErrorIs(t, err, context.Canceled,
		"the resolution of a cancelled request did not report the cancellation — it ran the lookup anyway and "+
			"failed for some unrelated reason. net.LookupIP takes no context, so cancelling a request releases "+
			"nothing and the lookup holds its goroutine until the resolver's own timeout; "+
			"net.DefaultResolver.LookupIPAddr(ctx, host) is the drop-in that observes it. Got: %v", err)
}
