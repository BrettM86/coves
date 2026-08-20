package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

// ErrBlockedAddress is the sentinel every address refusal matches, so a caller
// distinguishes "the guard refused this" from "the network failed" with
// errors.Is rather than by matching on a message.
//
// A resolution failure must NOT match it. The two need opposite handling — a
// DNS hiccup is retryable and unremarkable, a refusal is a security event — so
// wrapping every RoundTrip error in this sentinel would hand every caller a
// signal that means "something went wrong" and nothing more.
var ErrBlockedAddress = errors.New("SSRF blocked")

// blockedDial builds a refusal from the DIAL path, matching ErrBlockedAddress.
//
// A CONSTRUCTOR RATHER THAN THREE fmt.Errorf CALLS, because all three of those
// refusals used to render "SSRF blocked" and match nothing — the words were
// right and the identity was missing, which is the worst of both: the string
// reads as a block to a human scanning a log, and classifies as an ordinary
// network failure to the code deciding whether to retry. One place to get it
// right is one place for the next refusal added here to get it right too.
//
// The rendering is unchanged: ErrBlockedAddress's own message is "SSRF blocked",
// so "%w: " reproduces the prefix the rest of this package asserts on.
//
// The dial path has no BlockedAddressError of its own on purpose. That type
// carries a host and the address it resolved to, and none of these three
// refusals has resolved anything — they are structural failures of the dial
// itself, not a classification.
func blockedDial(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBlockedAddress}, args...)...)
}

// BlockedAddressError carries the detail behind a refusal — which host, and
// which of its addresses caused it — where errors.As can reach it and a
// rendered message cannot leak it.
//
// The split is the point. An operator debugging a block still has to know which
// answer caused it, especially when a name resolved to several and only one was
// private; a reader of the rendered string must not learn the same thing,
// because error strings travel into HTTP response bodies, shared logs and
// support tickets, and every host that reaches this transport was named by a
// stranger.
type BlockedAddressError struct {
	Host string

	// `json:"-"` because encoding/json is the OTHER renderer that reaches this
	// field, and it is reached on the same journeys Error() was rewritten for:
	// a structured logger handed the error value, an API rendering a failure as
	// JSON. Marshalling would put the address back beside the attacker-chosen
	// host — the whole mapping primitive in one object.
	//
	// IT COVERS encoding/json AND NOTHING ELSE. %#v and %+v read the field
	// through reflection regardless of tags, so a caller that dumps this struct
	// with a formatting verb still sees the address; shutting that route down
	// needs the field unexported behind an accessor, which is a larger change
	// and is not made here. errors.As remains the intended way to reach it.
	IP net.IP `json:"-"`

	// literal separates the two refusals this type carries — an address written
	// where a hostname belongs, versus a name that resolved to a blocked one —
	// so the rendered sentence is true of the one that actually happened. It is
	// unexported because it selects wording rather than being something a caller
	// acts on; the fields above are the diagnostic. Its zero value gives the
	// resolution wording, which is the case every other construction is.
	literal bool
}

// Error renders the refusal WITHOUT the resolved address.
//
// That address is an internal-network oracle: the attacker supplied the
// hostname — a DID document's serviceEndpoint, an acceptance record's subject,
// a name whose zone they control — so an error that reports what it resolved to
// turns each refusal into a mapping primitive. Point a name at a candidate
// address, read the answer back out of the message, repeat.
//
// The host stays because it is the half of the sentence the attacker already
// supplied, and because it cannot be hidden anyway: http.Client.Do wraps this
// in a *url.Error that embeds the full request URL.
func (e *BlockedAddressError) Error() string {
	if e.literal {
		return fmt.Sprintf("SSRF blocked: %s is an address written where a hostname belongs", e.Host)
	}
	return fmt.Sprintf("SSRF blocked: %s resolves to a private or reserved address", e.Host)
}

// Unwrap makes errors.Is(err, ErrBlockedAddress) hold. The sentinel is the
// checkable identity; this type is the detail behind it.
func (e *BlockedAddressError) Unwrap() error {
	return ErrBlockedAddress
}

// ErrResponseTooLarge is what a read fails with once a response body has
// delivered more than the transport's cap.
//
// IT IS DELIBERATELY NOT io.EOF, and that is the whole point of having a
// sentinel here rather than ending the stream. io.ReadAll treats io.EOF as the
// clean end of a body and returns a nil error, so a cap that reported itself
// that way would hand every caller in this tree a short body indistinguishable
// from a complete one — a parser would read half a document, a size check like
// blobs/service.go's would never fire, and a truncated image would be written
// to a user's PDS as a whole one. Silent truncation is worse than no cap at
// all, because no cap at least fails loudly.
var ErrResponseTooLarge = errors.New("response body exceeds the maximum size")

// ssrfSafeTransport wraps http.Transport to prevent SSRF attacks
type ssrfSafeTransport struct {
	base *http.Transport

	// allowPrivate turns the address guard off, for dev and testing only. Set ONLY
	// by WithPrivateAddressesAllowed, which only ever opens it — the constructor's
	// struct literal leaves it at its zero value, so a client built with no
	// options is guarded. Read in two places in RoundTrip; see the option for why
	// that matters.
	allowPrivate bool

	// maxResponseBytes is how much of a response body a caller may read before
	// the read fails. Set by WithMaxResponseBytes; DefaultMaxResponseBytes
	// otherwise, applied by the constructor BEFORE the options run so a client
	// built without one is capped rather than capped at zero.
	maxResponseBytes int64

	// lookupIP resolves a hostname. A field so a test can drive the
	// check-then-dial window that the guard has to close; nil means the default
	// resolver.
	lookupIP func(ctx context.Context, host string) ([]net.IP, error)
}

