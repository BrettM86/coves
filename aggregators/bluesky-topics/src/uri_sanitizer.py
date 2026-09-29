"""
Percent-encoding for URLs written into atproto records.

The atproto `uri` string format admits only printable ASCII after the scheme
(the reference regex is ``^[a-z][a-z.-]{0,80}:[[:graph:]]+$``, and POSIX
[:graph:] is 0x21-0x7E). RSS feeds routinely carry URLs with the character
itself rather than its escape — ``.../rudy_gobert_pokémon_lineup/`` — and those
resolve perfectly well in a browser while making the published record fail
schema validation for every third-party tool that resolves our lexicons.

This module repairs such URLs rather than dropping them: percent-encoding a
non-ASCII path byte and punycoding a non-ASCII host both name the exact same
resource, so a sanitized URL dereferences identically to the original. The input
is never *decoded* along the way — an existing ``%2F`` stays ``%2F`` rather than
becoming a path separator, which would silently repoint the link.

Mirrors the server-side normalizer in internal/validation/uri.go. That claim is
enforced, not aspirational: both implementations are tested against the shared
corpus in internal/validation/testdata/uri_vectors.json, so a one-sided change
here turns the Go suite red and vice versa.

Splitting is done with plain string operations rather than urlsplit(), which
silently deletes interior tab/newline characters instead of encoding them.
"""
import re

import idna

# Bounds of POSIX [:graph:] — the only bytes atproto's uri format allows after
# the scheme.
_GRAPH_LOW = 0x21
_GRAPH_HIGH = 0x7E

# Matches the atproto `uri` format, as enforced by the reference implementation.
# `fullmatch` is used at the call site rather than `match`, because `$` would
# also accept a trailing newline that the reference parser rejects.
_URI_FORMAT = re.compile(r"[a-z][a-z.-]{0,80}:[\x21-\x7e]+")

# The reference parser also caps total length.
_MAX_URI_LENGTH = 8192

# Generic RFC 3986 scheme, deliberately broader than the atproto format so a
# scheme the format cannot express is reported by name rather than as a generic
# parse failure.
_SCHEME = re.compile(r"^([A-Za-z][A-Za-z0-9+.-]*):")
_ATPROTO_SCHEME = re.compile(r"^[a-z][a-z.-]{0,80}$")

# The only schemes sanitize_uri will emit. These reach fields that clients
# render as clickable links, and sanitizing would otherwise happily repair an
# unsafe URI into a valid record. An allowlist rather than a blocklist: a
# blocklist of javascript/data/vbscript/file/mailto still let through ftp:,
# blob:, intent: and every custom app scheme, none of which is a web link a
# browser should navigate a reader to from a feed.
_ALLOWED_SCHEMES = frozenset({"http", "https"})


def is_valid_uri(value: str) -> bool:
    """Return True if value already satisfies the atproto `uri` format."""
    return len(value) <= _MAX_URI_LENGTH and bool(_URI_FORMAT.fullmatch(value))


def sanitize_uri(value) -> str:
    """
    Coerce a URL into the atproto `uri` format.

    Idempotent: a URL that already conforms is returned untouched, so existing
    percent-escapes are never double-encoded or decoded.

    Args:
        value: URL as it came off the feed. A non-string is rejected rather than
            raising an unexpected AttributeError at the caller.

    Returns:
        A URL satisfying the atproto `uri` format.

    Raises:
        ValueError: if no valid URI can be recovered — an empty value, a value
            with no scheme, a scheme the format cannot express or that is
            forbidden in a rendered link, a value beyond the length cap, or a
            host that cannot be punycoded.
    """
    if not isinstance(value, str):
        raise ValueError(f"uri is empty or not a string: {type(value).__name__}")

    # Surrounding whitespace is stripped rather than escaped: a trailing newline
    # off a scraped feed is never part of the intended URL, and %0A would
    # silently bake it into the published record. Interior whitespace is
    # escaped, not dropped, since it may be significant.
    trimmed = value.strip()
    if not trimmed:
        raise ValueError("uri is empty")
    if len(trimmed) > _MAX_URI_LENGTH:
        raise ValueError(
            f"uri is too long: {len(trimmed)} bytes (max {_MAX_URI_LENGTH})"
        )

    # The scheme is inspected before the already-conforming shortcut below: a
    # "javascript:" or "mailto:" URI is pure ASCII and therefore satisfies the
    # format on its own, so checking afterwards would wave through exactly the
    # schemes this refuses to emit.
    #
    # Failure messages deliberately never embed the URI itself — they are logged
    # by the bridges, and feed URLs can carry signed-URL tokens or credentials
    # in the query string. The Go normalizer's errors are equally redacted.
    match = _SCHEME.match(trimmed)
    if not match:
        raise ValueError(
            "uri is missing a scheme (an absolute URI such as https://… is required)"
        )
    scheme = match.group(1).lower()
    if not _ATPROTO_SCHEME.match(scheme):
        raise ValueError(f"uri scheme is not valid for the atproto uri format: {scheme!r}")
    if scheme not in _ALLOWED_SCHEMES:
        raise ValueError(
            f"uri scheme is not allowed in a rendered link (only http and https are accepted): {scheme!r}"
        )

    # An http(s) URI must be hierarchical with a host. "https:foo" and
    # "https://" satisfy the atproto format and the scheme check, but the
    # WHATWG parser every client renders through rejects them, so a record
    # carrying one would have a link that silently vanishes in the UI.
    rest = trimmed[match.end():]
    if not rest.startswith("//"):
        raise ValueError(f"uri has no host: {scheme} URI has no authority (expected {scheme}://host/…)")
    authority, remainder = _split_authority(rest[2:])
    if not _host_of(authority):
        raise ValueError(f"uri has no host: {scheme} URI has an empty host")

    if is_valid_uri(trimmed):
        return trimmed

    sanitized = f"{scheme}://{_encode_authority(authority)}{_escape_non_graph(remainder)}"

    if len(sanitized) > _MAX_URI_LENGTH:
        raise ValueError(
            f"uri is too long: {len(sanitized)} bytes after encoding (max {_MAX_URI_LENGTH})"
        )
    if not is_valid_uri(sanitized):
        raise ValueError("uri cannot be normalized to the atproto uri format")
    return sanitized


