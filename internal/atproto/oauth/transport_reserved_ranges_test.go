package oauth

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsPrivateIP_ReservedAndUnspecifiedRanges pins the reserved address classes
// beyond RFC1918 and loopback.
//
// # WHY AN ALLOWLIST-SHAPED PROBLEM IS BEING SOLVED WITH A DENYLIST
//
// The predicate is a denylist, so anything it does not recognise is "public" by
// default and gets dialled. That default is the dangerous direction: the address
// space holds far more reserved territory than RFC1918 and loopback, and every
// range below either reaches this host, reaches the operator's own network, or
// reaches something the kernel treats specially. A URL is chosen by a stranger —
// a DID document's PDS endpoint, an acceptance record's subject — so the attacker
// picks from the whole space, not from the part we remembered. Each class here
// was missing at some point, and the table is what stops the list regressing to
// the part that was obvious.
//
// The classes, and what each one actually reaches:
//
//   - UNSPECIFIED (0.0.0.0, ::, ::ffff:0.0.0.0). A wildcard in bind(), not in
//     connect(): the kernel substitutes the local host, so it is loopback wearing
//     a different number. This is the class the acceptance test pins end-to-end.
//   - CGNAT (100.64.0.0/10). Carrier-grade NAT, and the default subnet of a great
//     deal of infrastructure — Tailscale hands out 100.64/10 addresses, so this
//     range is frequently the operator's private mesh.
//   - IETF PROTOCOL ASSIGNMENTS (192.0.0.0/24). Reserved for protocol machinery
//     (DS-Lite's 192.0.0.0/29 among it); nothing here is a destination a caller
//     legitimately names.
//   - BENCHMARKING (198.18.0.0/15). Reserved for device test harnesses and, in
//     practice, routed internally where it is used at all.
//   - RESERVED (240.0.0.0/4) and BROADCAST (255.255.255.255). Former class E and
//     the all-hosts broadcast; the stack handles both unlike a normal unicast
//     destination.
//   - MULTICAST, both families. A single packet addressed to a group, which is a
//     way to touch hosts on the local segment without naming one.
//
// The multicast case carries a trap worth stating, because the obvious
// implementation walks into it: adding "224.0.0.0/4" to the CIDR list covers IPv4
// ONLY, and leaves every IPv6 multicast scope open. That is why ff02::1
// (link-local all-nodes), ff05::1 (site-local) and ff0e::1 (global) are each
// pinned separately rather than represented by one row — a fix that generalises
// over scope passes all three, and a CIDR-shaped fix does not.
//
// # THE ALLOWED ROWS, AND WHICH ERROR EACH ONE CATCHES
//
// They are not padding, but they do not all guard the same mistake, and the
// distinction is easy to get backwards because net.ParseCIDR normalises a prefix
// to its network address.
//
//   - The "just BELOW" rows catch a range written one bit too wide. Widen
//     100.64.0.0/10 to /9 and it normalises to 100.0.0.0/9 — 100.0.0.0 through
//     100.127.255.255 — which swallows `100.63.255.255`. Same shape for
//     198.18.0.0/15 → 198.16.0.0/14, which swallows `198.17.255.255`.
//   - The "just ABOVE" rows catch a different error: a wrong base address, or a
//     widening of two bits or more. They do NOT catch the one-bit case, because
//     normalising to the network address extends a range downward rather than
//     upward — 100.0.0.0/9 stops below `100.128.0.0`, and 198.16.0.0/14 stops
//     below `198.20.0.0`.
//
// One row is neither: `9.255.255.255` sits below 10/8, which was covered long
// before this table existed. It is a plain regression guard on the pre-existing
// RFC1918 boundary, kept because the table is where someone will look.
func TestIsPrivateIP_ReservedAndUnspecifiedRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Unspecified — the local host by another name, in all three spellings.
		{"Unspecified IPv4", "0.0.0.0", true},
		{"Unspecified IPv6", "::", true},
		{"Unspecified IPv4-mapped IPv6", "::ffff:0.0.0.0", true},

		// The REST of 0.0.0.0/8, which is a different claim from the unspecified
		// address and rests on a different argument.
		//
		// It is NOT that these are independently reachable. Two reviewers checked
		// in a Linux container and found they are not: `ip route get 0.0.0.5`
		// takes the default route, not lo. The reason to block the /8 is the
		// ordering dependency it removes. ::1 read as an IPv4-compatible address
		// decodes to 0.0.0.1, so IPv6 loopback stays blocked today only because
		// isPrivateIP's loopback check runs BEFORE its embedded-payload decode —
		// an invariant no assertion in this package stated until now, and one that
		// a reordering refactor would silently delete. Blocking 0.0.0.0/8 makes
		// the payload private on its own merits, so the order stops mattering.
		// See the ::1 row in transport_embedded_ipv4_test.go.
		//
		// "This host on this network" is in any case not a destination a caller
		// legitimately names, so the range costs nothing to give up.
		{"Zero network low", "0.0.0.1", true},
		{"Zero network mid", "0.1.2.3", true},
		{"Zero network high edge", "0.255.255.255", true},

		// CGNAT 100.64.0.0/10 — carrier NAT and Tailscale meshes.
		{"CGNAT low edge", "100.64.0.1", true},
		{"CGNAT high edge", "100.127.255.255", true},

		// IETF protocol assignments 192.0.0.0/24. Both ends are pinned because a
		// single interior row does not distinguish the /24 from the /29 that
		// DS-Lite occupies inside it — an implementation narrowed to 192.0.0.0/29
		// would pass on 192.0.0.1 alone.
		{"IETF protocol assignments first", "192.0.0.0", true},
		{"IETF protocol assignments last", "192.0.0.255", true},

		// Benchmarking 198.18.0.0/15.
		{"Benchmarking low edge", "198.18.0.1", true},
		{"Benchmarking high edge", "198.19.255.255", true},

		// Reserved 240.0.0.0/4, pinned at both ends for the same reason. It runs
		// all the way to the limited broadcast address, which is why there is no
		// "just above" row: there is no above. Nor is there a meaningful "just
		// below" — 239.255.255.255 is multicast and blocked on that ground.
		{"Reserved former class E first", "240.0.0.0", true},
		{"Reserved former class E interior", "240.0.0.1", true},
		{"Reserved former class E penultimate", "255.255.255.254", true},
		{"Limited broadcast", "255.255.255.255", true},

		// Multicast IPv4 224.0.0.0/4.
		{"Multicast IPv4 internetwork control", "224.0.1.1", true},
		{"Multicast IPv4 source-specific", "233.1.2.3", true},
		{"Multicast IPv4 SSDP", "239.255.255.250", true},

		// Multicast IPv6 — every scope, because scope is where a v4-shaped fix leaks.
		{"Multicast IPv6 link-local all-nodes", "ff02::1", true},
		{"Multicast IPv6 site-local", "ff05::1", true},
		{"Multicast IPv6 global scope", "ff0e::1", true},

		// IPv6 site-local fec0::/10 — deprecated by RFC 3879 and superseded by
		// fc00::/7, but still an "internal network" range, and stacks that
		// recognise it route it. It falls outside BOTH stdlib predicates that
		// look like they should cover it: IsPrivate is fc00::/7 and
		// IsLinkLocalUnicast is fe80::/10, and fec0::/10's bit pattern is
		// disjoint from each. Neither neighbour gives it a boundary row —
		// fe80::/10 below and ff00::/8 multicast above are both blocked already.
		{"IPv6 site-local deprecated", "fec0::1", true},

		// Boundaries that must STAY reachable, so the ranges above cannot be
		// implemented one bit too wide.
		{"Just above the zero network", "1.0.0.0", false},
		{"Just below CGNAT", "100.63.255.255", false},
		{"Just above CGNAT", "100.128.0.0", false},
		{"Just below benchmarking", "198.17.255.255", false},
		{"Just above benchmarking", "198.20.0.0", false},
		{"Just above IETF protocol assignments", "192.0.1.0", false},
		{"Above IETF protocol assignments", "192.0.1.1", false},
		{"Just below multicast", "223.255.255.255", false},
		{"Just below 10/8", "9.255.255.255", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "the test's own input %q must parse as an IP", tt.ip)

			if tt.blocked {
				assert.True(t, isPrivateIP(ip),
					"isPrivateIP(%s) returned false: this address reaches a local or internal destination, "+
						"so a caller-supplied URL naming it is an SSRF the guard waves through", tt.ip)
				return
			}
			assert.False(t, isPrivateIP(ip),
				"isPrivateIP(%s) returned true: this is an ordinary public address one step outside a "+
					"reserved range, and blocking it means the range was implemented too wide", tt.ip)
		})
	}
}