// resolveHost is the transport's one name lookup per request.
//
// The lookup runs under the REQUEST'S OWN CONTEXT, which is what makes a
// cancellation mean something here. net.LookupIP takes no context, so a caller
// that gives up — a client that closed the connection, a handler whose deadline
// expired, a shutdown in progress — released nothing: the lookup kept a
// goroutine and a socket alive until the resolver's own unbounded timeout, and
// every fetch site sitting behind a request-scoped context was paying for a
// deadline the resolution never saw.
func (t *ssrfSafeTransport) resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if t.lookupIP != nil {
		return t.lookupIP(ctx, host)
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	// The IPv6 zone on each answer is DISCARDED, and this is a limitation rather
	// than a conversion that loses nothing. Nothing regresses today, because the
	// dialler rebuilds its destination with net.JoinHostPort(ip.String(), port)
	// and that spelling carries no zone either — so a zoned address was never
	// dialled as one. What stays broken is the operator running with the private
	// hatch open who points this client at a link-local address needing its
	// interface (fe80::1%en0): the zone is gone by the time anything tries to
	// connect. Carrying it would mean threading net.IPAddr through the vetted
	// addresses and the dial, which no caller has asked for.
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

// reservedNetworks are the ranges, in BOTH families, that no stdlib predicate
// names. The default is fail-closed for IANA special-purpose space that is not
// globally reachable: local routing can give nominally unroutable ranges a
// meaning, and an attacker choosing a destination gets to exploit that local
// meaning even when the public Internet does not route the prefix.
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
//   - 192.88.99.0/24 is the 6to4 anycast relay, the other half of a mechanism
//     whose destination side 2002::/16 already refuses below. Banning one and
//     not the other is half a decision: this /24 is the address a host sends
//     6to4 traffic TO, and nothing else answers on it — RFC 7526 deprecated it,
//     IANA marks the prefix deprecated, and operators were advised to stop
//     originating the route, so there is no legitimate destination left inside
//     it. (RFC 7526 does NOT itself withdraw the route, and an earlier draft of
//     this comment said it did. Deprecation is not withdrawal; §6 only asks
//     current operators to "consider carefully whether the anycast relay can be
//     discontinued".) Note which way round the RFC cuts here,
//     because it is the reverse of the 2002::/16 case: RFC 7526 deprecates THIS
//     prefix by name, so the entry below is the RFC's call, while banning
//     2002::/16 is ours — see embeddedIPv4, which says so at length.
//   - 198.18.0.0/15 is the benchmarking range, routed internally where it is
//     routed at all.
//   - The three TEST-NET blocks are documentation space. They are not globally
//     reachable, but local labs and overlays do route them; caller-supplied URLs
//     have no legitimate reason to depend on such deployment-specific routes.
//   - 240.0.0.0/4 is former class E, and it carries 255.255.255.255 with it:
//     the all-hosts broadcast, which the stack handles unlike a unicast
//     destination.
//   - 64:ff9b:1::/48 is RFC 8215 local-use translation space. Without an
//     explicitly configured Pref64 the IPv4 payload position is unknowable, so
//     accepting an unrecognised layout is a NAT64 bypass. The well-known
//     globally reachable 64:ff9b::/96 remains decoded in embeddedIPv4.
//   - IPv6 discard, dummy, benchmarking, documentation and SRv6 SID ranges are
//     non-global and may acquire local routing semantics. Teredo and deprecated
//     ORCHID are blocked for the same fail-closed reason.
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
	mustParseCIDR("192.0.2.0/24"),
	mustParseCIDR("192.88.99.0/24"),
	mustParseCIDR("198.18.0.0/15"),
	mustParseCIDR("198.51.100.0/24"),
	mustParseCIDR("203.0.113.0/24"),
	mustParseCIDR("240.0.0.0/4"),
	mustParseCIDR("64:ff9b:1::/48"),
	mustParseCIDR("100::/64"),
	mustParseCIDR("100:0:0:1::/64"),
	mustParseCIDR("2001::/32"),
	mustParseCIDR("2001:2::/48"),
	mustParseCIDR("2001:10::/28"),
	mustParseCIDR("2001:db8::/32"),
	mustParseCIDR("2002::/16"),
	mustParseCIDR("3fff::/20"),
	mustParseCIDR("5f00::/16"),
	mustParseCIDR("fec0::/10"),
}

// 2001::/23 is IANA's IPv6 IETF Protocol Assignments parent reservation. The
// parent is non-global except for the more-specific assignments below, so a
// flat denylist cannot represent it without either failing open on unallocated
// protocol space or blackholing the globally reachable exceptions.
var ietfProtocolAssignments = mustParseCIDR("2001::/23")

