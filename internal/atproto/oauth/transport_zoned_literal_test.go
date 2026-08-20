package oauth

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSRFSafeHTTPClient_RefusesAZonedIPv6LiteralBeforeResolving closes the one
// spelling of an IP literal that walks past the literal check.
//
// # THE HOLE
//
// The check is `net.ParseIP(req.URL.Hostname())`, and net.ParseIP returns nil
// for any address carrying a ZONE — the `%eth0` in `fe80::1%eth0`, which names
// the interface the address is scoped to. url.Parse does understand the form:
// `http://[2600::1%25eth0]/` yields a Hostname of `2600::1%eth0`
// (verified by probe, along with ParseIP returning nil for it). So the address
// is refused as "not a literal", handed to the resolver, resolved locally with
// no DNS involved, its zone silently discarded, and dialled.
//
// # WHAT THIS IS AND IS NOT
//
// It is a hole in the CONTROL, not a live SSRF: an address that would be
// dangerous once the zone is stripped — fe80::1, ::1 — is still caught by
// classification a few lines later, and transport_ip_literal_test.go covers
// those. What gets through the literal check untouched is a PUBLIC-classified
// zoned address, and the literal check exists precisely because classification
// is not the answer for literals: a caller-supplied literal naming a public
// address is still a destination this AppView has no business reaching, and
// that is the case this row demonstrates.
//
// The private row is here too, and it is not redundant: it pins WHERE the
// refusal happens. Today it is refused after a lookup; it must be refused
// before one, for the same reason every other literal is — resolving an address
// asks a question the URL already answered, of a resolver an attacker may
// influence.
//
// # THE FIX
//
// netip.ParseAddr, which accepts zones. The assertion below is deliberately
// about behaviour rather than about which parser is used, but the parser is
// worth naming because there is no way to reach the same result with
// net.ParseIP short of hand-splitting on '%'.
func TestSSRFSafeHTTPClient_RefusesAZonedIPv6LiteralBeforeResolving(t *testing.T) {
	t.Parallel()

	// The premise, checked rather than assumed. If netip ever stopped accepting
	// zones the fix this test asks for would not exist, and the test would be
	// demanding something impossible instead of something undone.
	zoned, err := netip.ParseAddr("2600::1%eth0")
	require.NoError(t, err, "netip.ParseAddr must accept a zoned address: it is the parser this fix needs")
	require.Equal(t, "eth0", zoned.Zone(), "the parsed address must carry its zone")
	require.Nil(t, net.ParseIP("2600::1%eth0"),
		"net.ParseIP must still return nil for a zoned address — that nil is the whole defect, and if it "+
			"ever stops being nil this test is pinning a bug that no longer exists")

	tests := []struct {
		name string
		// url carries the RFC 6874 spelling: the zone's '%' is percent-encoded
		// as %25 inside the brackets, which is what a URL parser requires and
		// what an attacker would send.
		url string
		why string
	}{
		{
			name: "a zoned address that classifies as public",
			url:  "http://[2600::1%25eth0]/",
			why: "globally reachable space, which this package classifies as public — so classification cannot " +
				"refuse it and only the literal check can. This is the row where the hole has consequences",
		},
		{
			name: "a zoned link-local address",
			url:  "http://[fe80::1%25en0]/",
			why: "link-local, so classification refuses it today — but only AFTER handing an attacker-supplied " +
				"address to a resolver, which is the step the literal check exists to skip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// An empty answer table: nothing here is resolvable, so a request
			// that gets past the literal check dies at resolution rather than
			// reaching a socket. The assertion is on what the resolver was
			// ASKED, not on whether the request failed.
			resolver := &hostRoutedResolver{answers: map[string][]net.IP{}}

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = resolver.lookup

			// Belt and braces: a dialler that records and refuses, so nothing
			// can leave the machine even if both guards were removed.
			var dialled atomic.Bool
			transport.base = &http.Transport{
				DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
					dialled.Store(true)
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
				},
			}

			resp, err := client.Get(tt.url)
			if err == nil {
				_ = resp.Body.Close()
			}

			// THE DISCRIMINATOR. Everything else here passes today.
			assert.Emptyf(t, resolver.hostsAsked(),
				"the transport sent %v to the resolver. %s is an address written where a hostname belongs, "+
					"and net.ParseIP returning nil for it because of the zone does not make it a name — %s",
				resolver.hostsAsked(), tt.url, tt.why)

			require.Error(t, err, "GET %s must be refused", tt.url)
			assert.ErrorIsf(t, err, ErrBlockedAddress,
				"the refusal must be the literal check's, matchable by identity. A resolution failure that "+
					"happens to stop the request is not the same control and does not hold when the name is "+
					"one the resolver can answer; got: %v", err)

			assert.Falsef(t, dialled.Load(),
				"GET %s reached the dialler. A zoned literal is still a literal, and the destination was "+
					"chosen by whoever supplied the URL", tt.url)
		})
	}
}

// The hatch, and the reason it needs its own row: every integration fixture in
// this tree is served from a loopback listener, so a literal check that ignores
// allowPrivate is an outage in dev and in the test suite. A zoned address is
// the one spelling an operator would legitimately reach for with the hatch open
// — fe80::1%en0 needs its interface to be reachable at all — and it must not
// become the one spelling the hatch does not cover.
//
// Asserted at the RESOLVER rather than through a completed request: the zone is
// discarded by resolveHost today (transport.go documents this), so a real dial
// to a link-local address would not work anyway. What must hold is that the
// request is not refused OUT OF HAND when the hatch is open.
func TestSSRFSafeHTTPClient_TheHatchStillAdmitsAZonedLiteral(t *testing.T) {
	t.Parallel()

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{}}

	client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	resp, err := client.Get("http://[fe80::1%25en0]/")
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "the empty resolver answers nothing, so the request fails — but at resolution")
	assert.NotErrorIs(t, err, ErrBlockedAddress,
		"the zoned literal was refused as a blocked address with the hatch OPEN. allowPrivate means 'this "+
			"client is pointed at a developer-chosen address', and a zoned link-local address is the case "+
			"that most needs it; got: %v", err)
	assert.Equal(t, []string{"fe80::1%en0"}, resolver.hostsAsked(),
		"with the hatch open the host must reach the resolver exactly as every other literal does")
}
