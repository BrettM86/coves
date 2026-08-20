package validation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"Coves/tests/domaincorpus"
)

// NormalizeDomain is the shared half of a two-mechanism defence, and this file
// is its whole contract.
//
// Two call sites build a URL by concatenating a caller-supplied domain into it:
// the aggregator's registration handler (`https://` + domain +
// `/.well-known/atproto-did`, reachable with no credential) and the community
// consumer's DID-document fetch (`https://` + domain + `/.well-known/did.json`,
// reachable by anyone federated). String concatenation into a URL is the whole
// vulnerability: every part of a URL that comes after the host can be smuggled
// in through the host, because the parser does not know the concatenation was
// supposed to stop.
//
// # WHY A GUARDED DIALLER IS NOT ENOUGH, WHICH IS WHY THIS FUNCTION EXISTS
//
// The SSRF-safe transport refuses private ADDRESSES. It has no opinion about
// paths, and `internal-admin/v1/secrets?x=y#` does not name a private address —
// it names whatever `internal-admin` resolves to, which on a corporate resolver
// or a split-horizon DNS is a public-looking answer, and then requests
// `/v1/secrets?x=y` from it. Wiring the guard and skipping this validator closes
// the address half and leaves the URL-structure half wide open.
//
// # WHY THE CASES LIVE IN tests/domaincorpus
//
// So the two call sites cannot drift. See that package's doc comment.

func TestNormalizeDomain_RefusesAnythingThatIsNotAHostname(t *testing.T) {
	t.Parallel()

	for _, tt := range domaincorpus.Invalid() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			normalized, err := NormalizeDomain(tt.Domain)

			require.ErrorIsf(t, err, ErrDomainInvalid,
				"NormalizeDomain(%q) must refuse this with ErrDomainInvalid; accepting it lets a "+
					"caller aim an AppView .well-known fetch at %q",
				tt.Domain, tt.Domain)
			require.Emptyf(t, normalized,
				"NormalizeDomain(%q) refused but still returned %q; a caller that drops the error "+
					"must not come away holding something it can concatenate into a URL",
				tt.Domain, normalized)
		})
	}
}

// The other half of the allowlist: refusing everything is not a fix.
func TestNormalizeDomain_AcceptsHostnames(t *testing.T) {
	t.Parallel()

	for _, tt := range domaincorpus.Valid() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			normalized, err := NormalizeDomain(tt.Domain)

			require.NoErrorf(t, err,
				"NormalizeDomain(%q) refused a hostname; the allowlist has to admit the domains "+
					"real aggregators and real federated instances use, or both call sites are closed",
				tt.Domain)
			require.Equalf(t, tt.Want, normalized, "NormalizeDomain(%q) canonical form", tt.Domain)
		})
	}
}

