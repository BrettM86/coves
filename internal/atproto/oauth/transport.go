package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ssrfSafeTransport wraps http.Transport to prevent SSRF attacks
type ssrfSafeTransport struct {
	base         *http.Transport
	allowPrivate bool // For dev/testing only

	// lookupIP resolves a hostname. A field so a test can drive the
	// check-then-dial window that the guard has to close; nil means net.LookupIP.
	lookupIP func(host string) ([]net.IP, error)
}

// resolveHost is the transport's one name lookup per request.
func (t *ssrfSafeTransport) resolveHost(host string) ([]net.IP, error) {
	if t.lookupIP != nil {
		return t.lookupIP(host)
	}
	return net.LookupIP(host)
}

// reservedNetworks are the ranges, in BOTH families, that no stdlib predicate
// names.
//
// Parsed ONCE, at package scope, because isPrivateIP runs on the per-request hot
// path — every resolved address of every outbound call walks this list.
//
// Each entry is a destination a caller-supplied URL has no business naming:
//
//   - 0.0.0.0/8, "this network". The honest reason is NOT reachability: the rest
//     of the /8 is not independently routable, and `ip route get 0.0.0.5` takes
//     the default route rather than lo. It is that ::1 read as an
//     IPv4-compatible address decodes to 0.0.0.1, so blocking the /8 makes that
//     payload private on its own merits and removes isPrivateIP's dependence on
//     testing loopback BEFORE it decodes a payload. An ordering invariant that
//     nothing enforces is one a refactor deletes silently.
//   - 100.64.0.0/10 is carrier-grade NAT, and in practice the operator's own
//     mesh — Tailscale hands out addresses from this block.
//   - 192.0.0.0/24 is reserved for protocol machinery (DS-Lite's 192.0.0.0/29
//     among it), never a destination a caller legitimately asks for.
//   - 198.18.0.0/15 is the benchmarking range, routed internally where it is
//     routed at all.
//   - 240.0.0.0/4 is former class E, and it carries 255.255.255.255 with it:
//     the all-hosts broadcast, which the stack handles unlike a unicast
//     destination.
//   - 2002::/16 is 6to4, banned outright rather than decoded. Its embedded IPv4
//     names the tunnel's gateway, not where the packet ends up — see
//     embeddedIPv4, which explains why this one prefix is the exception to
//     everything that file does.
//   - fec0::/10 is IPv6 site-local: deprecated by RFC 3879 and superseded by
//     fc00::/7, but a stack that still recognises it routes it as an internal
//     network. It falls outside BOTH predicates that look like they cover it,
//     since IsPrivate is fc00::/7, IsLinkLocalUnicast is fe80::/10, and
//     fec0::/10's bit pattern is disjoint from each.
//
// NEVER add ::ffff:0:0/96 to this list to cover SIIT. It is not the SIIT prefix,
// and net.ParseCIDR degenerates it to 0.0.0.0/0 — the full reasoning is on the
// SIIT branch in embeddedIPv4, which is where that prefix is handled instead.
var reservedNetworks = []*net.IPNet{
	mustParseCIDR("0.0.0.0/8"),
	mustParseCIDR("100.64.0.0/10"),
	mustParseCIDR("192.0.0.0/24"),
	mustParseCIDR("198.18.0.0/15"),
	mustParseCIDR("240.0.0.0/4"),
	mustParseCIDR("2002::/16"),
	mustParseCIDR("fec0::/10"),
}

// mustParseCIDR panics on a malformed prefix. Its arguments are compile-time
// constants in this file, so a failure is a typo caught at startup rather than a
// range that silently stops being checked.
func mustParseCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("oauth: malformed reserved CIDR %q: %v", cidr, err))
	}
	return network
}