var globallyReachableIETFAssignments = []*net.IPNet{
	mustParseCIDR("2001:1::1/128"), // PCP anycast
	mustParseCIDR("2001:1::2/128"), // TURN anycast
	mustParseCIDR("2001:1::3/128"), // DNS-SD registration anycast
	mustParseCIDR("2001:3::/32"),   // AMT
	mustParseCIDR("2001:4:112::/48"),
	mustParseCIDR("2001:20::/28"), // ORCHIDv2
	mustParseCIDR("2001:30::/28"), // Drone Remote ID DETs
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
// network, special-purpose space, or a range that is not globally reachable.
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

	// IANA marks the 2001::/23 parent non-global except for a small set of
	// explicit assignments. Preserve those public services and fail closed on
	// every other address under the parent, including future-looking or locally
	// routed space that a static list of today's named children would miss.
	if ietfProtocolAssignments.Contains(ip) {
		for _, network := range globallyReachableIETFAssignments {
			if network.Contains(ip) {
				return false
			}
		}
		return true
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

// asciiHost returns the hostname in the form net/http will actually use, which
// is the only form worth vetting.
//
// # WHY THE GUARD CANNOT SKIP THIS
//
// net/http punycodes a URL's host before that string becomes a dial address, a
// connection-pool key or a TLS ServerName: canonicalAddr calls
// idnaASCIIFromURL, which calls idnaASCII, which is the two lines below in the
// same order (net/http/transport.go and net/http/request.go). A guard that
// resolves req.URL.Hostname() raw is therefore asking about a string nothing
// downstream will ever use.
//
// For an IDN host that is not a subtle mismatch, it is an outage. The
// production build is CGO_ENABLED=0, so the pure-Go resolver answers, and it
// gates on isDomainName — which permits only [A-Za-z0-9._-] and drops any byte
// at or above 0x80 into its default case. So `bücher.example` comes back "no
// such host" without a packet being sent, and every atProto PDS on a non-ASCII
// domain is unreachable through this client at every site that adopted it. It
// fails closed, which is why nothing noticed: no fixture in this tree uses an
// IDN host.
//
// # THE ASCII SHORT-CIRCUIT IS NOT AN OPTIMISATION
//
// It is the half of this function that prevents a worse regression than the one
// it fixes, and net/http has it for the same reason.
//
// idna.Lookup is not a punycode encoder. The profile applies ValidateLabels,
// CheckHyphens and the BidiRule, so it REFUSES ASCII hostnames that Go's own
// resolver resolves and that this AppView reaches today — `_atproto.example.com`
// (UTS#46 disallows U+005F; net.isDomainName permits it deliberately, citing
// SRV-style underscore labels) and `aa--bb.example.com` (hyphens in the third
// and fourth positions, which is ordinary RFC-valid DNS). Running every host
// through ToASCII would trade "IDN hosts are unreachable" for "IDN hosts and a
// family of ASCII hosts are unreachable".
//
// Byte-wise rather than rune-wise, matching net/http's ascii.Is exactly: a
// hostname that is all ASCII is returned untouched, so the ASCII path through
// this transport is byte-identical to what it was before normalization existed.
// That includes case — ToASCII would lowercase `PDS.Example.COM`, and not
// lowercasing it is what keeps the name this guard vets equal to the name
// net/http computes for the pool key.
func asciiHost(host string) (string, error) {
	if isASCII(host) {
		return host, nil
	}
	return idna.Lookup.ToASCII(host)
}

// isASCII reports whether every byte of s is ASCII, which is net/http's
// ascii.Is under a different name — the comparison is against unicode.MaxASCII
// there too, and matching it byte for byte is the point rather than an
// incidental resemblance.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > unicode.MaxASCII {
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

	// THE REQUEST BODY IS CLOSED ON EVERY RETURN, which net/http requires in
	// those words: "RoundTrip must always close the body, including on errors".
	// http.Client is written against that promise and does not close the body
	// itself, so a refusal that returns early leaks whatever the body was
	// holding — a file handle, a pipe, a buffer with a finalizer — once per
	// refused request. The refusals below are the path a hostile input takes,
	// so the leak scales with how well the guard is working.
	//
	// OWNERSHIP TRANSFERS EXACTLY ONCE, which is why this is a flag rather than
	// an unconditional defer. The base transport takes the body over when it is
	// called and closes it itself, including on its own errors, so the flag is
	// cleared the moment it returns — before the declared-length refusal below,
	// which would otherwise close a SECOND time. Once is the contract in both
	// directions: a body whose Close is not idempotent reports an error the
	// second time.
	ownsBody := req.Body != nil
	defer func() {
		if ownsBody {
			_ = req.Body.Close()
		}
	}()

	// THE HOSTNAME BECOMES ITS A-LABEL BEFORE ANYTHING ELSE LOOKS AT IT.
	//
	// asciiHost says why the translation has to happen at all. This comment is
	// about WHERE it happens, which is a security question and not a tidiness
	// one.
	//
	// BEFORE THE LITERAL CHECK BELOW, because IDNA mapping can PRODUCE an IP
	// literal. The Lookup profile maps before it validates, and its mapping
	// table folds fullwidth digits (U+FF10..U+FF19) and the ideographic full
	// stop (U+3002) onto their ASCII equivalents — so `１２７。０。０。１` is not a
	// literal on the way in and is exactly "127.0.0.1" on the way out, verified
	// by probe. Normalize after the shape check and the check inspects a string
	// that is not yet the address it is about to become.
	//
	// The row where that has consequences is a PUBLIC one. For the loopback
	// spelling, classification is a backstop: the mapped literal resolves to
	// itself and is refused a few lines further down, at the wrong layer but
	// refused. `８.８.８.８` has no backstop — it maps to a public address that
	// classification cannot refuse, and the literal check is the only control
	// standing between a caller-supplied address and a destination this AppView
	// has no business reaching.
	//
	// AFTER THE BODY DEFER ABOVE, because a refusal here is a return, and
	// RoundTrip must close the request body on every one of them.
	//
	// NOT GATED ON THE HATCH, unlike both refusals below it, and the reason is
	// the same one PrivateAddressOptions exists for: `.env.ci:140` sets
	// IS_DEV_ENV=true, so the merge gate runs every call site with allowPrivate
	// open. A translation that only happened on the guarded path would be a
	// translation CI never exercises in the form production runs. It is also not
	// a gate — it decides what gets vetted, never whether vetting happens, which
	// is the same division of labour WithHostResolver is safe to export under.
	//
	// REFUSED RATHER THAN FALLEN BACK ON, which is the one place this diverges
	// from net/http on purpose. idnaASCIIFromURL swallows the error and keeps
	// the raw host, which is right for a transport whose next step is a dial
	// that will simply fail; it is wrong for a guard, whose next step is to make
	// a decision about a name. Vetting a string that nothing will dial is how a
	// guard approves one host and connects to another.
	//
	// The refusal is written here rather than through blockedDial: that
	// constructor names the DIAL path, and this fires a layer earlier. It is not
	// a BlockedAddressError either, because that type's contract is a host AND
	// the address it resolved to, and nothing has been resolved — there is no
	// address to carry, and its two renderings are both untrue of this case.
	// ErrBlockedAddress is the identity a caller matches on, and it is here.
	normalized, err := asciiHost(host)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a hostname this transport can resolve: %w",
			ErrBlockedAddress, host, err)
	}
	host = normalized

	// AN ADDRESS WRITTEN WHERE A HOSTNAME BELONGS IS REFUSED OUTRIGHT, before
	// anything is resolved.
	//
	// There is no traffic to lose. A legitimate atProto endpoint is always a
	// name: the handle specification forbids IP literals, and a DID document's
	// serviceEndpoint is an HTTPS URL with a hostname. So refusing the shape is
	// both cheaper and more total than classifying whatever it points at — and
	// classification is what cannot help here, since a caller-supplied literal
	// may well name a PUBLIC address and pass the check below on its way to a
	// destination this AppView has no business reaching.
	//
	// Before resolution rather than after, because resolving a literal asks a
	// question the URL already answered, and the answer would come back from a
	// resolver an attacker may influence.
	//
	// GATED ON THE HATCH, which is not a softening — it is what the hatch means.
	// allowPrivate says "this client is pointed at a developer-chosen address",
	// and every integration fixture in this tree is served from an httptest
	// listener, which is to say from a loopback literal:
	// internal/core/blobs/fetch_guard_test.go and
	// internal/core/blueskypost/service_test.go both drive THIS client at
	// 127.0.0.1:PORT with the hatch open. An ungated check would not merely fail
	// those suites, it would leave a dev environment unable to reach anything
	// local.
	//
	// netip.ParseAddr AND NOT net.ParseIP, because net.ParseIP returns nil for
	// any address carrying a ZONE — the `%eth0` in `fe80::1%eth0`, naming the
	// interface the address is scoped to. url.Parse does understand the form:
	// `http://[2600::1%25eth0]/` yields a Hostname of `2600::1%eth0`,
	// so a zoned literal was refused as "not a literal", handed to the resolver,
	// resolved locally with no DNS involved, its zone silently discarded, and
	// dialled. netip.ParseAddr accepts zones and every spelling ParseIP accepts,
	// so the switch closes the hole without narrowing anything.
	//
	// WHAT THIS COVERS: the dotted-quad and bracketed-IPv6 spellings, uppercase
	// hex, IPv4-mapped and now zoned forms, which is what the tests pin.
	//
	// WHAT THIS DOES NOT DO: it does not close the obfuscated encodings. BOTH
	// parsers refuse 0x7f.0.0.1, 2130706433, 127.1 and "127.0.0.1." — verified,
	// not assumed — so all four are not literals as far as this check is
	// concerned and reach the resolver. Resolver implementations differ on
	// whether they reject those spellings or normalize them to an address; any
	// returned address is still classified before it can be dialled.
	if !t.allowPrivate {
		if literal, err := netip.ParseAddr(host); err == nil {
			// AsSlice, because BlockedAddressError.IP is a net.IP and the
			// diagnostic is the whole reason that field exists. The zone is not
			// carried across — netip drops it here, as resolveHost drops it
			// there — and the address is what an operator reading the block
			// needs.
			return nil, &BlockedAddressError{Host: host, IP: literal.AsSlice(), literal: true}
		}
	}

	ips, err := t.resolveHost(req.Context(), host)
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
				return nil, &BlockedAddressError{Host: host, IP: ip}
			}
		}
	}

	// coves:allow-bare-client: this IS the guard handing off to its base transport, on the far side of the classification above
	resp, err := t.base.RoundTrip(req.WithContext(context.WithValue(req.Context(), vettedAddrsKey, ips)))

	// The handover, and it is cleared on BOTH outcomes: the base transport's own
	// contract is the one quoted above, so whether it answered or failed it has
	// already closed the body. Anything after this line must not close it again.
	ownsBody = false

	if err != nil {
		return nil, err
	}

	// AN ANNOUNCED length over the cap is refused without reading the body, and
	// the response is closed because a refused response still holds a connection.
	//
	// This is an OPTIMISATION AND NOT THE CONTROL. The header is chosen by the
	// same party as the body, so it is a hint at best — and against compression
	// it is not even that: http.Transport sends Accept-Encoding: gzip on its own,
	// transparently decompresses the reply, and DELETES Content-Length while
	// setting ContentLength to -1 when it does. So anyone wanting past this check
	// need only enable compression on their server. Do not read the wrapper below
	// as redundant with this branch; it is the other way round.
	//
	// A NEGATIVE LENGTH IS "UNKNOWN", NOT SUSPICIOUS. -1 is what every chunked
	// response reports and what every transparently decompressed one reports, so
	// refusing on it would refuse a large share of ordinary traffic. Unknown
	// means "rely on the wrapper", which is why the comparison is > and not !=.
	if resp.ContentLength > t.maxResponseBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %d bytes declared, %d allowed",
			ErrResponseTooLarge, resp.ContentLength, t.maxResponseBytes)
	}

	// The control proper. Wrapping the body the base transport RETURNS is what
	// makes the unit DECOMPRESSED bytes: by this point resp.Body is the gzip
	// reader's output, so the count is of what io.ReadAll will actually allocate.
	// A cap on bytes off the wire would let a thousand-to-one bomb deliver
	// gigabytes through a limit it never appeared to exceed.
	//
	// PER-HOP, NOT CUMULATIVE across a redirect chain, because RoundTrip runs
	// once per hop. That is adequate rather than a compromise: http.Client drains
	// a redirect body at roughly 2 KB before following it.
	resp.Body = newCappedBody(resp.Body, t.maxResponseBytes)
	return resp, nil
}