// The final-label rule, stated in one place and in both directions.
//
// It lives in its own test rather than as more rows in the corpus because a
// table cannot show a RULE — it shows a set of inputs, and a reader checking
// whether the rule is sound has to reconstruct it from forty scattered cases.
// That reconstruction is exactly what went wrong once: the rule "the final label
// contains at least one letter" reads as "this is a TLD and not a number", and
// it is not — `0x1` is a number that contains a letter, and the corpus's
// `0x7f.0.0.1` row hid it, because that name is refused on its `1` and never on
// its `0x7f`.
//
// The rule that holds is the positive one, and both halves are load-bearing:
//
//   - ALPHABETIC, so no spelling of an integer can satisfy it. Not "contains a
//     letter", which every hex octet does.
//   - OR PUNYCODE (`xn--`), because an internationalised TLD reaches a resolver
//     as ASCII with digits in it — `example.xn--p1ai` is Russia's `.рф` and is a
//     real registration both call sites must accept.
//
// Nothing else needs admitting: the IANA root has no TLD that is neither
// all-alphabetic nor `xn--`-prefixed.
//
// THIS TEST MUST SURVIVE THE MOVE INTACT. A
// promotion that carried the corpus across but left this behind would restore
// the hex bypass with the suite green.
func TestNormalizeDomain_TheFinalLabelIsAlphabeticOrPunycode(t *testing.T) {
	t.Parallel()

	refused := []struct {
		name   string
		domain string
	}{
		// The three hex bypasses, restated here so this test fails on its own
		// if the rule is loosened later.
		{name: "hex final octet", domain: "127.0.0.0x1"},
		{name: "hex final octet with omitted octets", domain: "127.0.0x1"},
		{name: "hex first and final octets", domain: "0x7f.0.0.0x1"},

		// The general case the three above are instances of. No demonstrated
		// resolver treats `example.a1` as an address — this row is here because
		// the RULE is "alphabetic or punycode", and a fix that special-cased the
		// string `0x` would satisfy every row above while leaving the proxy in
		// place for whatever spelling comes next.
		{name: "alphanumeric final label", domain: "example.a1"},
		{name: "digit-leading alphanumeric final label", domain: "example.1a"},

		// Already covered by the corpus; kept so this test states the whole rule.
		{name: "all-numeric final label", domain: "example.123"},

		// `xn--` has to be a PREFIX, not a substring. A label that merely
		// contains it is not punycode and must not inherit the exemption.
		{name: "xn-- in the middle of the final label", domain: "example.axn--p1ai"},
	}

	for _, tt := range refused {
		t.Run("refused/"+tt.name, func(t *testing.T) {
			t.Parallel()

			normalized, err := NormalizeDomain(tt.domain)

			require.ErrorIsf(t, err, ErrDomainInvalid,
				"NormalizeDomain(%q) accepted a final label that is neither alphabetic nor punycode. "+
					"Letter-bearing was only ever a proxy for 'not a number', and a hex octet defeats it: "+
					"%q reaches a resolver as an address, which is the whole class this validator exists "+
					"to refuse", tt.domain, tt.domain)
			require.Emptyf(t, normalized,
				"NormalizeDomain(%q) refused but still returned %q", tt.domain, normalized)
		})
	}

	// The other half. A rule tightened until it refuses everything is not a fix,
	// and each of these is a shape a real instance runs under.
	accepted := []struct {
		name   string
		domain string
		want   string
	}{
		{name: "an ordinary TLD", domain: "example.com", want: "example.com"},
		{name: "a multi-part public suffix", domain: "sub.example.co.uk", want: "sub.example.co.uk"},
		{name: "a long alphabetic TLD", domain: "aggregator.museum", want: "aggregator.museum"},
		// The punycode exemption, and the only reason it exists: this is `.рф`.
		{name: "a punycode TLD carrying digits", domain: "example.xn--p1ai", want: "example.xn--p1ai"},
		// Punycode in a non-final label needs no exemption — the final label is
		// `com` — but it must go on working, and it is the more common shape.
		{name: "a punycode label under an alphabetic TLD", domain: "xn--n3h.example.com", want: "xn--n3h.example.com"},
		// Digits are legal anywhere but the last label, per RFC 1123.
		{name: "a digit-leading label under an alphabetic TLD", domain: "1.example.com", want: "1.example.com"},
		{name: "uppercase, canonicalised rather than refused", domain: "EXAMPLE.COM", want: "example.com"},
		{name: "uppercase punycode TLD", domain: "example.XN--P1AI", want: "example.xn--p1ai"},
	}

	for _, tt := range accepted {
		t.Run("accepted/"+tt.name, func(t *testing.T) {
			t.Parallel()

			normalized, err := NormalizeDomain(tt.domain)

			require.NoErrorf(t, err,
				"NormalizeDomain(%q) refused a hostname a real instance runs under. Tightening the "+
					"final-label rule to close the hex spellings must not close the TLDs that carry digits "+
					"legitimately — punycode is how every internationalised domain reaches a resolver", tt.domain)
			require.Equalf(t, tt.want, normalized, "NormalizeDomain(%q) canonical form", tt.domain)
		})
	}
}

// The length limits, as boundaries rather than as "something long". A cap that
// is only tested with an obviously oversized input pins no number at all.
func TestNormalizeDomain_BoundsTheNameLength(t *testing.T) {
	t.Parallel()

	const maxName = 253 // RFC 1035's 255-octet wire form, written out as text
	const maxLabel = 63

	atLimit := nameOfLength(t, maxName)
	overLimit := nameOfLength(t, maxName+1)

	normalized, err := NormalizeDomain(atLimit)
	require.NoErrorf(t, err,
		"a %d-character name is the longest DNS can carry and must be accepted", maxName)
	require.Equal(t, atLimit, normalized, "a name at the length limit is returned unchanged")

	_, err = NormalizeDomain(overLimit)
	require.ErrorIsf(t, err, ErrDomainInvalid,
		"a %d-character name exceeds the %d-character limit and cannot resolve, so it must be "+
			"refused here rather than handed to a resolver", maxName+1, maxName)

	longestLabel := strings.Repeat("a", maxLabel) + ".com"
	normalized, err = NormalizeDomain(longestLabel)
	require.NoErrorf(t, err, "a %d-character label is the longest DNS allows and must be accepted", maxLabel)
	require.Equal(t, longestLabel, normalized, "a label at the length limit is returned unchanged")

	_, err = NormalizeDomain(strings.Repeat("a", maxLabel+1) + ".com")
	require.ErrorIsf(t, err, ErrDomainInvalid,
		"a %d-character label exceeds the %d-character limit", maxLabel+1, maxLabel)
}

// nameOfLength builds an ordinary hostname of exactly n characters: 63-character
// labels until the remainder fits in one more. The length boundary above is only
// a boundary if the accepted and refused cases differ in nothing but the count,
// which is what this guarantees.
func nameOfLength(t *testing.T, n int) string {
	t.Helper()
	require.Greaterf(t, n, 64, "nameOfLength cannot build a %d-character name with two labels", n)

	var labels []string
	for n > 64 {
		labels = append(labels, strings.Repeat("a", 63))
		n -= 64 // the label plus the dot that follows it
	}
	labels = append(labels, strings.Repeat("a", n))
	return strings.Join(labels, ".")
}