// TestReservedNetworks_ContainNoPublicAddress is a tripwire on the range list
// itself, not on any address in particular.
//
// A CIDR entry can silently mean something far wider than it reads, and the
// failure is total rather than partial. The worked example, verified against the
// stdlib: `net.ParseCIDR("::ffff:0:0/96")` — a plausible-looking way to write the
// SIIT IPv4-translated prefix, and one group short of the real one — returns a
// network that prints as `0.0.0.0/0`. Its network number is the IPv4-mapped
// 0.0.0.0, so it passes To4, and Contains then compares in 4-byte space against
// the last four bytes of a 16-byte /96 mask, which are zero. Contains(8.8.8.8) is
// true. Added to the list below, that one entry refuses every outbound request
// the AppView makes — no federation, no identity resolution, nothing.
//
// A per-address table cannot catch this, because the table only asks about
// addresses someone thought to write down. This asks the inverse question of the
// whole list at once: does any entry claim an address that is unambiguously
// public? An entry that has degenerated answers yes to all of them.
//
// See the SIIT block in transport_embedded_ipv4_test.go for why that prefix must
// be a byte-pattern decode and must never be added here.
func TestReservedNetworks_ContainNoPublicAddress(t *testing.T) {
	t.Parallel()

	// Ordinary public destinations, deliberately spread across the space rather
	// than clustered: a degenerate entry catches all of them, a merely
	// over-broad one might catch only its neighbourhood.
	publicAddresses := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
		"2001:4860:4860::8888",
		"2600::1",
	}

	require.NotEmpty(t, reservedNetworks,
		"the reserved-range list is empty, so every range test below is passing vacuously")

	for _, network := range reservedNetworks {
		for _, address := range publicAddresses {
			ip := net.ParseIP(address)
			require.NotNil(t, ip, "the test's own input %q must parse as an IP", address)

			assert.False(t, network.Contains(ip),
				"the reserved range %s contains the public address %s. Either it was written too wide, or it "+
					"is a prefix that DEGENERATED on parse — net.ParseCIDR reduces some IPv6 prefixes whose "+
					"network number passes To4 down to an IPv4 mask, and ::ffff:0:0/96 becomes 0.0.0.0/0 that "+
					"way. A range in this list that matches public traffic takes down every outbound request",
				network, address)
		}
	}
}