// newCappedBody wraps body with an allowance that Read can survive.
//
// THE CLAMP LIVES HERE AND IN THE CONSTRUCTOR, at the two boundaries where a
// number becomes an allowance, rather than being re-checked on the per-read hot
// path. What it buys is that `remaining` is positive by construction, so the
// arithmetic below never has to reason about a negative slice bound.
func newCappedBody(body io.ReadCloser, limit int64) *cappedBody {
	return &cappedBody{body: body, remaining: clampResponseCap(limit)}
}

// cappedBody fails the read once the body it wraps has delivered more than
// remaining bytes.
type cappedBody struct {
	body      io.ReadCloser
	remaining int64
	exceeded  bool
}

// Read delivers at most the remaining allowance and then fails.
//
// The mechanism is to READ ONE BYTE PAST THE ALLOWANCE and treat that byte's
// existence as the proof, which is what puts the boundary in the right place: a
// body of exactly the cap yields EOF on the extra read and arrives complete,
// while one byte more is seen and refused. Measuring after the fact — the
// obvious alternative — gets the same boundary and misses the point, since by
// then the whole body is in memory and the cap has protected nothing.
func (b *cappedBody) Read(p []byte) (int, error) {
	// Sticky, because the alternative is the truncation this cap exists to
	// avoid: a caller that reads again after the failure would otherwise be told
	// EOF once the underlying body ran out, and would take the short body it
	// already holds for a complete one.
	if b.exceeded {
		return 0, ErrResponseTooLarge
	}

	// The probe is sized under the comparison rather than as `remaining+1`
	// computed up front, and that ordering is the whole defence against
	// overflow: inside this branch remaining is BELOW len(p), which is an int,
	// so remaining+1 cannot exceed MaxInt and cannot wrap. The obvious spelling
	// wraps math.MaxInt64 to math.MinInt64 and panics on the slice bound — and
	// MaxInt64 is what someone writes for "no limit".
	if b.remaining < int64(len(p)) {
		probe := b.remaining + 1
		if probe < 0 {
			// Unreachable through newCappedBody, which clamps. It stands
			// between a hand-built wrapper and a negative slice bound, since a
			// panic here takes down whatever goroutine is reading a body.
			probe = 0
		}
		p = p[:probe]
	}

	n, err := b.body.Read(p)
	if int64(n) > b.remaining {
		// max(0, remaining) and not remaining, because io.Reader FORBIDS a
		// negative count and the standard library does not defend against one:
		// bytes.Buffer.ReadFrom panics with "reader returned negative count
		// from Read". Unreachable through newCappedBody for the same reason as
		// above; kept because the consequence of being wrong is a panic in a
		// caller doing the most ordinary thing there is with a response body.
		delivered := b.remaining
		if delivered < 0 {
			delivered = 0
		}

		// BOTH FIELDS, because they describe one state. This body will never
		// deliver another byte, so an allowance left positive is a count of
		// bytes that can never be spent — meaningless today only because Read
		// consults exceeded first, which is a read-order coincidence and not an
		// invariant. Zeroing it makes (exceeded, remaining>0) unrepresentable
		// rather than merely unreached.
		b.exceeded = true
		b.remaining = 0

		return int(delivered), ErrResponseTooLarge
	}
	b.remaining -= int64(n)
	return n, err
}

