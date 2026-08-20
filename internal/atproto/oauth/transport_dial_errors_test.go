package oauth

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeTransport_DialErrorAccountsForEveryVettedAddress pins that a
// request which tried several addresses and failed on all of them says so.
//
// # WHAT AN OPERATOR USED TO LOSE (fixed by the change this test drove)
//
// The dial loop kept only a `lastErr`, so every failure but the final one was
// discarded. A hostname with an A and an AAAA record is the ordinary case, not a
// corner one, and the two commonly fail differently — IPv6 unreachable on a
// v4-only host, connection refused on the v4 address. What reaches the log is
// the second symptom alone, with nothing to say another address was tried at
// all. Someone debugging a federation failure then investigates one address
// while the request also failed against another they cannot see.
//
// # A SECOND, WEAKER REASON, STATED CAREFULLY
//
// `return nil, lastErr` at the end of the loop is only guaranteed non-nil
// because the `len(vetted) == 0` guard above it refuses the empty case first.
// That is a latent fragility in the loop's contract, NOT a live bug: an empty
// vetted slice cannot reach the loop through the public API today, and nothing
// currently returns `(nil, nil)` from the dial. It is worth fixing while this
// code is open because the guard and the loop are separated by twenty lines, and
// an edit to either one that forgets the other would silently produce a dial
// result net/http does not tolerate. Do not read this paragraph as a claim that
// the current code can panic — it cannot.
//
// # WHY NAMING ADDRESSES HERE IS NOT THE ORACLE CYCLE 1 REMOVED
//
// The refusal error deliberately does NOT name the resolved IP, because a
// classification refusal is reachable by a stranger who picked the hostname and
// would learn what it resolved to inside our network. This error is a different
// animal: it reports that addresses we already accepted could not be connected
// to. The dial-path errors at transport.go:382 and :387 name `addr` for the same
// reason and cycle 1 left them untouched on purpose — see the scope note there
// and the assertion at transport_revetting_test.go:252.
func TestSSRFSafeTransport_DialErrorAccountsForEveryVettedAddress(t *testing.T) {
	t.Parallel()

	// A port this test owned and then released, rather than a number written
	// down: nothing is listening on it, and it cannot collide with a service a
	// developer happens to be running. Both dials below therefore fail, which is
	// the only thing the assertions may depend on.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "binding a throwaway listener to claim a port")
	_, port, err := net.SplitHostPort(probe.Addr().String())
	require.NoError(t, err, "splitting the throwaway listener address %q", probe.Addr())
	require.NoError(t, probe.Close(), "releasing the claimed port so nothing is listening on it")

	// Both loopback, in the two families, which is what makes them fail for
	// PLATFORM-DEPENDENT reasons — refused where the family is available,
	// unreachable where it is not. That is deliberate: the assertions below name
	// the addresses and never the operating system's wording, because the wording
	// differs across platforms and the addresses do not.
	//
	// 127.0.0.2 was considered and rejected for the same reason in reverse: Linux
	// has all of 127.0.0.0/8 on lo where macOS does not bind 127.0.0.2, so the
	// two platforms differ in how long the attempt takes as well as what it says.
	first := net.ParseIP("127.0.0.1")
	second := net.ParseIP("::1")
	require.NotNil(t, first, "the test's own first address must parse")
	require.NotNil(t, second, "the test's own second address must parse")

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{
		"multi.test": {first, second},
	}}

	// The hatch is OPEN because both addresses are loopback and classification is
	// not what is under test here — the dial loop is. And unlike most tests in
	// this package, transport.base is deliberately NOT substituted: the loop
	// lives inside the base transport built by the constructor, so replacing it
	// would replace the unit under test with the test's own fake.
	client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://" + net.JoinHostPort("multi.test", port) + "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err,
		"nothing is listening on the claimed port, so a request to it must fail against both addresses")

	assert.Contains(t, err.Error(), first.String(),
		"the error does not mention %s, the FIRST address the dial loop tried. Its failure was overwritten by "+
			"the next attempt's, so an operator reading this sees one symptom and cannot tell that another "+
			"address was tried at all — which is the ordinary case for any host with both an A and an AAAA "+
			"record; got: %v", first, err)

	assert.Contains(t, err.Error(), second.String(),
		"the error does not mention %s, the LAST address the dial loop tried. An aggregate that drops the "+
			"final attempt has traded one blind spot for another; got: %v", second, err)
}