def _host_of(authority: str) -> str:
    """Return the host portion of an authority (userinfo and port stripped)."""
    host = authority.rsplit("@", 1)[-1]
    if host.startswith("["):
        end = host.find("]")
        return host[: end + 1] if end >= 0 else host
    head, sep, tail = host.rpartition(":")
    if sep and tail.isdigit():
        return head
    return host


def _split_authority(value: str) -> tuple:
    """Split the authority from the rest. Per RFC 3986 it ends at / ? or #."""
    for index, char in enumerate(value):
        if char in "/?#":
            return value[:index], value[index:]
    return value, ""


def _encode_authority(authority: str) -> str:
    """
    Punycode a non-ASCII host, leaving any userinfo escaped and the port intact.

    A host cannot simply be percent-encoded: escapes in an authority are not
    resolvable by DNS, so ``exämple.com`` -> ``ex%C3%A4mple.com`` would produce
    a URL that satisfies the format check but that our HTTP clients cannot
    fetch.

    The IDNA2008 `idna` package with uts46=True is used rather than the stdlib
    ``.encode("idna")`` codec: the stdlib codec is IDNA2003, which maps ``ß`` to
    ``ss`` and would resolve to a different registrable domain than the Go side
    produces. This choice is what keeps the two implementations in agreement.
    """
    if not _has_non_graph(authority):
        return authority

    userinfo = ""
    hostport = authority
    at = authority.rfind("@")
    if at >= 0:
        userinfo = _escape_non_graph(authority[:at]) + "@"
        hostport = authority[at + 1:]
    if not _has_non_graph(hostport):
        return userinfo + hostport

    # Only a registered domain name can still carry non-graph bytes here — an
    # IPv6 literal is entirely ASCII — so a trailing ":digits" is unambiguously
    # a port.
    host, port = hostport, ""
    colon = hostport.rfind(":")
    if colon >= 0 and hostport[colon + 1:].isdigit():
        host, port = hostport[:colon], ":" + hostport[colon + 1:]

    try:
        encoded_host = idna.encode(host, uts46=True).decode("ascii")
    except idna.IDNAError as exc:
        raise ValueError(f"cannot punycode host {host!r}: {exc}") from exc

    return userinfo + encoded_host + port


def _has_non_graph(value: str) -> bool:
    """Return True if value contains a byte outside printable ASCII."""
    return any(byte < _GRAPH_LOW or byte > _GRAPH_HIGH for byte in value.encode("utf-8"))


def _escape_non_graph(value: str) -> str:
    """
    Percent-encode every byte outside printable ASCII.

    Every conforming byte — including an existing '%' — is left untouched, which
    is what keeps the transform idempotent and prevents an existing escape from
    being decoded. Iteration is over UTF-8 octets because percent-encoding is
    defined on bytes, not characters.
    """
    if not _has_non_graph(value):
        return value
    return "".join(
        chr(byte) if _GRAPH_LOW <= byte <= _GRAPH_HIGH else f"%{byte:02X}"
        for byte in value.encode("utf-8")
    )
