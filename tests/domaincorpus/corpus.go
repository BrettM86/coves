// Package domaincorpus holds the one table of hostname-validation cases that
// every call site building a URL out of a caller-supplied domain is tested
// against.
//
// # WHY THIS IS A PACKAGE AND NOT A TABLE IN ONE TEST FILE
//
// There are now two such call sites — the aggregator's registration handler and
// the community consumer's `.well-known/did.json` fetch — and they were written
// eighteen months and one package apart. The consumer's line
//
//	didDocURL := fmt.Sprintf("https://%s/.well-known/did.json", domain)
//
// is byte-identical in shape to the one the aggregator fix closed, and the only
// reason it was still open is that `normalizeDomain` was unexported in
// `internal/api/handlers/aggregator` and therefore unreachable from
// `internal/atproto/jetstream` even by someone who noticed.
//
// A shared validator fixes the reachability. It does not fix the coverage: two
// tables maintained side by side drift the moment someone adds a payload to one
// of them, and the site that did not get the row is the site that stays
// vulnerable. So the payloads live here, and a new call site is tested against
// every case both existing sites are.
//
// # WHY IT IS UNDER tests/ AND NOT NEXT TO THE VALIDATOR
//
// Go cannot import a _test.go file across packages, so a corpus shared by three
// packages' tests has to be an ordinary package. Putting it under tests/ — with
// testkit and fixtures, which internal tests already import — keeps it out of
// the production build, and keeps it inside scripts/test-audit.sh's test-code
// scope so the host-literal payloads below still have to carry their markers.
package domaincorpus

// InvalidCase is a domain that must be refused, with the name its subtest runs
// under.
type InvalidCase struct {
	Name   string
	Domain string
}

// ValidCase is a domain that must be accepted, and the canonical form it
// normalises to.
type ValidCase struct {
	Name   string
	Domain string
	Want   string
}

// Invalid returns every domain a hostname validator must refuse.
//
// A fresh slice per call, because callers iterate it in parallel subtests and a
// shared backing array is one `append` away from two tests writing over each
// other.
func Invalid() []InvalidCase {
	return []InvalidCase{
		// The three confirmed injections, first, because they are the reason
		// this validation exists. Each was verified against the registration
		// handler before it was closed:
		//
		//	internal-admin/v1/secrets?x=y#   fetches /v1/secrets?x=y from internal-admin
		//	evil.com@internal-host           fetches from internal-host; evil.com is userinfo
		//	127.0.0.1:5432                   fetches from a port on the loopback interface
		//
		// The trailing `#` in the first is what makes this more than an SSRF to
		// one fixed path: it turns the well-known suffix into a fragment, which
		// is never sent, so the attacker chooses the path AND the query as well
		// as the host. THIS IS THE HALF A GUARDED DIALLER CANNOT CLOSE — a safe
		// transport has an opinion about addresses and none about paths.
		{Name: "path and query smuggled past a fragment", Domain: "internal-admin/v1/secrets?x=y#"},
		{Name: "userinfo hiding the real host", Domain: "evil.com@internal-host"},
		{Name: "loopback address and port", Domain: "127.0.0.1:5432"}, // coves:allow-host-literal: the injection payload IS the literal; it is asserted on, never dialled

		// IPv4 in the spellings net.ParseIP does not recognise but a resolver
		// does. An "is it an IP" check passes all three through; asking "is it
		// a hostname" refuses all three, which is the argument for the positive
		// allowlist in one table.
		{Name: "IPv4 as a single decimal", Domain: "2130706433"},
		{Name: "IPv4 with omitted octets", Domain: "127.1"},
		{Name: "IPv4 in hex", Domain: "0x7f.0.0.1"},

		// THE HEX SPELLINGS THE ROW ABOVE ONLY LOOKS LIKE IT COVERS.
		//
		// `0x7f.0.0.1` is refused because its FINAL label is `1`, which carries
		// no letter — the hex in the first label is incidental and nothing in
		// the validator ever looks at it. Move the hex to the last position and
		// a "contains a letter" rule waves the name through, because `0x1`
		// contains an `x`:
		//
		//     127.0.0.0x1     accepted, resolves to 127.0.0.1
		//     127.0.0x1       accepted, resolves to 127.0.0.1
		//     0x7f.0.0.0x1    accepted, resolves to 127.0.0.1
		//
		// Verified by probe on a cgo-resolver machine. Under the pure-Go
		// resolver these NXDOMAIN, so a CGO_ENABLED=0 production build fails
		// closed by accident rather than by design.
		{Name: "hex final octet", Domain: "127.0.0.0x1"},
		{Name: "hex final octet with omitted octets", Domain: "127.0.0x1"},
		{Name: "hex first and final octets", Domain: "0x7f.0.0.0x1"},

		// Names that resolve to something internal without looking like an
		// address at all.
		{Name: "localhost", Domain: "localhost"},
		{Name: "a bare single-label name", Domain: "example"},
		{Name: "an internal short name", Domain: "internal-admin"},

		// Numeric TLDs. `example.123` has the two labels and the dot; the letter
		// requirement on the last label is what separates it from a hostname,
		// and is the same rule that catches 127.1 above.
		{Name: "numeric TLD", Domain: "example.123"},

		// URL syntax that must not survive into the host position.
		{Name: "https scheme", Domain: "https://example.com"},
		{Name: "http scheme", Domain: "http://example.com"},
		{Name: "protocol-relative with a path", Domain: "//example.com/x"},
		{Name: "userinfo", Domain: "user@example.com"},
		{Name: "userinfo with a password", Domain: "user:pass@example.com"},
		{Name: "explicit port", Domain: "example.com:8443"},
		{Name: "trailing slash", Domain: "example.com/"},
		{Name: "path", Domain: "example.com/.well-known/atproto-did"},
		{Name: "query", Domain: "example.com?x=y"},
		{Name: "fragment", Domain: "example.com#x"},
		{Name: "percent-encoded dot", Domain: "example%2ecom"},

		// IPv6, both spellings. Neither is a hostname and the bracketed form is
		// the one that would otherwise slot straight into a URL authority.
		{Name: "IPv6 loopback", Domain: "::1"},
		{Name: "IPv6 loopback in brackets", Domain: "[::1]"},
		{Name: "IPv6 link-local in brackets", Domain: "[fe80::1]"},

		// Whitespace and control characters. The newline is the one that
		// matters most: a name carrying CR or LF is how a header gets split.
		{Name: "internal space", Domain: "exam ple.com"},
		{Name: "leading space", Domain: " example.com"},
		{Name: "trailing space", Domain: "example.com "},
		{Name: "only whitespace", Domain: "   "},
		{Name: "tab", Domain: "exam\tple.com"},
		{Name: "newline", Domain: "example.com\n"},
		{Name: "carriage return and newline", Domain: "example.com\r\nHost: internal\r\n"},
		{Name: "NUL", Domain: "exam\x00ple.com"},

		// Non-ASCII. A unicode domain is a real thing, but it is a hostname only
		// once it has been punycoded, and doing that conversion here would mean
		// the validator quietly changing which host is contacted.
		{Name: "non-ASCII", Domain: "exämple.com"},
		{Name: "zero-width space", Domain: "exam\u200bple.com"},

		// Label and dot structure.
		{Name: "empty", Domain: ""},
		{Name: "a lone dot", Domain: "."},
		{Name: "empty label", Domain: "example..com"},
		{Name: "leading dot", Domain: ".example.com"},
		{Name: "underscore in a label", Domain: "exam_ple.com"},
		{Name: "label starting with a hyphen", Domain: "-example.com"},
		{Name: "label ending with a hyphen", Domain: "example-.com"},

		// The trailing dot is a legitimate way to write a fully qualified name,
		// and this refuses it anyway. Two reasons, both about the allowlist
		// being an allowlist: it gives one domain two spellings, which is the
		// shape every allowlist bypass takes, and the root-anchored form is
		// handled inconsistently by TLS stacks deciding whether it matches a
		// certificate. No atproto client sends it. If that turns out to be
		// wrong, the fix is to strip it in the validator — which is where the
		// canonical form is decided — and move this case into Valid().
		{Name: "trailing dot", Domain: "example.com."},
	}
}