// TestIsPrivateIP_MappedSpellingsOfReservedRanges pins an assumption the range
// checks make without stating it.
//
// Every range above is expressed as an IPv4 CIDR, and every one of them is
// nonetheless matched when the same address arrives in its ::ffff: mapped form.
// That works because net.IPNet.Contains normalises through To4 internally — a
// standard-library behaviour, not something this package does. So the mapped
// spellings are correct today by inheritance, and nothing in the tree says they
// have to be.
//
// The rows matter because the inheritance is not guaranteed to survive a
// refactor. A range check rewritten as a byte-prefix comparison, a length-16
// fast path, or a hand-rolled mask — all reasonable-looking optimisations for a
// predicate on the per-request hot path — drops the normalisation, and every one
// of these addresses becomes public while the unmapped spelling stays blocked.
// One prefix on a URL is not a difficult thing for an attacker to try.
//
// One row per range, plus a public control so a fix cannot pass by treating the
// mapped prefix itself as reserved.
func TestIsPrivateIP_MappedSpellingsOfReservedRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"Mapped zero network", "::ffff:0.0.0.1", true},
		{"Mapped CGNAT", "::ffff:100.64.0.1", true},
		{"Mapped IETF protocol assignments", "::ffff:192.0.0.1", true},
		{"Mapped benchmarking", "::ffff:198.18.0.1", true},
		{"Mapped reserved former class E", "::ffff:240.0.0.1", true},
		{"Mapped limited broadcast", "::ffff:255.255.255.255", true},
		{"Mapped multicast", "::ffff:224.0.1.1", true},
		{"Mapped link-local metadata service", "::ffff:169.254.169.254", true},
		{"Mapped RFC1918", "::ffff:10.0.0.1", true},

		// The control: a public address in the same spelling. If this were
		// blocked, the mapped prefix would be acting as a reserved range in its
		// own right and the rows above would prove nothing.
		{"Mapped public address", "::ffff:100.128.0.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "the test's own input %q must parse as an IP", tt.ip)

			if tt.blocked {
				assert.True(t, isPrivateIP(ip),
					"isPrivateIP(%s) returned false: the unmapped spelling of this address is blocked, so a "+
						"caller need only write it with an ::ffff: prefix to reach the same destination", tt.ip)
				return
			}
			assert.False(t, isPrivateIP(ip),
				"isPrivateIP(%s) returned true: this is a public address, and blocking it means the mapped "+
					"prefix is being treated as reserved rather than decoded", tt.ip)
		})
	}
}
