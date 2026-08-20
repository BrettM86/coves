// Package validation holds predicates shared by every call site that builds a
// URL, a query or a command out of something a caller supplied.
//
// # WHY THE HOSTNAME VALIDATOR LIVES HERE AND NOT NEXT TO A CALL SITE
//
// It started as `normalizeDomain`, unexported, in
// internal/api/handlers/aggregator. That is where the first injection was found
// and closed, and being unexported is the reason the second call site — the
// community consumer's `.well-known/did.json` fetch, whose URL construction is
// byte-identical in shape — stayed open through that fix. Someone who noticed
// could not have called it.
//
// A security predicate that two packages need is a package, not a copy. The
// corpus it is tested against is shared for the same reason and lives at
// tests/domaincorpus.
package validation

import (
	"errors"
	"strings"
)

const (
	// maxDomainLength is RFC 1035's 255-octet wire form written out as text: the
	// octet count includes a length byte for the first label and a zero byte for
	// the root, neither of which appears in the dotted string. A longer name
	// cannot be carried by DNS, so accepting one only means handing a resolver
	// something that is guaranteed to fail.
	maxDomainLength = 253

	// maxLabelLength is the single-octet length prefix DNS puts in front of each
	// label, which leaves 63 characters for the label itself.
	maxLabelLength = 63

	// punycodePrefix marks a label as the ASCII encoding of a non-ASCII name.
	// It is the one reason a top-level domain may carry digits — `example.рф`
	// reaches a resolver as `example.xn--p1ai` — and so the one exemption from
	// the all-alphabetic rule below.
	punycodePrefix = "xn--"
)

// ErrDomainInvalid is returned when a caller-supplied domain is not a hostname.
//
// It is a distinct sentinel rather than a formatted error because TWO separate
// mechanisms refuse most of the same inputs — this validator, and the
// SSRF-guarded transport wired in underneath the call sites that use it — so a
// test that only asserts "an error came back" cannot tell which one fired. A
// build where the validator had been quietly deleted and the guard caught the
// residue would look identical, and the residue is NOT the same set: the guard
// sees an address, and `internal-admin/v1/secrets?x=y#` is a *path* injection
// against whatever `internal-admin` resolves to.
var ErrDomainInvalid = errors.New("domain is not a valid hostname")

// NormalizeDomain checks that domain is a hostname and returns its canonical
// form.
//
// This is a POSITIVE allowlist on DNS shape, not a blocklist on addresses, and
// that is the whole design. A blocklist has to start by asking "is this an IP",
// and net.ParseIP answers no for 2130706433, 127.1 and 0x7f.0.0.1 — every one
// of which a resolver turns into loopback. Asking instead "is this a hostname"
// refuses all three for the same reason it refuses `localhost`: they are not
// two-label names ending in a TLD.
//
// The accepted shape:
//
//   - two or more labels separated by single dots, no leading or trailing dot
//   - each label 1-63 characters of ASCII letters, digits and hyphens, and
//     neither first nor last character a hyphen
//   - the final label is entirely alphabetic, or punycode (so `127.1`,
//     `example.123` and `127.0.0.0x1` are refused while `example.xn--p1ai` is
//     not)
//   - 253 characters total at most
//
// Everything a URL can carry besides the host is therefore refused by
// construction: no scheme remnant, no userinfo, no port, no path, no query, no
// fragment, no whitespace, no control characters, no non-ASCII.
//
// The returned string is the domain lowercased. DNS is case-insensitive, so
// refusing EXAMPLE.COM would turn a legitimate registration away over nothing;
// leaving the case alone would give one domain several spellings in the logs
// and in anything that later compares them. Canonicalising is the third option
// and the only one that costs nothing.
//
// IT HAS NO OPINION ABOUT WHAT A CLIENT WRAPPED THE HOSTNAME IN. `https://
// example.com` is refused here — it is a URL, not a hostname — while the
// aggregator's registration handler goes on accepting it, because that handler
// strips a small, enumerated set of client sloppiness BEFORE calling this and
// validates the result. Unwrapping belongs at the call site that has a reason to
// tolerate it; the predicate stays a predicate.
//
// On refusal the returned string is empty: a caller that drops the error must
// not come away holding something it can put in a URL.
func NormalizeDomain(domain string) (string, error) {
	// Length first, on the raw string. Lowercasing ASCII cannot change a
	// string's length, so measuring before is the same as measuring after, and
	// doing it here keeps an absurdly long input from being walked label by
	// label.
	if len(domain) > maxDomainLength {
		return "", ErrDomainInvalid
	}

	// Split rather than a single pass over the bytes, because every rule below
	// is a rule about a label, and the dots are where the structure lives. Split
	// yields an empty string for each position where a dot is missing a label,
	// so a leading dot, a trailing dot, a doubled dot and the empty input all
	// arrive at the same zero-length label the loop refuses. That is why none of
	// them needs a case of its own.
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		// A single label is `localhost`, `internal-admin`, `2130706433` — names
		// with no public existence that resolve to whatever the AppView's own
		// network or resolver decides they mean. A hostname a stranger can prove
		// ownership of has a public suffix under it, so requiring the dot costs
		// a legitimate registrant nothing.
		return "", ErrDomainInvalid
	}

	for _, label := range labels {
		if !isHostnameLabel(label) {
			return "", ErrDomainInvalid
		}
	}

	// The TLD requirement falls on the last label only, because a leading-digit
	// label like `1.example.com` is legal per RFC 1123 and in use. It is what
	// separates `127.1` and `example.123` from a hostname without this function
	// ever having to decide whether something is an IP address: every spelling
	// of an address ends in a number, and no TLD is one.
	if !isTLDLabel(labels[len(labels)-1]) {
		return "", ErrDomainInvalid
	}

	return strings.ToLower(domain), nil
}