// Valid returns the hostnames a validator must go on accepting, with their
// canonical forms. Refusing everything is not a fix.
func Valid() []ValidCase {
	return []ValidCase{
		{Name: "two labels", Domain: "example.com", Want: "example.com"},
		{Name: "multi-label under a multi-part TLD", Domain: "sub.example.co.uk", Want: "sub.example.co.uk"},
		{Name: "the shortest real shape", Domain: "a.co", Want: "a.co"},
		{Name: "hyphen inside a label", Domain: "my-aggregator.example.com", Want: "my-aggregator.example.com"},
		// Leading digits in a label are legal per RFC 1123 and common in
		// practice; only the LAST label has to carry a letter.
		{Name: "label starting with a digit", Domain: "1.example.com", Want: "1.example.com"},
		{Name: "long TLD", Domain: "aggregator.museum", Want: "aggregator.museum"},

		// Punycode is how an internationalised domain arrives at a resolver, and
		// it is plain letters, digits and hyphens — so the allowlist admits it
		// without knowing what it is.
		{Name: "punycode label", Domain: "xn--n3h.example.com", Want: "xn--n3h.example.com"},
		{Name: "punycode TLD", Domain: "example.xn--p1ai", Want: "example.xn--p1ai"},

		// Case is canonicalised rather than refused. DNS does not distinguish
		// these, so refusing would turn away a legitimate registration over
		// nothing, and passing the case through would give one domain several
		// spellings in the logs.
		{Name: "uppercase", Domain: "EXAMPLE.COM", Want: "example.com"},
		{Name: "mixed case", Domain: "Sub.Example.CoM", Want: "sub.example.com"},
	}
}

// InjectionPayloads returns the subset of Invalid() that a URL-building call
// site must refuse BEFORE it touches an HTTP client.
//
// Every entry parses as a URL once concatenated, so `http.NewRequestWithContext`
// succeeds and the client is reached. That is the point: an input Go's URL
// parser rejects would never touch a transport even with no validation at all,
// and would prove nothing about ordering.
func InjectionPayloads() []InvalidCase {
	return []InvalidCase{
		{Name: "path and query smuggled past a fragment", Domain: "internal-admin/v1/secrets?x=y#"},
		{Name: "userinfo hiding the real host", Domain: "evil.com@internal-host"},
		{Name: "loopback address and port", Domain: "127.0.0.1:5432"}, // coves:allow-host-literal: the injection payload IS the literal; the transport refuses to dial it
		{Name: "IPv4 as a single decimal", Domain: "2130706433"},
		{Name: "localhost", Domain: "localhost"},
		{Name: "internal short name", Domain: "internal-admin"},
	}
}
