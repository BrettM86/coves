package validation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"golang.org/x/net/idna"
)

// The atproto `uri` string format admits only printable ASCII after the scheme
// (indigo enforces `^[a-z][a-z.-]{0,80}:[[:graph:]]+$`, and POSIX [:graph:] is
// 0x21-0x7E). Any other byte — a literal accented character, a space, a stray
// newline from a scraped feed — makes the record fail schema validation for
// every third-party tool that resolves our lexicons, even though the URI itself
// resolves perfectly well in a browser.
const (
	graphLow  = 0x21
	graphHigh = 0x7e

	// maxURILength mirrors the cap in indigo's syntax.ParseURI. Checked
	// explicitly so an over-long URI reports its actual cause rather than
	// collapsing into the generic "cannot be normalized".
	maxURILength = 8192
)

// schemePattern matches the generic RFC 3986 scheme so the scheme can be split
// off before any encoding work. It is deliberately more permissive than the
// atproto format (which allows neither digits nor '+'); atprotoScheme below is
// what decides whether the scheme is actually usable, so a rejected scheme gets
// a message naming the scheme instead of a generic parse failure.
var (
	schemePattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)
	atprotoScheme = regexp.MustCompile(`^[a-z][a-z.-]{0,80}$`)

	// hostProfile is UTS #46 lookup processing with DNS length verification:
	// it case-folds and maps before punycoding, and rejects empty or over-long
	// labels. It is chosen to match, case for case, what the Python bridges get
	// from the IDNA2008 `idna` package with uts46=True — the two must agree or
	// a bridge will drop links the AppView would have accepted, and vice versa.
	// See testdata/uri_vectors.json, which pins that agreement.
	hostProfile = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.VerifyDNSLength(true))

	// disallowedURIs are schemes refused outright rather than normalized. These
	// values reach `embed.external.uri` and richtext `#link.uri`, both of which
	// clients render as hrefs, and normalization would otherwise happily
	// *repair* such a URI into a schema-valid one and sign it into a federated
	// record that every downstream consumer inherits.
	//
	// Two categories, refused for different reasons:
	//   - javascript/data/vbscript execute or inline content in the renderer,
	//     so a stored one is stored XSS in any client that does not defend
	//     itself.
	//   - file/mailto do not name a fetchable remote resource. file: points at
	//     the viewer's own disk, and mailto: in a public federated feed is an
	//     address-harvesting and spam vector rather than a link to content.
	disallowedURIs = map[string]struct{}{
		"javascript": {},
		"data":       {},
		"vbscript":   {},
		"file":       {},
		"mailto":     {},
	}
)

// Errors returned by NormalizeURI for input that carries no recoverable URI.
var (
	// ErrURIEmpty is returned for an empty or whitespace-only URI.
	ErrURIEmpty = errors.New("uri is empty")

	// ErrURINoScheme is returned when the input has no scheme. The atproto
	// `uri` format requires an absolute URI, and there is no safe way to guess
	// the intended scheme for a bare "example.com/path".
	ErrURINoScheme = errors.New("uri is missing a scheme (an absolute URI such as https://… is required)")

	// ErrURIBadScheme is returned when a scheme is present but cannot appear in
	// the atproto `uri` format, which allows no digits and no '+' (so "s3://…"
	// and "view-source:…" are both rejected).
	ErrURIBadScheme = errors.New("uri scheme is not valid for the atproto uri format")

	// ErrURISchemeNotAllowed is returned for a scheme that names executable or
	// inline content, or that does not name a fetchable remote resource at all.
	// See disallowedURIs for the set and the reasoning.
	ErrURISchemeNotAllowed = errors.New("uri scheme is not allowed in a rendered link")

	// ErrURITooLong is returned for a URI beyond the atproto length cap, either
	// as supplied or after percent-encoding expanded it.
	ErrURITooLong = errors.New("uri is too long")

	// ErrURIUnnormalizable is returned when encoding the input still does not
	// yield a conforming URI.
	ErrURIUnnormalizable = errors.New("uri cannot be normalized to the atproto uri format")
)

// ValidURI reports whether raw already satisfies the atproto `uri` string
// format, using the same parser third-party validators run against our
// federated records.
func ValidURI(raw string) bool {
	_, err := syntax.ParseURI(raw)
	return err == nil
}

