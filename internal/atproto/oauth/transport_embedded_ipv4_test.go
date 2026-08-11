package oauth

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsPrivateIP_IPv6FormsEmbeddingIPv4 pins the IPv6 addresses that carry an
// IPv4 address inside them.
//
// # WHY THIS IS A DIFFERENT MECHANISM, NOT MORE RANGES
//
// The reserved-range checks ask "which block is this address in". These forms
// defeat that question rather than answering it wrongly: the address the packet
// ends up at is not the address being classified, it is a four-byte field
// EMBEDDED in it. A classifier that never decodes the payload cannot tell a
// loopback from a public DNS server, because the two differ only in bytes no CIDR
// over the IPv6 space separates.
//
// # 6to4 IS THE EXCEPTION, AND THE REASON IS NOT "IT WAS TOO HARD"
//
// The rule for every other form here is decode the payload, classify the payload.
// 6to4 is banned wholesale instead, and `2002:808:808::` — 6to4 carrying the
// public 8.8.8.8 — is a BLOCKED row rather than an allowed one. This file
// asserted the opposite when it was first written. **The reversal is a
// requirements change, not a test relaxed to let an implementation pass**, and
// the argument is worth having in front of you before you change it back.
//
// 6to4's embedded IPv4 is not the destination. It is the TUNNEL ENDPOINT:
// `2002:V4ADDR:SLA:iface` names a gateway in bytes 2..6 and then a subnet and a
// host BEHIND that gateway in the remaining ten. So "the embedded v4 is public,
// therefore this address is safe" does not follow — a 6to4 address whose gateway
// is public can still name an internal IPv6 service on the far side of it, and
// decoding tells you only who the tunnel belongs to. The decode answers a
// different question from the one asked.
//
// The decision is to ban `2002::/16` wholesale and stop decoding 6to4 at all.
// The soundness argument above is the reason; what makes it cheap is that
// unicast 6to4 is effectively dead traffic. RFC 7526 §4 requires that "in host
// implementations, unicast 6to4 MUST also be disabled by default"; `2002::/16`
// sits on the standard bogon lists (Team Cymru, NLNOG); 6to4 and Teredo together
// round to 0.00% of Google's measured IPv6 traffic; and a `2002::`-only server is
// unreachable in practice anyway, because the return-relay ecosystem it depended
// on is gone. The operator has confirmed nothing in this infrastructure uses it.
//
// Be careful with the citation, because the obvious shorthand is wrong: RFC 7526
// does NOT deprecate 6to4. It deprecates the ANYCAST RELAY prefix 192.88.99.0/24,
// and says of the rest, in terms — "The basic unicast 6to4 mechanism defined in
// [RFC3056] and the associated 6to4 IPv6 prefix 2002::/16 are not deprecated."
// The ban here is our policy choice, justified by the gateway-field argument and
// made cheap by the traffic numbers; it is not something an RFC did for us.
//
// # NAT64 KEEPS ITS DECODE, AND THAT IS NOT A NICETY
//
// The temptation for the next reader is to make NAT64 symmetric with the 6to4
// ban. That would be an outage. `64:ff9b::/96` is a legitimate connect()
// destination: an IPv6-only host with DNS64 reaches every IPv4-only server in the
// world through it, so on an IPv6-only deployment a wholesale ban does not block
// some outbound federation, it blocks ALL of it. `64:ff9b:1::/48` (RFC 8215) is
// the same mechanism with a locally-chosen prefix and carries the same
// consequence.
//
// The asymmetry, stated plainly, is the thing to remember about this file:
// **6to4 embeds a gateway, so we ban it. NAT64 embeds the destination, so we
// decode it.** Same-shaped encoding, different semantics, different answer.
//
// # WHAT THE ALLOWED ROWS ARE FOR
//
// Every remaining allowed row embeds the public 8.8.8.8 under a prefix that is
// still decoded. They are what stops the 6to4 decision from being generalised
// into "ban every prefix": a wholesale ban of NAT64 or SIIT turns all their
// blocked rows green and these red. The blocked rows prove the extraction
// happens; the allowed rows prove it is a decode and not a ban.
//
// # DELIBERATE EXCLUSIONS
//
// Teredo is NOT handled here, and if anyone adds it, the prefix is
// `2001:0000::/32` — **never** `2001::/16`. That is not a style preference. The
// atProto ecosystem has live production PDSes at `2001:19f0:7002:191::` (socl.is)
// and `2001:550:5a00:785b::1` (pds.zzls.xyz), and a /16 rule blocks both:
// verified, `net.ParseCIDR("2001::/16")` contains each of them while
// `2001:0000::/32` contains neither. Both addresses are pinned as allowed rows
// below so the mistake fails a test rather than federation.
func TestIsPrivateIP_IPv6FormsEmbeddingIPv4(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// 6to4, 2002::/16 — banned wholesale, payload irrelevant. The public-payload
		// row is the one that states the ban; without it, a decode-and-recheck
		// implementation still passes the other three.
		{"6to4 embedding loopback", "2002:7f00:1::", true},
		{"6to4 embedding RFC1918 10/8", "2002:a00:1::", true},
		{"6to4 embedding RFC1918 192.168/16", "2002:c0a8:1::", true},
		{"6to4 with a public tunnel endpoint", "2002:808:808::", true},

		// The prefix ban must be exactly /16 and not wider.
		{"Just above 6to4", "2003::1", false},

		// NAT64 well-known prefix, 64:ff9b::/96 — the IPv4 is the last 4 bytes and
		// it is the real destination, so this one is decoded.
		{"NAT64 well-known embedding loopback", "64:ff9b::7f00:1", true},
		{"NAT64 well-known embedding RFC1918 10/8", "64:ff9b::a00:1", true},
		{"NAT64 well-known embedding a public address", "64:ff9b::808:808", false},

		// NAT64 local-use prefix, 64:ff9b:1::/48 (RFC 8215). Same semantics as the
		// well-known prefix and a separate range: an implementation matching only
		// 64:ff9b::/96 lets every one of these through, and the metadata-service row
		// is what that costs.
		{"NAT64 local-use embedding loopback", "64:ff9b:1::7f00:1", true},
		{"NAT64 local-use embedding the metadata service", "64:ff9b:1::a9fe:a9fe", true},
		{"NAT64 local-use embedding a public address", "64:ff9b:1::808:808", false},

		// SIIT IPv4-translated, ::ffff:0:0:0/96 — bytes 0-7 zero, 8-9 ffff, 10-11
		// zero, IPv4 in the last 4. Decoded, not banned: like NAT64, the embedded
		// address is the destination.
		//
		// THIS ONE MUST BE A BYTE-PATTERN DECODE AND MUST NEVER BECOME A CIDR
		// ENTRY. Two separate traps sit on top of each other here, and the second
		// is a production outage:
		//
		//  1. It READS as already covered. ::ffff:0:127.0.0.1 is one group away
		//     from ::ffff:127.0.0.1, which is blocked — but they are different
		//     prefixes (mapped puts ffff at bytes 10-11, translated at bytes 8-9)
		//     and To4 normalises only the mapped one.
		//
		//  2. The near-miss spelling silently blocks the entire IPv4 internet.
		//     `::ffff:0:0/96` — one ":0" group short of the translated prefix, and
		//     the spelling anyone reaching for a CIDR entry is most likely to
		//     write — is not SIIT at all. It is ::ffff:0.0.0.0, the IPv4-MAPPED
		//     prefix, whose network number passes To4; net.IPNet.Contains then
		//     compares in 4-byte space using the last four bytes of the 16-byte
		//     /96 mask, and those are all zero. Verified with the stdlib:
		//     net.ParseCIDR("::ffff:0:0/96") yields a network that prints as
		//     0.0.0.0/0 and whose Contains returns true for 8.8.8.8 and 1.1.1.1.
		//     Dropped into a reserved-range list, that single typo refuses every
		//     outbound request the AppView makes.
		//
		//     (The correctly-spelled `::ffff:0:0:0/96` does NOT degenerate — it
		//     parses and prints as itself. It is still the wrong tool, because a
		//     range entry bans the prefix where the public-payload row below
		//     requires it to be decoded.)
		{"SIIT translated embedding loopback", "::ffff:0:7f00:1", true},
		{"SIIT translated embedding RFC1918 10/8", "::ffff:0:a00:1", true},
		{"SIIT translated embedding a public address", "::ffff:0:808:808", false},

		// IPv4-compatible IPv6, ::/96 — deprecated, the IPv4 is the last 4 bytes,
		// and Go's To4 does NOT normalise this form (it normalises only ::ffff:),
		// which is why it reaches the classifier as sixteen opaque bytes.
		{"IPv4-compatible embedding loopback", "::7f00:1", true},
		{"IPv4-compatible embedding RFC1918 10/8", "::a00:1", true},

		// ::1 IS BLOCKED TWICE OVER, AND IT USED TO BE BLOCKED ONCE.
		//
		// IsLoopback catches it, and that check runs BEFORE the embedded decode.
		// The ordering used to be the only thing protecting it: read as an
		// IPv4-compatible address, ::1's payload is 0.0.0.1, so a decode running
		// first would have classified THAT instead — and 0.0.0.1 was public.
		// Mutation testing found the exposure: moving the decode block to the top
		// of isPrivateIP was killed by exactly one assertion in the whole package,
		// the ::1 row in transport_test.go. "Unspecified IPv6" (::) did not cover
		// it, because :: survives that reorder on its payload's own merits
		// (0.0.0.0 is still unspecified).
		//
		// Blocking 0.0.0.0/8 removed the dependency rather than documenting it:
		// the payload is now private in its own right, so the two checks agree and
		// the order between them no longer decides the answer. This row stays as
		// the statement of the invariant, in the file whose mechanism it concerns
		// instead of only in a legacy file nobody knows is load-bearing.
		{"IPv6 loopback via the IPv4-compatible decode path", "::1", true},

		// Control: the mapped form, which To4 DOES normalise, so it reaches the
		// loopback check as a 4-byte address and never needs the decode at all.
		// Pinned because it is the half that always worked — changes to its
		// siblings must not disturb it.
		{"IPv4-mapped embedding loopback", "::ffff:127.0.0.1", true},

		// THE TEREDO LANDMINE, pinned rather than merely described.
		//
		// Teredo (2001:0000::/32) also embeds IPv4 and is a plausible future
		// addition to this file. The mistake to guard against is writing the
		// prefix as 2001::/16, which is sixteen bits too wide and swallows a large
		// slice of live production IPv6. These two addresses are real atProto
		// PDSes — socl.is and pds.zzls.xyz — and a /16 rule blocks both while
		// 2001:0000::/32 blocks neither.
		//
		// They pass today because nothing matches 2001::/anything. That is the
		// point: they are a tripwire for a change nobody has made yet, and the
		// cost of not having them is silently unreachable federation peers.
		{"Live atProto PDS under 2001::/16", "2001:19f0:7002:191::", false},
		{"Second live atProto PDS under 2001::/16", "2001:550:5a00:785b::1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "the test's own input %q must parse as an IP", tt.ip)

			if tt.blocked {
				assert.True(t, isPrivateIP(ip),
					"isPrivateIP(%s) returned false: this IPv6 address reaches a local or internal destination "+
						"through a spelling the guard does not read — either it carries a private IPv4 payload, or "+
						"it is a tunnelling prefix whose far side the payload does not describe", tt.ip)
				return
			}
			assert.False(t, isPrivateIP(ip),
				"isPrivateIP(%s) returned true: this address is an ordinary public destination, so blocking it "+
					"means a prefix was banned wholesale where the embedded address should have been extracted "+
					"and re-checked", tt.ip)
		})
	}
}