// Close closes the body underneath.
//
// DELEGATING IS LOAD-BEARING. http.Transport returns a connection to the idle
// pool when its body is closed, so a wrapper that implements Read and forgets
// Close leaks one connection per request while passing every functional test —
// bodies still arrive, caps still fire, and MaxIdleConns describes a pool
// nothing is ever returned to.
func (b *cappedBody) Close() error {
	return b.body.Close()
}

// DefaultMaxResponseBytes is the response-body cap a client gets when no option
// tightens it: 32 MiB, chosen to sit above every caller in this tree (the
// largest is 10 MB) so that adopting the shared client changes no existing
// behavior, while still bounding what a remote host can make this process
// allocate.
//
// A CALLER WITH ITS OWN, LARGER LIMIT MUST RAISE THIS EXPLICITLY. The trap is
// the image proxy: IMAGE_PROXY_MAX_SOURCE_SIZE_MB is operator-configurable, so
// an operator who sets it above 32 would find their own setting silently
// clamped by a constant in another package. Whoever wires that call site onto
// this client has to pass WithMaxResponseBytes from the configured value, the
// way blobs.NewBlobService raises the client timeout back to 30s rather than
// living with the shared default.
const DefaultMaxResponseBytes = 32 << 20

// Option configures the transport behind NewSSRFSafeHTTPClient.
type Option func(*ssrfSafeTransport)