// isPrivateIP reports whether an address reaches this host, the operator's own
// network, or something the kernel treats specially.
//
// THE DEFAULT IS THE DANGEROUS DIRECTION. Anything this predicate does not
// recognise is treated as public and dialled, and the address space holds far
// more reserved territory than RFC1918. Every host that reaches this transport
// was chosen by a stranger — a DID document's PDS endpoint, an acceptance
// record's subject — so the attacker picks from the whole space, not from the
// part we happened to remember.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}

	// The unspecified address is loopback wearing a different number. 0.0.0.0
	// (and :: , and ::ffff:0.0.0.0) is a wildcard in bind() only; in connect()
	// the kernel substitutes the local host, so http://0.0.0.0:5432/ reaches
	// whatever is listening on 127.0.0.1:5432.
	if ip.IsUnspecified() {
		return true
	}

	if ip.IsLoopback() {
		return true
	}

	// Link-local reaches the local segment without routing, and 169.254.169.254
	// is the cloud instance metadata service.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// A group address touches every listener on the segment without naming one.
	// This has to be the FAMILY-AGNOSTIC predicate rather than a 224.0.0.0/4
	// entry in the list below, which would cover IPv4 only: the scopes it would
	// leave open are ff05:: (site-local) and ff0e:: (global). NOT ff02::, which
	// IsLinkLocalMulticast already catches one check above — mutation testing
	// confirmed the site-local and global scopes are the only two that
	// discriminate here.
	//
	// Which is why IsLinkLocalMulticast must stay even though this line now makes
	// it redundant FOR IPv6: for IPv4 it is not redundant at all, and it is the
	// only cover for 224.0.0.x if IsMulticast were ever narrowed.
	if ip.IsMulticast() {
		return true
	}

	// RFC1918 and IPv6 unique-local, via the stdlib so the masks cannot drift.
	if ip.IsPrivate() {
		return true
	}

	for _, network := range reservedNetworks {
		if network.Contains(ip) {
			return true
		}
	}

	// Last, because everything above answers "which block is this address in" and
	// some IPv6 forms defeat that question rather than answering it wrongly: the
	// destination is a four-byte field carried INSIDE the address.
	//
	// Position is no longer load-bearing, and that is deliberate. ::1 read as an
	// IPv4-compatible address decodes to 0.0.0.1, so running the decode first
	// used to reclassify IPv6 loopback on a payload nothing blocked; 0.0.0.0/8 in
	// the list above now blocks that payload too, and the reorder is safe.
	if embedded := embeddedIPv4(ip); embedded != nil {
		return isPrivateIP(embedded)
	}

	return false
}