// TestEmbeddedIPv4_DoesNotPanicOnMalformedAddresses pins the guard that
// embeddedIPv4's own comment says is load-bearing.
//
// The function indexes a 16-byte slice directly, and its length check is what
// stands between a malformed net.IP and an out-of-range panic. That panic would
// not be a wrong answer for one request — isPrivateIP runs on every resolved
// address of every outbound call, so it would take down every outbound request
// the process makes. Nothing exercised it before this test.
//
// A malformed slice is not hypothetical: net.IP is a byte slice with exported
// contents, callers construct them, and net.ParseIP is not the only way one
// arrives. The assertion is both halves — no panic, AND nil, because inventing a
// destination out of three arbitrary bytes would be its own bug.
func TestEmbeddedIPv4_DoesNotPanicOnMalformedAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   net.IP
	}{
		{"too short", net.IP{1, 2, 3}},
		{"too long", net.IP{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		{"empty", net.IP{}},
		{"nil", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.NotPanics(t, func() {
				assert.Nil(t, embeddedIPv4(tt.ip),
					"a %d-byte slice is not an address and must decode to nothing", len(tt.ip))
			}, "embeddedIPv4 panicked on a %d-byte slice; isPrivateIP runs on every resolved address of every "+
				"outbound request, so a panic here fails all of them", len(tt.ip))

			assert.NotPanics(t, func() {
				_ = isPrivateIP(tt.ip)
			}, "isPrivateIP panicked on a %d-byte slice", len(tt.ip))
		})
	}
}
