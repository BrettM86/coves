package oauth

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeHTTPClient_RefusalIsTypedAndDoesNotDiscloseTheResolvedAddress
// pins the vocabulary of a refusal: matchable by type, detailed for the
// operator, silent about the address to everyone else.
//
// # WHY THE RESOLVED IP MUST LEAVE THE MESSAGE
//
// Before this change the refusal rendered as "SSRF blocked: %s resolves to
// private IP %s" — this test is what removed the second verb, so the sentence
// below describes what WAS wrong, not what the code does now.
// The second verb is an internal-network oracle. The host in that sentence is
// the attacker's own input — a DID document's serviceEndpoint, a community
// record's domain, a name whose zone they control — so they can ask about any
// name they like and read back which address it resolved to INSIDE our network.
// That turns a refusal into a mapping primitive: point a name at a candidate,
// read the answer out of the error, repeat. Error strings travel further than
// the code that writes them — into an HTTP response body, a shared log, a
// support ticket — and every one of the nine fetch sites about to use this
// client formats errors somewhere.
//
// # WHY errors.Is RATHER THAN A SUBSTRING
//
// The callers being wired up need to tell "the guard refused this" from "the
// network failed", because the two get different HTTP statuses and different
// log levels. Substring matching on a message makes the message an API: it
// cannot be reworded, and a caller that misspells the substring fails open with
// no signal. A sentinel is checkable by the compiler's users and reworded
// freely.
//
// # WHAT IS DELIBERATELY NOT ASSERTED
//
// That the HOSTNAME is absent. http.Client.Do wraps every RoundTrip error in a
// *url.Error whose Error() embeds the full request URL, so the host appears in
// what the caller sees no matter what this transport returns — an assertion to
// the contrary would be unsatisfiable, and an unsatisfiable assertion is one
// somebody eventually weakens. It also protects nothing: the host is the
// attacker's own input, and they already know what they sent. The resolved
// address is the only half of that sentence they did not supply, which is
// exactly why it is the only half that has to go.
func TestSSRFSafeHTTPClient_RefusalIsTypedAndDoesNotDiscloseTheResolvedAddress(t *testing.T) {
	t.Parallel()

	const blockedHost = "blocked.test"

	// Checked, not assumed: isPrivateIP(nil) returns false, so a typo'd literal
	// here would classify as public, the request would never be refused, and
	// this test would fail for a reason that has nothing to do with its subject.
	resolved := net.ParseIP("10.99.13.37")
	require.NotNil(t, resolved, "the test's own private address must parse")

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{blockedHost: {resolved}}}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://" + blockedHost + "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "a hostname resolving to a private address must be refused")

	// Matchable by identity. Non-fatal so the message assertions below still run
	// and one failure reports the whole gap rather than the first step of it.
	assert.ErrorIs(t, err, ErrBlockedAddress,
		"the refusal does not match ErrBlockedAddress. The nine callers being wired onto this client have to "+
			"separate a guard refusal from a network failure to choose a status code and a log level, and "+
			"substring matching on a message makes the message an API that cannot be reworded and fails open "+
			"when it is misspelled; got: %v", err)

	assert.Contains(t, err.Error(), "SSRF blocked",
		"the refusal must keep the prefix that transport_unspecified_address_test.go:78 and the outer "+
			"acceptance contract both assert on, so a transport error cannot be mistaken for a block; got: %v", err)

	assert.NotContains(t, err.Error(), resolved.String(),
		"the rendered refusal discloses %s, the address the attacker's own hostname resolved to inside our "+
			"network. They chose the name and can point it anywhere, so an error that answers 'and what did "+
			"that resolve to' is a mapping primitive rather than a diagnostic; got: %v", resolved, err)

	// The detail is not deleted, it is relocated: an operator debugging a block
	// still needs to know which address caused it, and errors.As is where that
	// now lives. Fatal, because the field assertions below would nil-deref.
	var blocked *BlockedAddressError
	require.ErrorAs(t, err, &blocked,
		"the refusal must carry its detail on a typed error reachable with errors.As. Moving the address off "+
			"the message only works if it stays recoverable somewhere — otherwise this is a genericisation that "+
			"costs the operator the diagnostic; got: %v", err)

	assert.Equal(t, blockedHost, blocked.Host,
		"the typed error must name the host that was refused")
	assert.True(t, blocked.IP.Equal(resolved),
		"the typed error names %s as the blocking address, but the answer that caused the refusal was %s",
		blocked.IP, resolved)
}