// NormalizeURI coerces raw into a string that satisfies the atproto `uri`
// format, returning an error only when no valid URI can be recovered.
//
// The transform is meaning-preserving. Bytes outside printable ASCII are
// percent-encoded and a non-ASCII host is punycoded; both name the exact same
// resource, so a normalized URI dereferences identically to what the client
// sent. Critically, the input is never *decoded* along the way: an existing
// `%2F` stays `%2F` rather than becoming a path separator, which would silently
// repoint the link at a different resource.
//
// This runs on the write path because the AppView — not the client — is what
// signs community post records into the PDS, so normalizing here is the only
// place that can guarantee a conforming record regardless of which client
// produced it. Rejecting instead would turn an ordinary action (pasting a link
// containing an accented character) into a hard error the user has no way to
// fix.
//
// Encoding is done by splitting the URI into scheme / authority / remainder
// with plain string operations rather than net/url. url.Parse rejects several
// inputs that are trivially recoverable — a stray '%' that is not an escape, an
// interior tab, a non-ASCII userinfo, an `at://` URI whose DID reads as a port
// — and round-tripping through url.URL.String() decodes reserved characters in
// the path. Both behaviours are the opposite of what this function promises.
//
// NormalizeURI is idempotent: input that already conforms is returned untouched,
// so existing percent-escapes are never double-encoded.
func NormalizeURI(raw string) (string, error) {
	// Surrounding whitespace is stripped rather than escaped: a trailing newline
	// off a scraped feed is never part of the intended URI, and %0A would
	// silently bake it into the record. Interior whitespace is escaped, not
	// dropped, since it may be significant.
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrURIEmpty
	}
	if len(trimmed) > maxURILength {
		return "", fmt.Errorf("%w: %d bytes (max %d)", ErrURITooLong, len(trimmed), maxURILength)
	}

	// The scheme is inspected before the already-conforming fast path below.
	// A "javascript:" or "mailto:" URI is pure ASCII and therefore satisfies
	// the format on its own, so checking after the fast path would wave through
	// exactly the schemes this refuses to emit.
	match := schemePattern.FindStringSubmatch(trimmed)
	if match == nil {
		return "", ErrURINoScheme
	}
	scheme := strings.ToLower(match[1])
	if !atprotoScheme.MatchString(scheme) {
		return "", fmt.Errorf("%w: %q", ErrURIBadScheme, scheme)
	}
	if _, blocked := disallowedURIs[scheme]; blocked {
		return "", fmt.Errorf("%w: %q", ErrURISchemeNotAllowed, scheme)
	}

	if ValidURI(trimmed) {
		return trimmed, nil
	}

	var out strings.Builder
	out.Grow(len(trimmed) + 16)
	out.WriteString(scheme)
	out.WriteByte(':')

	rest := trimmed[len(match[0]):]
	if strings.HasPrefix(rest, "//") {
		out.WriteString("//")
		authority, remainder := splitAuthority(rest[2:])
		encoded, err := encodeAuthority(authority)
		if err != nil {
			return "", err
		}
		out.WriteString(encoded)
		out.WriteString(escapeNonGraphBytes(remainder))
	} else {
		// Opaque URI (urn:, magnet:, at: without an authority, …): everything after
		// the scheme is encoded as-is.
		out.WriteString(escapeNonGraphBytes(rest))
	}

	normalized := out.String()
	if len(normalized) > maxURILength {
		return "", fmt.Errorf("%w: %d bytes after encoding (max %d)",
			ErrURITooLong, len(normalized), maxURILength)
	}
	if !ValidURI(normalized) {
		return "", ErrURIUnnormalizable
	}
	return normalized, nil
}

// splitAuthority separates the authority component from everything that follows
// it. Per RFC 3986 the authority ends at the first '/', '?' or '#'.
func splitAuthority(s string) (authority, remainder string) {
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// encodeAuthority punycodes a non-ASCII host and percent-encodes any userinfo,
// leaving the port untouched.
//
// A host cannot simply be percent-encoded: escapes in an authority are not
// resolvable by DNS, so "exämple.com" -> "ex%C3%A4mple.com" would produce a URI
// that satisfies the format check but that our HTTP clients cannot fetch.
// hostProfile is used rather than the bare idna.ToASCII wrapper: the latter is
// the raw Punycode profile, which applies no case folding and no validation, so
// it encodes "EXÄMPLE.com" to a different DNS label than the one the user
// linked to and happily emits empty or over-long labels.
func encodeAuthority(authority string) (string, error) {
	if !hasNonGraphBytes(authority) {
		return authority, nil
	}

	var userinfo string
	hostPort := authority
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo = escapeNonGraphBytes(authority[:at]) + "@"
		hostPort = authority[at+1:]
	}
	if !hasNonGraphBytes(hostPort) {
		return userinfo + hostPort, nil
	}

	// Only a registered domain name can still carry non-graph bytes here: an
	// IPv6 literal is entirely ASCII, so a trailing ":digits" is unambiguously
	// a port.
	host, port := hostPort, ""
	if colon := strings.LastIndex(hostPort, ":"); colon >= 0 {
		if candidate := hostPort[colon+1:]; isAllDigits(candidate) {
			host, port = hostPort[:colon], ":"+candidate
		}
	}

	asciiHost, err := hostProfile.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("%w: cannot punycode host %q: %w", ErrURIUnnormalizable, host, err)
	}
	return userinfo + asciiHost + port, nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// hasNonGraphBytes reports whether s contains a byte outside printable ASCII.
func hasNonGraphBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < graphLow || s[i] > graphHigh {
			return true
		}
	}
	return false
}

// escapeNonGraphBytes percent-encodes every byte outside printable ASCII,
// leaving every conforming byte — including an existing '%' — untouched so the
// result stays idempotent under repeated application and no existing escape is
// decoded. Iteration is over bytes rather than runes because percent-encoding
// is defined on the UTF-8 octets.
func escapeNonGraphBytes(s string) string {
	if !hasNonGraphBytes(s) {
		return s
	}
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= graphLow && c <= graphHigh {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}