// embeddedIPv4 returns the IPv4 address an IPv6 address is carrying, or nil.
//
// # WHY THESE PAYLOADS ARE DECODED RATHER THAN THEIR PREFIXES BANNED
//
// 64:ff9b::7f00:1 and 64:ff9b::808:808 differ only in the four bytes the prefix
// carries — one is 127.0.0.1 and the other is 8.8.8.8 — and no CIDR over the
// IPv6 space separates them. Banning the prefix to catch the first would be an
// outage, not a trade: 64:ff9b::/96 is a legitimate connect() destination, and
// an IPv6-only host with DNS64 reaches every IPv4-only server in the world
// through it. On such a deployment a wholesale ban does not block some outbound
// federation, it blocks all of it.
//
// # WHAT IS DECODED HERE
//
//   - NAT64 well-known prefix, 64:ff9b::/96 (RFC 6052) — IPv4 in the last four
//     bytes. Purpose-built to mean "this IPv4 host", so a translator on the path
//     delivers it exactly there.
//   - NAT64 local-use prefix, 64:ff9b:1::/48 (RFC 8215) — the same mechanism
//     with an operator-chosen prefix, and the same consequence if banned.
//   - SIIT IPv4-translated, ::ffff:0:0:0/96 (RFC 6052 §2.2) — IPv4 in the last
//     four bytes, and likewise the destination itself.
//   - IPv4-compatible IPv6, ::/96 — deprecated. It slips past every range check
//     because of the standard library, not this package: To4 normalises ONLY the
//     ::ffff: mapped form, so ::ffff:127.0.0.1 reaches the loopback test as a
//     4-byte 127.0.0.1 while ::7f00:1 reaches it as sixteen opaque bytes.
//
// # WHAT IS DELIBERATELY NOT DECODED
//
// This is not an exhaustive list of the encodings that embed IPv4, and it is not
// trying to be. Each exclusion has a precondition that does not hold here:
//
//   - 6to4, 2002::/16 — BANNED WHOLESALE in reservedNetworks instead, and the
//     asymmetry with NAT64 is the thing to remember about this function. Making
//     the two symmetric is wrong in either direction. 6to4 embeds a GATEWAY:
//     2002:V4ADDR:SLA:iface names a tunnel endpoint in bytes 2..6 and then a
//     subnet and a host BEHIND it, so a public payload says only who the tunnel
//     belongs to and nothing about the far side, and decoding answers a question
//     nobody asked. NAT64 embeds the DESTINATION, so its payload is precisely
//     what to classify. 6to4 is banned; NAT64 is decoded.
//
//     Do not shorten the justification to "RFC 7526 deprecated it", because that
//     is false: RFC 7526 deprecates the ANYCAST RELAY prefix 192.88.99.0/24 and
//     says of the rest, verbatim, that "the associated 6to4 IPv6 prefix
//     2002::/16 are not deprecated". The ban is our policy call. What makes it
//     cheap is RFC 7526 §4 ("in host implementations, unicast 6to4 MUST also be
//     disabled by default"), 2002::/16's place on the standard bogon lists, and
//     6to4 rounding to 0.00% of Google's measured IPv6 traffic.
//
//   - Teredo — needs a Teredo tunnel on the host. If it is ever added the prefix
//     is 2001:0000::/32 and NEVER 2001::/16, which is sixteen bits too wide:
//     live atProto PDSes sit at 2001:19f0:7002:191:: and 2001:550:5a00:785b::1,
//     and a /16 rule blocks both. They are pinned as allowed rows in the tests.
//
//   - ISATAP — its 0000:5efe:V4ADDR interface identifier can appear under ANY
//     unicast /64, so matching it means reading the low bytes of every IPv6
//     address rather than recognising a prefix, and it reaches nothing without
//     an ISATAP interface configured on this host.
//
//   - RFC 6052 network-specific prefixes — the operator chooses the prefix, it
//     may be a /32, /40, /48, /56, /64 or /96, and the IPv4 sits at a different
//     offset in each. There is no set of them to enumerate.
func embeddedIPv4(ip net.IP) net.IP {
	// An IPv4 address carries no payload of its own — which is also what bounds
	// isPrivateIP's recursion to a single step, since every return below is one.
	// The ::ffff: mapped form lands here too, already normalised by To4.
	if ip.To4() != nil {
		return nil
	}

	// nil for any length that is not an address, so the indexing below cannot
	// panic on a malformed slice. A panic here would take down every outbound
	// request, not just the odd one.
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}

	// NAT64, well-known prefix: 64:ff9b:: followed by eight zero bytes.
	if hasBytePrefix(ip16, 0x00, 0x64, 0xff, 0x9b) && isAllZero(ip16[4:12]) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}

	// NAT64, local-use prefix: the same four bytes, then 0001, then zeroes.
	//
	// Only the /96-suffix shape is read. RFC 8215 hands the operator a /48 and
	// RFC 6052 then puts the IPv4 at an offset that depends on the prefix length
	// they actually deployed, so a 64:ff9b:1:: address with a non-zero middle is
	// one whose payload offset this code cannot know — and guessing wrong would
	// invent a destination rather than find one.
	if hasBytePrefix(ip16, 0x00, 0x64, 0xff, 0x9b, 0x00, 0x01) && isAllZero(ip16[6:12]) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}

	// SIIT IPv4-translated: eight zero bytes, ffff, two more zero bytes.
	//
	// A BYTE PATTERN AND NOT A CIDR ENTRY, deliberately, because the CIDR that
	// looks right is a production outage. ::ffff:0:0/96 — one ":0" group short of
	// the translated prefix, and the spelling anyone reaching for a range entry
	// writes first — is the IPv4-MAPPED prefix, whose network number passes To4;
	// net.IPNet.Contains then compares in 4-byte space against the last four
	// bytes of a 16-byte /96 mask, which are zero. The result parses and prints
	// as 0.0.0.0/0 and Contains(8.8.8.8) is true, so that single typo in
	// reservedNetworks refuses every outbound request the AppView makes.
	//
	// The correctly spelled ::ffff:0:0:0/96 does not degenerate, and is still the
	// wrong tool: a range entry bans the prefix, and the destination here has to
	// be decoded and re-checked like NAT64's.
	if isAllZero(ip16[:8]) && ip16[8] == 0xff && ip16[9] == 0xff && isAllZero(ip16[10:12]) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}

	// IPv4-compatible: twelve zero bytes. Disjoint from SIIT above, which carries
	// ffff where this form has zeroes.
	if isAllZero(ip16[:12]) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}

	return nil
}