// WithMaxResponseBytes caps how many bytes of a response body a caller can read
// before the read fails with ErrResponseTooLarge, replacing
// DefaultMaxResponseBytes.
//
// A cap of zero or less is not honoured; see clampResponseCap.
func WithMaxResponseBytes(n int64) Option {
	return func(t *ssrfSafeTransport) {
		t.maxResponseBytes = n
	}
}

// WithMaxIdleConnsPerHost sets how many idle connections the transport keeps
// per destination host, replacing net/http's DefaultMaxIdleConnsPerHost of 2.
//
// IT EXISTS SO A CALL SITE WITH ITS OWN POOL SETTINGS DOES NOT LOSE THEM WHEN
// IT ADOPTS THIS CLIENT. The base transport below already carries the
// MaxIdleConns and IdleConnTimeout every caller in this tree had set, so this
// was the one pool setting a conversion silently changed — the community
// consumer's .well-known fetch ran on 10 and would have dropped to 2 without
// anything failing, which is a throughput regression arriving as part of an SSRF
// fix and attributable to nothing.
//
// A value of zero or less is ignored rather than installed, so a caller that
// passes an unset config field gets net/http's default instead of a transport
// that pools nothing.
func WithMaxIdleConnsPerHost(n int) Option {
	return func(t *ssrfSafeTransport) {
		if n <= 0 {
			return
		}
		t.base.MaxIdleConnsPerHost = n
	}
}

// WithHostResolver replaces the transport's name lookup.
//
// IT IS A SEAM FOR CALLERS' OWN TESTS, and it exists because there is no
// hermetic way to make a hostname answer with a chosen address: the hermetic
// tiers block egress, and nothing in this tree can write /etc/hosts. Without
// it, a package that wires this client can only prove its guard by naming an IP
// literal — which the guard refuses on SHAPE, one branch earlier than the
// classification most call sites actually depend on. The aggregator's
// registration handler is the case that forced this: its domain validator
// already refuses every IP literal, so a literal-based test there proves
// nothing about the transport at all.
//
// IT CANNOT OPEN THE GUARD, and that is what makes exporting it safe. Whatever
// this function answers is classified by exactly the same pass a real DNS answer
// goes through, and the dial still goes only to addresses that survived it. The
// seam chooses what gets classified; it has no say in whether classification
// happens. A caller that wanted to reach a private address would use
// WithPrivateAddressesAllowed, which says so in its name.
//
// A nil lookup is ignored rather than installed, so a caller that passes one by
// accident gets the real resolver instead of a client that panics on its first
// request.
func WithHostResolver(lookup func(ctx context.Context, host string) ([]net.IP, error)) Option { // coves:allow-dns-seam: the seam's own declaration; a production CALL is what the rule is for
	return func(t *ssrfSafeTransport) {
		if lookup == nil {
			return
		}
		t.lookupIP = lookup
	}
}

// WithPrivateAddressesAllowed disables the address guard entirely, for a client
// a developer has deliberately pointed at their own machine.
//
// IT EXISTS TO BE READ AT THE CALL SITE. The byte ceiling already had a
// self-documenting name while the setting that turns the guard OFF was an
// unlabelled positional boolean, which is exactly backwards — the more dangerous
// switch was the one wearing no label. The old spelling — the constructor called
// with a bare `true` — said nothing: a reader had to open this file to learn that
// the argument was the difference between a guarded client and an unguarded one.
//
// ONE NAME FOR SEVERAL DECISIONS is what makes a named option worth more here
// than at any other setting on this transport. allowPrivate is read in two
// separate places in RoundTrip — the literal refusal, which covers the dotted-
// quad, bracketed-IPv6 and zoned spellings in one netip.ParseAddr, and the
// classification pass over the resolved answers — so the boolean is one token
// standing for gates whose spelling names none of them. The option names the
// state they share: this client has the hatch open.
//
// It is also the thing a regression fence can find. `true` is not greppable;
// this identifier is, which is how "which call sites disable the guard" stays an
// answerable question as call sites are added.
//
// ONE-WAY, so it composes: it only ever opens the hatch, and no unrelated option
// can close a hatch this one opened, whatever order the options run in. A client
// built with NO options stays guarded because the constructor's struct literal
// does not set allowPrivate at all and its zero value is false — unlike the byte
// cap, which needs an explicit default written there for the same reason: what a
// caller gets by omission has to be the safe value.
func WithPrivateAddressesAllowed() Option { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(t *ssrfSafeTransport) { t.allowPrivate = true }
}

