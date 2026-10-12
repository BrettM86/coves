# Security model

Formerly `CONSUMER_TRUST_AUDIT.md`, `SECURITY_AUDIT_2026-09-01.md` and `COMMUNITY_HANDLE_BINDING.md`. Backlog issues and older code comments still cite those names. `SECURITY_AUDIT_2026-09-01.md` §1.3 is now [Firehose error classes](#firehose-error-classes), `COMMUNITY_HANDLE_BINDING.md` §4 is now [Community handles and hostedBy](#community-handles-and-hostedby), and the image-proxy decode budget (`SECURITY_AUDIT_2026-09-01.md` §1.1) is in [MEDIA.md](MEDIA.md). Outbound requests are covered in [SSRF_SECURITY.md](SSRF_SECURITY.md) and stored credentials in [CREDENTIAL_ENCRYPTION.md](CREDENTIAL_ENCRYPTION.md). Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Authentication

Coves is an OAuth client (indigo `ClientApp`). It never validates PDS-issued access tokens. It issues its own sealed session token after login and keeps the OAuth session server-side.

`DualAuthMiddleware.RequireAuth` in `internal/api/middleware/auth.go` takes the token from `Authorization: Bearer` first and the `coves_session` cookie second, then dispatches in this order:

1. A token starting with `ckapi_` is an aggregator API key.
2. A JWT-shaped token (three dot-separated parts) is a PDS service JWT. It is accepted only when the DID is a registered aggregator. It is validated with `lexMethod = nil` on purpose, so aggregators authenticate globally rather than per endpoint.
3. Anything else is a sealed session token.

A sealed token alone does not authenticate. `authenticateSealedSession` unseals it (AES-256-GCM, embedded expiry), loads the session from the server-side store, and requires the stored `AccountDID` to equal the sealed DID, so deleting the session row revokes the token. Production refuses an empty, placeholder or wrong-length `OAUTH_SEAL_SECRET`. One live exception: the web `POST /delete-account` handler in `internal/web/handlers.go` only unseals the cookie and never checks the session store.

API keys are 32 random bytes from `crypto/rand`, stored as SHA-256, shown by prefix only, and checked for revocation on every use. A key can only be created from an OAuth session.

## Web login

The frontend starts web login at `/oauth/login?handle=…&redirect=<local path>`. Go owns the transaction. It saves the return path and a browser binding with the pending OAuth request, and sets the `oauth_web_binding` cookie (`Path=/oauth`, HttpOnly, SameSite=Lax, 10 minutes). The cookie never reaches the SvelteKit server.

The callback claims the binding once, atomically, before it exchanges the code or issues `coves_session`. A completion that no browser binding claimed is refused, and the session indigo already persisted is deleted. A new login replaces the cookie, so only the newest attempt can finish from that browser.

Failures redirect to `/login?error=<code>&redirect=<saved path>`. An invalid, expired or unrelated binding is `invalid_request`; storage and configuration failures are `server_error`. Error codes are clamped to a known set. Unrelated callbacks use `/` as the retry destination and do not clear another pending login's cookie.

Mobile uses `/oauth/mobile/login`. Its redirect URIs are matched exactly against `BuildAllowedRedirectURIs`, and a rejected mobile callback cannot fall back to creating a web session. Keep the local browser origin on `127.0.0.1`: mixing it with `localhost` splits the host-only cookies.

## Write path

Every route that writes as a user sits behind `RequireAuth`. The unauthenticated POST routes are deliberate: `social.coves.actor.signup`, `social.coves.actor.requestSignupToken`, `social.coves.aggregator.register`, `/oauth/logout`, `/oauth/refresh` and `/delete-account`. `internal/api/routes/registration_test.go` enforces the auth kind and rate budget of each route in both directions, and `caddy_allowlist_test.go` pins the Caddy `@appview` allowlist against the router. A new non-XRPC route must join that allowlist.

The acting DID always comes from the request context. Handlers reject a body that carries `authorDid`, `createdByDid` or `hostedByDid`. The `repo` in every PDS write is the session DID. Update and delete authorize the URI authority against the session DID before any fetch.

A post-login redirect must pass `isSafeLocalPath`: a path on this origin, no scheme-relative `//` or backslash, no control bytes, and no scheme or authority after `url.Parse`.

## Firehose ownership

Every consumer keys ownership on `event.Did` plus the commit rkey. Voter, commenter, subscriber, blocker and postv2 author are all `event.Did`. An aggregator service record must be published by the aggregator's own DID, and an authorization by the community's own DID. Delete paths build the URI as `at://<event.Did>/<collection>/<rkey>`, so no repo can delete or re-attribute another repo's rows.

A record field is never an identity source. When a record names a DID (a vote subject, a subscription target, a post's community), that DID is a reference to look up, not a claim about who wrote the record.

Each consumer decodes a fixed set of keys. The connector caps WebSocket frames at 16 MiB, and the cursor never advances past an event whose dead-letter write failed.

## Firehose error classes

A Jetstream consumer is a serial read loop. While one event retries, nothing behind it is indexed. The connector sorts every handler error into one of three classes (`internal/atproto/jetstream/errors.go`), and the class decides how much lane time it may spend.

| Class | Meaning | In-line retries | Redrives |
| --- | --- | --- | --- |
| `ErrPermanentEvent` | The event is invalid and replays fail identically | none | none, kept for forensics |
| `ErrUnresolvedReference` | Well-formed, but names something not indexed or an identity not established | none | `UnresolvedRedriveAttempts` (3) |
| transient (any unwrapped error) | Infrastructure failure: Postgres blip, PDS timeout | 200ms, 1s, 3s | `MaxRedriveAttempts` (10) |

Dead letters are pruned after seven days, measured from the last redrive attempt. A redrive pass snapshots the dead-letter ID high-water mark and tries each row at most once. A replay that did no new work because of a cache hit or an in-flight request does not spend an attempt.

The rule: any failure an outside repo can cause with a "the referenced thing may arrive later" argument must use `ErrUnresolvedReference`, never a plain error. A plain error gives that repo a free 4.2-second stall per event, and a thousand fabricated references stall a lane for over an hour. Values that can never resolve (not DID-shaped, wrong collection, rejected by a CHECK or UNIQUE constraint) are `ErrPermanentEvent`. `requireDIDShaped` admits only `did:plc` and `did:web`.

Identity resolution triggered by a firehose event is bounded at 5 seconds (`identityResolveTimeout`), and so is the `hostedBy` DID-document fetch (`wellKnownTimeout`): the record names the host, so a host that accepts TCP and never answers costs the lane the full timeout. Failed or unverified resolutions are cached negatively in-process for `IDENTITY_NEGATIVE_CACHE_TTL` (default 90s, at most 4096 entries). Config refuses to load unless `REDRIVE_INTERVAL` is greater than that TTL, so each redrive reaches the network.

## Record-asserted values

A value a record asserts about itself is never trusted where it affects ranking or provenance.

- `createdAt` in the future is clamped to now for posts and comments (`parseRecordCreatedAt`). Hot-rank SQL clamps a negative age to zero. Migration 041 repaired older comment rows; older post rows were not backfilled.
- `subscriber_count` is derived by a database trigger from `community_subscriptions` (migration 045). The record's `subscriberCount` is ignored.
- `bridgedStats` are honoured only when the repo's stored PDS URL is a `TRUSTED_BRIDGE_PDS_HOSTS` entry (`BridgeTrust`, default-deny). That URL comes from resolving `event.Did`, never from a record field.

`memberCount` is still seeded from the record at community create. That is a known open gap.

## Community handles and hostedBy

The stored community handle comes only from bidirectional resolution of `event.Did`. `createCommunity` always resolves the repo DID and passes the result through `identity.VerifiedHandle`. The record's `handle` field is inert; the lexicon does not declare it.

If the record claims a different handle, the consumer logs a warning and indexes under the resolved handle. It does not reject. Handles are mutable and records are snapshots, so a community that renames keeps a stale handle in its record, and the update path's not-found fallthrough routes such records into create. Rejecting on mismatch would dead-letter renamed communities permanently and buys nothing, because the claimed string never reaches a column.

Resolution outcomes on create:

- verified handle: stored.
- directory unreachable, empty handle or `handle.invalid` (`ErrHandleUnverified`): `ErrUnresolvedReference`.
- malformed identifier, or an answer about a different DID (`ErrIdentitySubjectMismatch`): `ErrPermanentEvent`.

On update, a malformed identifier or a subject mismatch is still permanent, but other failures are tolerated: the row's handle was verified at create, so a directory outage should not dead-letter profile edits. Only a verified resolution may write `pds_url`, because `BridgeTrust` reads that column.

The caching resolver never stores `handle.invalid` or an empty handle, and purges such a row if it finds one. Caching a failed lookup under the 24-hour `IDENTITY_CACHE_TTL` would turn one DNS wobble into a day-long unverifiable identity that every redrive reads back.

The community consumer refuses to start without an identity resolver (`cmd/server/consumers.go`). The record-handle fallback exists only for T0 tests.

`HANDLE_WELL_KNOWN_HOSTS` exists for the hermetic CI stack and local dev, which have no DNS or TLS for community handles. It maps a handle suffix (leading dot required) to a host that serves `/.well-known/atproto-did`, and sends the verification GET there with the handle in `Host`. The bidirectional check itself is unchanged. Config refuses it outside dev, and `identity.WithWellKnownHosts` refuses construction without `WithPrivateHostsAllowed`. This was chosen over a registry of handle authorities in production code: measured production handles (Coves communities and Tidepool bridge communities) all bind through ordinary resolution, and only the local and CI topologies needed help.

`hostedBy` is checked by `verifyHostedByClaim` after the handle is resolved. It must be a `did:web` whose domain equals the resolved handle's eTLD+1. The consumer fetches `https://<domain>/.well-known/did.json` and requires `id` to equal `hostedBy` and `alsoKnownAs` to contain `at://<domain>`. Results are cached in a 1000-entry LRU keyed by DID and handle domain, with an in-flight marker and a 5-second fetch ceiling. Definitive mismatches are permanent; reachability and parse failures are unresolved. `SKIP_DID_WEB_VERIFICATION` skips only this check, is set in dev and CI, and is refused in production. The handle binding is never skippable.

The federation boundary follows. An instance that hosts communities needs per-handle well-known resolution (wildcard DNS and TLS, which any PDS already needs) and an instance `did:web` document at its apex (`static/.well-known/did.json`). A bare PDS writing `social.coves.community.profile` into a repo is rejected because its domain serves no `did.json`. `bsky.social` serves none today. Because eTLD+1 is the comparison, two instances on sibling subdomains of one registrable domain are indistinguishable to `verifyHostedByClaim`.

## Accepted risks

- The AppView holds `PDS_ADMIN_PASSWORD` so it can mint invite codes.
- The frontend trusts `X-Real-IP` from any peer (`ADDRESS_HEADER=x-real-ip`). Caddy overwrites the header with `{remote_host}` on every route to the frontend and the AppView, and the frontend publishes no port, so the trust boundary is membership of `coves-prod-network`. That is accepted, not enforced. See [PRODUCTION_DEPLOY.md](PRODUCTION_DEPLOY.md).
- The rate limiter keys on `X-Real-IP` for the same reason.
- The chi `Logger` in `cmd/server/routes.go` logs full request URIs, including the OAuth `code` and `state` and the sealed token on `/app/oauth/callback?token=…`. Treat AppView logs as secret until those query strings are dropped (backlog `2026-09-04-oauth-tokens-and-codes-in-access-logs`).
- `HandleLogout` reads only the `coves_session` cookie, so a Bearer (mobile) client is never revoked, and `/oauth/refresh` reseals its token indefinitely. Deleting the session row is the only way to end a mobile session (backlog `2026-09-04-mobile-session-unrevocable-refresh-forever`).
- `static/.well-known/did.json` must keep `id` and `alsoKnownAs`, or every local community fails the `hostedBy` check.
