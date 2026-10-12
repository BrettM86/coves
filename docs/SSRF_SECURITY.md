# SSRF security

Report suspected vulnerabilities privately as described in [SECURITY.md](../SECURITY.md). The wider auth and firehose trust rules are in [SECURITY_MODEL.md](SECURITY_MODEL.md).

## Threat model

Coves fetches URLs derived from remote identities, PDS records, OAuth discovery, link previews, image metadata and aggregator registrations. A syntactically valid remote record is not a trust boundary: the remote party may control the hostname, its DNS answers, redirects, response headers and response body.

`NewSSRFSafeHTTPClient` in `internal/atproto/oauth/transport.go` is the single egress guard. Production code that fetches a non-static destination must use it, or carry a narrow exemption marker that `scripts/ssrf-audit.sh` enforces.

## Guard invariants

- Hostnames are normalized the way Go's HTTP stack normalizes them before lookup.
- IP-literal destinations are refused by default.
- DNS is resolved once per request under the request context. Every answer is classified, and the dial goes straight to an approved address. The base transport's `DialContext` ignores the hostname and fails closed when no vetted address is in the context. This closes the DNS rebinding window between check and dial. TLS still verifies the requested hostname.
- A hostname is refused if any answer is disallowed. Mixed public and private answer sets fail closed.
- The transport does not use environment proxies. Adding `http.ProxyFromEnvironment` would break the link between the vetted address and the socket, and is a security change.
- Redirects are limited, and each hop goes through the same guard.
- The client has a 10-second dial timeout and a 15-second overall timeout by default; call sites may override the overall timeout. Response bodies are capped at `DefaultMaxResponseBytes` (32 MiB) unless a call site sets its own limit.
- Address refusals match `ErrBlockedAddress` and oversized reads match `ErrResponseTooLarge`. Match them with `errors.Is`, not on the message.

Protocol-specific validation is the caller's job. The generic client allows non-default ports because federation endpoints use them. A caller with a narrower contract applies it to the first URL and every redirect.

## Address policy

The classifier rejects the standard library's loopback, unspecified, private, link-local and multicast classes. It also rejects IANA special-purpose ranges that are not globally reachable, even when the local network routes them: documentation and benchmarking networks, transition mechanisms that can carry private destinations, and deprecated local-use space.

Known IPv4-in-IPv6 formats are decoded and the embedded IPv4 address is classified. The well-known NAT64 prefix `64:ff9b::/96` stays usable for public IPv4 destinations.

RFC 8215 local-use NAT64 space, `64:ff9b:1::/48`, is blocked in full. RFC 6052 lets deployments choose more-specific Pref64 lengths and layouts, so the IPv4 position cannot be inferred from the address. If Coves ever needs a local-use Pref64, add explicit operator configuration and decode only the declared prefix and layout. Do not scan arbitrary IPv6 offsets or weaken the default block.

References:

- [IANA IPv4 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv4-special-registry/)
- [IANA IPv6 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv6-special-registry/)
- [RFC 6052: IPv6 Addressing of IPv4/IPv6 Translators](https://www.rfc-editor.org/rfc/rfc6052)
- [RFC 8215: Local-Use IPv4/IPv6 Translation Prefix](https://www.rfc-editor.org/rfc/rfc8215)

## OAuth discovery

Indigo's OAuth `ClientApp` carries three independent HTTP clients: the main OAuth client, the identity directory client, and the authorization-server metadata resolver. `NewOAuthClient` in `internal/atproto/oauth/client.go` replaces all three with guarded clients. Leaving the resolver's default client in place would expose a proxy-aware SSRF path, because `StartAuthFlow` takes an attacker-chosen `https://` authorization-server identifier on a public route.

The metadata resolver client (`newOAuthResolverHTTPClient`) has a 10-second timeout, a 1 MiB response cap, at most five redirects, and accepts only HTTPS redirects with no explicit port.

Password-based PDS login (`pds.NewFromPasswordAuth`) builds its API client on the guarded transport before calling `com.atproto.server.createSession`, so the password-bearing request and every later authenticated request share the guard.

## Development hatch

Private addresses are reachable only through explicit option functions, each marked `coves:allow-ssrf-hatch`: `WithPrivateAddressesAllowed` on the transport, and a `WithPrivateHostsAllowed` option in each package that builds a guarded client (identity, unfurl, imageproxy, blobs, communities). `identity.WithWellKnownHosts` redirects the handle-verification GET for configured suffixes to a local PDS over plain `http`. It wraps the guarded transport rather than replacing it, and resolver construction refuses it unless the private-hosts hatch is also set.

Production wiring passes no hatch. Keep the choice local to each client construction. Never derive it inside the transport or from ambient proxy settings. `.env.ci` sets `IS_DEV_ENV=true`, so `make ci` runs every call site with the hatch open; a green merge gate says nothing about the closed path. That is why `make ssrf-audit` exists.

Guard tests always cover both directions:

- hatch closed: the private listener is never reached and the error matches `ErrBlockedAddress`.
- hatch open: the otherwise identical fixture is reached, proving the closed case tests classification rather than a broken client.

## Resolver policy

The transport classifies whatever addresses the resolver returns, regardless of which Go resolver produced them. Legacy numeric hostname forms may be rejected as malformed or resolved to an IPv4 address depending on platform and resolver mode. If they resolve, the address is still classified before dialing.

The production image is built with `CGO_ENABLED=0`, so it uses the pure-Go resolver. `scripts/ssrf-audit.sh` fails if any `go build` in the `Dockerfile` runs without `CGO_ENABLED=0` in the same stage, which keeps production resolution deterministic.

Entry points that accept a domain rather than a URL should also use the positive DNS-name validation in `internal/validation/domain.go`.

## Adding egress

1. Identify who controls the destination, its DNS, redirects and response body.
2. Use `NewSSRFSafeHTTPClient` before the first request leaves the process.
3. Apply the scheme, port, content-type, timeout and body-size rules of that protocol.
4. Add a test where a well-formed hostname resolves to a private address and the listener is never reached, plus a hatch-open control where local access is supported.
5. Run `make test`, `make test-audit` and `make ssrf-audit`, plus the relevant integration and e2e tiers.

A `coves:allow-*` exemption is a security decision. Put it on the construction it exempts and say why an untrusted party cannot control the destination. `make ssrf-audit` is a tripwire over client construction, not a substitute for behavioural tests or review.

## Known gaps

The Jetstream connector dials with `websocket.DefaultDialer` (`internal/atproto/jetstream/connector.go`). That dialer is outside the guard and outside the ssrf-audit patterns, and it honours `ProxyFromEnvironment`. Jetstream URLs come from operator config (`JETSTREAM_FEEDS`), not from remote input.

The guarded client sets no `ResponseHeaderTimeout` or `MaxResponseHeaderBytes`; only the overall client timeout bounds a slow header. Both gaps are tracked in backlog issue `2026-09-04-ssrf-doc-residuals`.
