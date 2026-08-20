# SSRF security model

This document describes the maintained server-side request forgery (SSRF)
boundary in Coves. It is an architecture and contributor guide, not an incident
log. The implementation and its regression tests remain the source of truth.

Potential security vulnerabilities should be reported privately as described in
[`SECURITY.md`](../SECURITY.md).

## Threat model

Coves participates in a federated network and therefore fetches URLs derived
from remote identities, PDS records, OAuth discovery, link previews, image
metadata, and aggregator registrations. A syntactically valid remote record is
not a trust boundary: a remote party may control the hostname, its DNS answers,
redirects, response headers, and response body.

The shared egress boundary is `NewSSRFSafeHTTPClient` in
`internal/atproto/oauth/transport.go`. Production code that accepts a non-static
destination must use this client or document a narrowly scoped exemption that
is enforced by `scripts/ssrf-audit.sh`.

## Guard invariants

The guarded client provides the following properties:

- Hostnames are normalized consistently with Go's HTTP stack before lookup.
- IP-literal destinations are refused by default.
- DNS is resolved once per request under the request context. Every answer is
  classified, and the connection is made directly to an approved address. This
  closes the DNS rebinding window between validation and dialing.
- A hostname is refused if any answer is private, local, special-purpose, or
  otherwise disallowed. Mixed public/private answer sets fail closed.
- The transport does not use environment-configured HTTP proxies. Adding
  `http.ProxyFromEnvironment` would bypass the direct relationship between the
  vetted address and the socket destination and must be treated as a security
  change.
- Redirects are limited and each destination is processed by the same guard.
- Requests have connection and overall deadlines. Response bodies have a
  32 MiB default cap, with tighter or explicitly larger limits at call sites
  that need them.
- Address refusals match `ErrBlockedAddress`; oversized reads match
  `ErrResponseTooLarge`. Callers should use `errors.Is` rather than matching
  rendered messages.

Protocol-specific validation remains the caller's responsibility. The generic
client intentionally supports federation endpoints that use non-default ports.
A caller with a narrower protocol contract should apply that contract to the
initial URL and every redirect.

## Address policy

The classifier rejects the standard library's loopback, unspecified, private,
link-local, and multicast classes. It also rejects IANA special-purpose ranges
that are not globally reachable, even when a local network happens to route
them. That includes documentation and benchmarking networks, transition
mechanisms that can carry private destinations, and deprecated local-use space.

Known destination-carrying IPv4-in-IPv6 formats are decoded and the embedded
IPv4 address is classified. The well-known NAT64 prefix `64:ff9b::/96` remains
usable for public IPv4 destinations.

RFC 8215 local-use NAT64 space (`64:ff9b:1::/48`) is blocked in full. RFC 6052
allows deployments to choose more-specific Pref64 lengths and layouts, so the
IPv4 position cannot be inferred safely from the address alone. If Coves ever
needs a local-use Pref64, add an explicit operator configuration and decode only
the declared prefix and layout. Do not scan arbitrary IPv6 offsets or weaken
the default block.

Useful registries and protocol references:

- [IANA IPv4 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv4-special-registry/)
- [IANA IPv6 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv6-special-registry/)
- [RFC 6052: IPv6 Addressing of IPv4/IPv6 Translators](https://www.rfc-editor.org/rfc/rfc6052)
- [RFC 8215: Local-Use IPv4/IPv6 Translation Prefix](https://www.rfc-editor.org/rfc/rfc8215)

## OAuth discovery

Indigo's OAuth application contains three independent HTTP clients:

1. the main OAuth client;
2. the identity directory client; and
3. the authorization-server metadata resolver.

`NewOAuthClient` replaces all three with Coves-guarded clients. The metadata
resolver has a ten-second timeout, a 1 MiB response limit, and an HTTPS/no-
explicit-port redirect policy. Construction and end-to-end `StartAuthFlow`
tests cover all three clients, including a private DNS answer that must not
reach a listener and an explicit development-hatch control.

Password-based PDS login also constructs its API client before calling
`com.atproto.server.createSession`, so the password-bearing request and all
subsequent authenticated requests share the guarded transport.

## Development hatch

Private addresses are available only through explicit option functions such as
`WithPrivateAddressesAllowed` and package-specific equivalents. They exist for
local development and hermetic tests, whose services normally listen on
loopback.

Production wiring must pass no hatch option. Keep the choice local to each
client construction; do not derive it implicitly inside the transport or from
ambient proxy settings. Guard tests should always contain both directions:

- hatch closed: the private listener is never reached and the error matches
  `ErrBlockedAddress`;
- hatch open: the otherwise identical fixture is reached, proving the closed
  case is testing classification rather than a broken client.

## Resolver behavior and hostname syntax

The transport classifies addresses returned by the resolver, regardless of
which Go resolver implementation produced them. Legacy numeric hostname forms
may be rejected as malformed or may resolve to an IPv4 address depending on the
platform and resolver mode; if they resolve, the resulting address is still
classified before dialing. The production container uses the pure-Go resolver
with `CGO_ENABLED=0`, and the audit pins that build setting to keep production
resolution behavior deterministic.

Entry points that accept a domain rather than a general URL should additionally
use the positive DNS-name validation in `internal/validation/domain.go`.

## Adding or changing egress

When adding a network fetch:

1. Identify who controls the destination, DNS, redirects, and response body.
2. Use `NewSSRFSafeHTTPClient` before the first request leaves the process.
3. Apply scheme, port, content-type, timeout, and body-size rules for that
   protocol.
4. Add a listener-never-reached test for a well-formed hostname resolving to a
   private address, plus an explicit hatch-open control where local access is
   supported.
5. Run both audit gates and the relevant test tiers.

Any `coves:allow-*` exemption is a security decision. Keep it next to the
construction it exempts and explain why the destination cannot be controlled by
an untrusted party.

## Verification

Run at minimum:

```sh
make test
make test-audit
make ssrf-audit
```

`make ssrf-audit` is a regression tripwire over production egress construction;
it is not a substitute for behavioral tests or code review. Before a release,
run the repository's integration and end-to-end gates as well.