// PrivateAddressOptions returns the options a caller holding an allow-private
// boolean should pass to NewSSRFSafeHTTPClient: the hatch option when the
// boolean is set, and NOTHING when it is not.
//
// # WHY THIS IS A FUNCTION AND NOT AN `if` IN WIRING
//
// It looks like a conditional worth inlining, and inlining it would delete the
// only test coverage the production branch has.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// running T0+T1+T2 — takes the PERMISSIVE branch at every call site that holds
// such a boolean. A green merge gate therefore says nothing whatsoever about
// whether production is guarded: the guarded branch is evaluated in exactly one
// place in this repository, and that place is a unit test against this function.
//
// An inline `if cfg.IsDevEnv { ... }` in wiring is reachable only by standing up
// that wiring with a production config, and nothing in this tree does that. As a
// pure function the decision is testable without wiring, without a config and
// without an environment, which is the only reason the branch production
// actually runs is tested at all. Do not inline it back.
//
// # FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT
//
// Not "options that are safe" — no options. What a guarded caller gets is
// exactly the constructor's own struct literal, untouched, which is a claim a
// reader can check in one glance rather than one that has to be re-argued
// against whatever the slice holds this month.
//
// So an edit that appends a diagnostic option here, or that returns a
// one-element slice holding a no-op "explicitly deny" closure, is a breaking
// change even though it changes no behaviour: it moves the branch CI never runs
// from "provably applies nothing" to "applies something believed harmless".
// TestPrivateAddressOptions_ReturnsZeroOptionsWhenPrivateAddressesAreDisallowed
// asserts the exact length and will fail on both. That is deliberate; the answer
// is not to relax it.
//
// The slice is built fresh per call rather than shared from a package-level var,
// because callers append to it — the image proxy passes its operator-configured
// WithMaxResponseBytes alongside — and a shared backing array would let two call
// sites write over each other's options.
func PrivateAddressOptions(allowPrivate bool) []Option {
	if !allowPrivate {
		return nil
	}
	return []Option{WithPrivateAddressesAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// joinDialErrors turns the per-address dial failures into the ONE error the
// dial returns, and it owns both branches so that "what a caller can learn from
// a failed dial" is decided in a single place.
//
// # ONE ERROR IS RETURNED BARE
//
// Joining it would be aggregation with nothing to aggregate, and it costs the
// timeout signal for the reason spelled out at the call site. It also puts one
// more wrapper between a caller and the concrete type it may be matching on,
// for no information gained.
//
// # THE AGGREGATE HAS TO ANSWER THE SAME QUESTIONS ITS MEMBERS DO
//
// errors.Join alone returns an *errors.joinError, which implements Unwrap()
// []error and NOTHING ELSE — so the bare-return fix above preserved Timeout()
// for a host with one address and left it severed for a host with two. An
// ordinary dual-stack host has an A record and an AAAA record, so the
// aggregation branch is the COMMON case, not the corner one, and a caller that
// retries on timeouts stopped seeing them exactly there.
//
// BOTH METHODS OR NEITHER. url.Error.Timeout needs only `interface{ Timeout()
// bool }`, but `if ne, ok := err.(net.Error); ok && ne.Timeout()` — the shape
// retry and circuit-breaker logic is actually written in — needs Temporary()
// too, and an assertion to net.Error fails outright without it. Implementing
// half the interface would fix half the callers and look like it had fixed all
// of them.
//
// # ALL, NOT ANY
//
// The aggregate reports a property only when EVERY member reports it, because
// the caller's question is about the destination as a whole. A host whose IPv6
// address timed out while its IPv4 address was REFUSED has given a definite
// answer on one of them; calling that a timeout tells a retry loop to keep
// waiting on something that already said no. A member that is not a net.Error
// at all answers false for the same reason — an aggregate cannot claim a
// property of a member that cannot report it.
//
// Inside, membership is tested with errors.As rather than a direct assertion,
// and that asymmetry with our own callers is deliberate: what the dialer hands
// back is a *net.OpError wrapping the real cause, so walking the chain is how
// the members are read honestly. Our callers cannot do that — the stdlib
// asserts directly — which is precisely why this type exists to answer for
// them.
func joinDialErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	return &dialAggregateError{errs: errs, joined: errors.Join(errs...)}
}

// dialAggregateError is errors.Join's aggregation with net.Error's answers.
type dialAggregateError struct {
	errs []error

	// joined carries the message and nothing else. Reusing errors.Join for the
	// text keeps the operator-facing output byte-identical to what the plain
	// join produced, so this change adds a capability without editing a string
	// anyone may be reading in a log.
	joined error
}

func (e *dialAggregateError) Error() string { return e.joined.Error() }

// Unwrap is the multi-error form, which is what lets errors.Is and errors.As
// walk to every per-address failure. Dropping it would keep the message and
// lose the tree.
func (e *dialAggregateError) Unwrap() []error { return e.errs }

func (e *dialAggregateError) Timeout() bool {
	return e.everyMemberSatisfies(net.Error.Timeout)
}

func (e *dialAggregateError) Temporary() bool {
	return e.everyMemberSatisfies(net.Error.Temporary)
}

// everyMemberSatisfies reports whether every joined error is a net.Error for
// which want holds. An empty aggregate answers false: joinDialErrors never
// builds one, and "vacuously true" is the wrong default for a question a caller
// acts on.
func (e *dialAggregateError) everyMemberSatisfies(want func(net.Error) bool) bool {
	if len(e.errs) == 0 {
		return false
	}
	for _, err := range e.errs {
		var netErr net.Error
		if !errors.As(err, &netErr) || !want(netErr) {
			return false
		}
	}
	return true
}

// clampResponseCap maps a cap that is not a cap onto the default.
//
// ZERO AND NEGATIVE ARE BOTH ARRIVALS, NOT CHOICES. Zero is what an unset
// struct field, a missing config key or an unparsed environment variable
// arrives as, and honouring it refuses every non-empty response there is —
// which reads at the call site as "this remote host is broken", for every host.
// A negative cap is a typo, and honouring it used to make Read return n = -1.
//
// FALLING BACK TO THE DEFAULT RATHER THAN REFUSING THE OPTION, because a cap is
// a safety limit and the failure mode of getting it wrong should be a working
// client with a conservative bound, not a client that refuses everything or a
// constructor that panics on a value a config file supplied.
//
// There is no upper clamp, and none is needed: cappedBody.Read no longer
// computes remaining+1 anywhere it could overflow, so math.MaxInt64 is an
// ordinary — if useless — allowance rather than a panic. Do not add a magic
// upper bound to "fix" that; fix the arithmetic if it ever regresses.
func clampResponseCap(n int64) int64 {
	if n <= 0 {
		return DefaultMaxResponseBytes
	}
	return n
}

// NewSSRFSafeHTTPClient creates an HTTP client with SSRF protections
func NewSSRFSafeHTTPClient(opts ...Option) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &ssrfSafeTransport{
		base: &http.Transport{ // coves:allow-bare-client: the base transport the guard wraps; its DialContext below only reaches addresses RoundTrip vetted
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
					return nil, blockedDial("refusing to dial %s with no vetted address "+
						"(the SSRF-safe transport was bypassed)", addr)
				}
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, blockedDial("cannot read a port from %q: %w", addr, err)
				}
				// EVERY failure is kept, not just the last one. A host with both
				// an A and an AAAA record is the ordinary case, and the two
				// commonly fail differently — IPv6 unreachable on a v4-only host,
				// connection refused on the v4 address. Keeping one symptom sends
				// whoever is debugging a federation failure after a single address
				// with nothing to say another was tried at all.
				//
				// The addresses appear in the joined message because the stdlib
				// dial error embeds the one it tried, and naming them here is not
				// the oracle the classification refusal above avoids: these are
				// addresses ALREADY ACCEPTED, reported to an in-process caller,
				// rather than an answer to "what does the name you gave me resolve
				// to inside this network".
				var dialErrs []error
				for _, ip := range vetted {
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					dialErrs = append(dialErrs, dialErr)
				}

				// The loop's own contract, made total rather than borrowed from
				// twenty lines up. errors.Join of nothing is nil, so falling out of
				// an attempt-free loop would return (nil, nil) — a connection that
				// does not exist and no error explaining it, which is not a result
				// net/http is written to survive.
				//
				// THIS IS LATENT, NOT LIVE: an empty vetted slice cannot reach here
				// through the public API, because the guard above refuses it, so
				// the loop always runs at least once today. What this closes is the
				// edit that moves or weakens that guard without noticing that the
				// loop below was depending on it.
				if len(dialErrs) == 0 {
					return nil, blockedDial("no vetted address was attempted for %s", addr)
				}

				// ONE ERROR IS RETURNED BARE. Joining it would be aggregation
				// with nothing to aggregate, and it costs the timeout signal:
				// errors.Join returns a *errors.joinError, which implements
				// Unwrap() []error and nothing else, while url.Error.Timeout —
				// the method every caller of this client reaches, since
				// http.Client wraps every RoundTrip error in a *url.Error — is a
				// DIRECT TYPE ASSERTION rather than errors.As. Same for the
				// `if ne, ok := err.(net.Error); ok && ne.Timeout()` that retry
				// and circuit-breaker logic is built on. So a join over a lone
				// dial failure leaves the information in the chain and out of
				// reach, and a caller that cannot tell a timeout from a refusal
				// either retries what it should not or gives up on what it
				// should.
				//
				// The join stays for the genuine multi-address case, which is
				// what it was added for: a host with both an A and an AAAA
				// record commonly fails differently on each, and an operator
				// needs both symptoms. joinDialErrors owns both branches.
				return nil, joinDialErrors(dialErrs)
			},
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		// allowPrivate is NOT set here, and its zero value is the whole point:
		// a client built with no options is GUARDED. The hatch is reachable only
		// through WithPrivateAddressesAllowed, so "this client can reach private
		// addresses" is a phrase that appears at the call site or nowhere.
		//
		// This replaced a positional boolean. `NewSSRFSafeHTTPClient(true)` said
		// nothing at a call site about which of the guard's three refusals it was
		// switching off, and a reader had to open this file to learn that the
		// argument was the difference between a guarded client and an unguarded
		// one.

		// Before the options, so a client built without one is capped at the
		// default rather than at a zero value — which would refuse every
		// response there is.
		maxResponseBytes: DefaultMaxResponseBytes,
	}

	for _, opt := range opts {
		opt(transport)
	}

	// AFTER the options, so a cap outside the usable range is corrected wherever
	// it came from. It also keeps the ContentLength comparison in RoundTrip
	// sane: that branch reads this field directly, so a zero here would refuse
	// every response that declared a length before a byte was read.
	transport.maxResponseBytes = clampResponseCap(transport.maxResponseBytes)

	return &http.Client{ // coves:allow-bare-client: this IS the guarded client being constructed; the ssrfSafeTransport below is what makes it safe
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