// isHostnameLabel reports whether label is one DNS label of the preferred
// syntax: 1-63 characters of ASCII letters, digits and hyphens, with a hyphen
// in neither the first nor the last position.
//
// The character set is an allowlist, and that is the load-bearing part. Byte
// comparison rather than the unicode package: unicode.IsLetter says yes to `ä`
// and unicode.IsDigit to the Arabic-Indic digits, which would let a name
// through that a resolver cannot use as written and that reads as a different
// name to a human than it does to punycode. Anything outside this set — a
// colon, a slash, an at sign, a percent, a space, CR, LF, NUL, or any byte of a
// multi-byte rune — is a character that belongs to some other part of a URL or
// to no URL at all, and it is refused here rather than left for a URL parser to
// interpret generously.
func isHostnameLabel(label string) bool {
	if len(label) == 0 || len(label) > maxLabelLength {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// isTLDLabel reports whether label is a plausible top-level domain: entirely
// alphabetic, or punycode.
//
// # WHY NOT "CONTAINS A LETTER", WHICH IS WHAT THIS USED TO BE
//
// Because a hex octet is a number that contains a letter. The old rule was
// meant as a proxy for "this is a TLD and not an address", and it read as one
// until the hex spellings were tried against it:
//
//	127.0.0.0x1     accepted, and getaddrinfo resolves it to 127.0.0.1
//	127.0.0x1       accepted, likewise
//	0x7f.0.0.0x1    accepted, likewise
//
// The rejection table's `0x7f.0.0.1` row hid this for as long as it existed:
// that name is refused on its final `1`, and nothing in the validator ever
// looked at the `0x7f`. Move the hex to the LAST label and the proxy waves the
// name through on the `x`. A CGO_ENABLED=0 production build happens to fail
// closed — the pure-Go resolver NXDOMAINs all three — but that is an accident
// of the build, not this function doing its job, and a developer's build
// reaches loopback through an endpoint that needs no credential.
//
// So the rule is the positive one, and it must not be relaxed back to anything
// that admits a digit outside punycode:
//
//   - ALPHABETIC, so no spelling of an integer can satisfy it.
//   - OR PUNYCODE, because an internationalised TLD reaches a resolver as ASCII
//     with digits in it, and `example.xn--p1ai` (Russia's `.рф`) is a real
//     registration. The prefix is anchored: a label that merely CONTAINS
//     `xn--` is not punycode and inherits nothing.
//
// The IANA root holds no TLD that is neither all-alphabetic nor `xn--`-
// prefixed, so nothing legitimate is turned away.
//
// The punycode arm leans on isHostnameLabel having already run: every caller
// checks the character set first, so what follows the prefix here is letters,
// digits and hyphens by construction rather than by a second scan.
func isTLDLabel(label string) bool {
	if isAlphabetic(label) {
		return true
	}
	return len(label) > len(punycodePrefix) &&
		strings.EqualFold(label[:len(punycodePrefix)], punycodePrefix)
}

// isAlphabetic reports whether label is non-empty and made only of ASCII
// letters. Byte comparison rather than the unicode package, for the reason
// isHostnameLabel gives at length: unicode.IsLetter says yes to `ä`.
func isAlphabetic(label string) bool {
	if len(label) == 0 {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}