// hasBytePrefix reports whether b begins with the given bytes. The length test
// is what keeps the prefixes above from indexing past a short slice.
func hasBytePrefix(b []byte, prefix ...byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i, want := range prefix {
		if b[i] != want {
			return false
		}
	}
	return true
}

// isAllZero reports whether every byte is zero, which is how the prefixes above
// are recognised without allocating a mask per call.
func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// vettedAddrsKeyType keys the addresses RoundTrip approved, so the dialler can
// read them off the request's own context. A private type, so nothing outside
// this file can plant a value under the same key.
type vettedAddrsKeyType struct{}

var vettedAddrsKey vettedAddrsKeyType

// RoundTrip vets the hostname's addresses and then makes the dial use THOSE
// ADDRESSES rather than the name.
//
// PASSING THE NAME ON WOULD BE A SECOND DECISION. The base transport resolves
// whatever host it is given, so a guard that approved answer A and then handed
// over the hostname lets the dialler act on answer B — and nothing binds the two
// together. DNS rebinding is the name for exploiting that: an attacker who
// controls the zone answers the first query publicly and the second with
// 169.254.169.254, and the approval describes a host the connection never went
// to. Every input that reaches this transport is chosen by a stranger (a DID
// document's PDS endpoint, an acceptance record's subject), so the attacker
// picks the moment to flip as well.
//
// The addresses ride on the request context and the dialler below consumes
// them, which also means the hostname is resolved exactly ONCE per request.
// That is the property, not an optimisation: any later answer is one the guard
// never saw.
func (t *ssrfSafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	// A literal address is already the thing that will be dialled, so there is
	// no second resolution to defend against — but it still has to pass the
	// private check below.
	ips, err := t.resolveHost(host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve host: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("failed to resolve host: %s resolved to no addresses", host)
	}

	// Check all resolved IPs
	if !t.allowPrivate {
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return nil, fmt.Errorf("SSRF blocked: %s resolves to private IP %s", host, ip)
			}
		}
	}

	return t.base.RoundTrip(req.WithContext(context.WithValue(req.Context(), vettedAddrsKey, ips)))
}

// NewSSRFSafeHTTPClient creates an HTTP client with SSRF protections
func NewSSRFSafeHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &ssrfSafeTransport{
		base: &http.Transport{
			// The dial IGNORES the hostname in addr and connects to an address
			// RoundTrip already approved, which is what closes the
			// check-then-dial window. It takes only the port from addr, because
			// the port is the one part of the destination the guard has no
			// opinion about.
			//
			// FAIL CLOSED when there is nothing vetted: reaching here without a
			// context value means this base transport was used directly rather
			// than through RoundTrip, which is exactly the unguarded path the
			// wrapper exists to prevent.
			//
			// TLS is unaffected. http.Transport derives the handshake's server
			// name from the request URL rather than from the dial address, so
			// certificate verification and SNI still name the host the caller
			// asked for.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				vetted, _ := ctx.Value(vettedAddrsKey).([]net.IP)
				if len(vetted) == 0 {
					return nil, fmt.Errorf("SSRF blocked: refusing to dial %s with no vetted address "+
						"(the SSRF-safe transport was bypassed)", addr)
				}
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("SSRF blocked: cannot read a port from %q: %w", addr, err)
				}
				var lastErr error
				for _, ip := range vetted {
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				return nil, lastErr
			},
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		allowPrivate: allowPrivate,
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}