// TestBlockedAddressError_MarshalsWithoutTheResolvedAddress closes the second
// route out of this type.
//
// Error() was rewritten to drop the resolved address because error strings
// travel — into HTTP response bodies, shared logs and support tickets. Every one
// of those destinations is also somewhere errors get MARSHALLED rather than
// formatted: a structured logger handed the error value, an API that renders a
// failure as JSON, a handler that dumps a request context. IP is an exported
// net.IP, and net.IP marshals to its own textual form, so `json.Marshal(err)`
// puts back exactly the oracle Error() removed — with the attacker-chosen host
// beside it, which is the whole mapping primitive in one object.
//
// # WHAT THIS DOES AND DOES NOT COVER
//
// It covers encoding/json, which is the reflective renderer this tree actually
// reaches for. It does NOT cover %#v, %+v on a struct-formatting verb, or any
// other reflection-based dumper: those read the field regardless of tags, and
// shutting them out needs the field unexported behind an accessor. That is a
// larger change and is deliberately not made here — so this assertion is scoped
// to what it can honestly claim.
func TestBlockedAddressError_MarshalsWithoutTheResolvedAddress(t *testing.T) {
	t.Parallel()

	resolved := net.ParseIP("10.99.13.37")
	require.NotNil(t, resolved, "the test's own private address must parse")

	blocked := &BlockedAddressError{Host: "blocked.test", IP: resolved}

	encoded, err := json.Marshal(blocked)
	require.NoError(t, err, "the typed refusal must stay marshallable; got: %v", err)

	assert.NotContains(t, string(encoded), resolved.String(),
		"json.Marshal of the refusal renders %s, the address the attacker's own hostname resolved to inside "+
			"our network. Error() drops that address precisely because a refusal must not answer 'and what did "+
			"that resolve to' — a struct tag is all that stands between the two renderings, and a structured "+
			"logger handed the error value takes the marshalling one; got: %s", resolved, encoded)

	// The relocation is what makes the omission acceptable: errors.As still
	// reaches the address, so the operator keeps the diagnostic.
	assert.True(t, blocked.IP.Equal(resolved),
		"the field itself must keep the address — hiding it from the renderer is not the same as deleting the "+
			"diagnostic errors.As exists to deliver")
}

// TestSSRFSafeHTTPClient_ResolutionFailureIsNotABlockedAddress is the fence
// around the sentinel: it must mean "the guard refused this address", not "this
// request failed".
//
// A DNS failure and a refusal need opposite handling — one is retryable and
// unremarkable, the other is a security event worth logging loudly — so the
// cheapest wrong implementation of the cycle above, wrapping every RoundTrip
// error in ErrBlockedAddress, would give every caller a sentinel that says
// nothing. This passes today and must still pass afterwards.
func TestSSRFSafeHTTPClient_ResolutionFailureIsNotABlockedAddress(t *testing.T) {
	t.Parallel()

	// An empty answer table, so hostRoutedResolver reports the host as not
	// found: a resolution failure, not a classification.
	resolver := &hostRoutedResolver{answers: map[string][]net.IP{}}

	client := NewSSRFSafeHTTPClient()
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://unresolvable.test/")
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "a hostname that does not resolve must still fail the request")

	assert.NotErrorIs(t, err, ErrBlockedAddress,
		"a name that failed to resolve reports itself as a blocked address. The sentinel then means 'something "+
			"went wrong' rather than 'the guard refused a destination', and a caller using it to decide between "+
			"a retry and a security log gets the wrong answer for every DNS hiccup; got: %v", err)
}
