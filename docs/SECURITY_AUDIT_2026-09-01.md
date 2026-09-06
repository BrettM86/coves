# Security audit, 2026-09-01

Scope: full working tree at commit `f43de4c` plus the uncommitted applyWrites /
auto-upvote change. Six parallel reviews (authn/authz, injection and input
validation, SSRF and egress, firehose consumer trust, deployment and secrets,
service-layer authorization) plus `govulncheck` and `gosec`, then cross-checked
by the coordinating session. The two existing audits, `SSRF_SECURITY.md` and
`CONSUMER_TRUST_AUDIT.md` / `COMMUNITY_HANDLE_BINDING.md`, were validated claim
by claim (§4).

Read-only. No files outside this document were changed; no remote hosts were
contacted. Throwaway proof-of-concept code lives only in the session
scratchpad.

Confidence key: **Confirmed** = traced end to end and, where noted, reproduced
with a probe. **Plausible** = code path traced but not executed.

Severity key: p1 = fix before next deploy; p2 = schedule; p3 = backlog.

## Status as of 2026-09-04

Checked against `main` at `f64a999`.

**Fixed in commits since the audit**

| Finding | Commit |
|---|---|
| §1.1 image decompression bomb | `bd4ea42` DecodeConfig pixel budget (50 MP default), admission + processing semaphores, 503 with Retry-After |
| §1.2 encryption key in the database | `20e3e63` app-side AES-256-GCM keyed from `ENCRYPTION_KEY`, startup re-encrypt, migration 046 drops `encryption_keys` |
| §1.4 record-asserted `subscriberCount` | `eb91e03` consumer ignores the field; migration 045 trigger derives the column. `memberCount` still open (tracked below) |
| §2 postv2 content/title uncapped at ingest | `fd5b2ae` `MaxPostTitleBytes` / `MaxPostContentBytes` as permanent refusals. Embed/labels still uncapped |
| §1.3 plain-transient consumer sites | branch `tdd/bounded-transient-sites`: all three sites and the community-create resolver-error path return `ErrUnresolvedReference`; 5 s deadline on every firehose-triggered resolve; process-local DID negative cache (`IDENTITY_NEGATIVE_CACHE_TTL`, default 90 s) with the redriver skipping rows attempted inside that window. Residual: a fresh DID still costs one bounded resolve on the lane |
| §3 `community_blocks` not enforced on reads | `70c36f5` enforced on Discover and timeline (aggregate surfaces) by design |
| §2 auto-upvote amplification of vote divergence, cross-lane race, `RecordCID` float 500 | Not fixed: the uncommitted change was reset and never landed, so these no longer apply. The underlying divergence remains tracked in the backlog |

**Everything else remains open** and is now tracked in the central backlog
(`~/Code/claude-skills/issues/`, filed 2026-09-04):

- p1: `consumer-plain-transient-sites-lane-stall` (§1.3) — fixed, see the table above
- p2: `web-delete-account-skips-session-store`, `mobile-session-unrevocable-refresh-forever`, `oauth-tokens-and-codes-in-access-logs`, `actor-getposts-echoes-internal-errors`, `comment-path-no-ban-root-parent-policy`, `uri-scheme-validation-gaps-comment-embeds-and-ingest`, `unfurl-circuit-breaker-single-global-key`, `media-host-rate-limit-keys-on-cloudflare-edge-ip` (needs-info), `delete-account-leaves-aggregator-key-and-did-rows`, `comment-consumer-no-erasure-gate`, `users-handle-lookup-db-first-stale-owner`
- p3: `vote-cache-never-evicts`, `appview-env-carries-dead-pds-secrets`, `appview-holds-pds-admin-password`, `aggregator-containers-root-world-readable-key`, `oauth-session-hardening-lows`, `ssrf-doc-residuals`, `input-handling-lows`, `infra-exposure-lows`, `consumer-trust-audit-open-items-residual`
- Appended to existing issues: `2026-08-25-coves-community-lexicon-undeclared-field-drift` (memberCount ranking, raised to p2), `2026-08-30-web-oauth-redirect-uri-state-ignored-by-backend` (login CSRF, raised to p2), `2026-07-23-aggregator-register-ssrf-hardening` (cache-purge primitive), `2026-08-27-erasure-deletes-votes-without-recount` (comment_count), `2026-08-27-dlq-retention-and-vote-lane-throttle` (record_revs growth)
- Already tracked before the audit, unchanged: private-community read filtering (`2026-09-01-community-visibility-never-consulted-on-read-paths`), vote-state divergence (`2026-07-22-comment-viewer-vote-state-postgres-dependency`), vote createdAt unclamped (`2026-08-30-createdat-parser-consolidation-vote-consumer-unclamped`), vote `update` dropped (`2026-07-29-vote-putrecord-update-silently-ignored`)

## 0. Headline

No Critical findings. The security investment already made is visible: no SQL
injection anywhere, every write route behind `RequireAuth`, the acting DID
never taken from a request body, the SSRF transport holding up under adversarial
review, ownership keyed on `event.Did` in every consumer, dev resolvers behind a
build tag, production config failing closed, no committed secrets, and a clean
`govulncheck`.

Four findings are p1:

1. **Unauthenticated image decompression bomb** on `/img/...` (§1.1).
2. **Credential encryption key lives in the database it protects**, and the
   `ENCRYPTION_KEY` env var is read by nothing (§1.2).
3. **Three consumer sites still return plain-transient errors** on
   attacker-reachable inputs, re-opening the §1.3 denial-of-service class the
   prior audit closed, and one of them costs ~44 s of lane time per event (§1.3).
4. **Community records self-report `subscriberCount` / `memberCount`** and those
   values seed the columns that Popular and search sort by (§1.4).

The rest is a set of p2 items clustered around session lifecycle, comment-path
policy gaps, erasure completeness, and unbounded record sizes at ingest.

## 1. High (p1)

### 1.1 Image decompression bomb on the unauthenticated proxy — Confirmed, PoC run

`internal/core/imageproxy/processor.go:39` calls `image.Decode` on the full
blob with no `image.DecodeConfig` dimension check and no pixel budget; the
package has no such check anywhere. The handler has no concurrency semaphore.
The only bound is the 10 MB source byte cap.

Attacker needs no Coves account: any did:plc or did:web whose PDS endpoint they
control. They publish a solid-colour PNG and anyone requests
`GET /img/{preset}/plain/{did}/{cid}`.

| Encoded size | Declared dimensions | Decode heap | NRGBA copy |
|---|---|---|---|
| 17.6 KB (measured) | 12000×12000 | 137 MB | 549 MB |
| ~10 MB (extrapolated) | ~40000×40000 | ~1.6 GB | ~6.4 GB |

A handful of parallel requests under the 100 req/min limiter OOM-kills the
AppView. WebP via `x/image` has the same property. `ResolveDID` also does not
cache misses (`identity/caching_resolver.go:59`), so each request re-fetches the
attacker's `did.json`.

Fix: `image.DecodeConfig` first, reject `width*height` above a budget (a few
tens of megapixels), and a small semaphore around decode.

### 1.2 Encryption-at-rest key stored beside the ciphertext — Confirmed

Migration `006_encrypt_community_credentials.sql:15` inserts
`gen_random_bytes(32)` into `encryption_keys`. Every `pgp_sym_encrypt` /
`pgp_sym_decrypt` in `internal/db/postgres/community_repo.go` and
`aggregator_repo.go` reads `(SELECT key_data FROM encryption_keys WHERE id=1)`.
`ENCRYPTION_KEY` is passed to the AppView in `docker-compose.prod.yml:113` and
`.env.prod.example`, but no Go code reads it.

Anything that yields a database read (SQL read primitive, stolen `pg_dump`, the
plaintext `./backups` bind mount written by `scripts/backup.sh`) yields key and
ciphertext together: community PDS passwords (reversible by design), community
access/refresh tokens, aggregator OAuth tokens and DPoP private keys. The
encryption is cosmetic.

Fix: read `ENCRYPTION_KEY` in Go and pass it as the pgcrypto key parameter (or
move to app-side AEAD), re-encrypt existing rows, drop `encryption_keys`, and
encrypt or restrict backups.

### 1.3 Attacker-reachable plain-transient errors on three consumer lanes — Confirmed by probe

The prior audit's §1.3 fix (commit `f43de4c`) bounds `ErrUnresolvedReference`
sites. Three sites are still plain `fmt.Errorf`, so they take the full in-line
retry ladder (4.2 s of sleeps) plus 10 redrives per event. Three throwaway
overlay tests each proved the error matches neither sentinel.

| Site | Trigger record | Cost per event |
|---|---|---|
| `authorpost.go:1061-1071` acceptance/removal from a repo that is not an indexed community | any repo writes `social.coves.community.acceptance` naming any post | 4.2 s posts-lane stall + 10 redrives, no network |
| `community_consumer.go:509-511, 635-637` community profile whose handle resolves to `handle.invalid` or errors | did:plc with no `_atproto` TXT and a tarpit `.well-known`, writing `community.profile/self`; `update` ops re-enter the create path | ~44 s of communities lane (4 × 10 s resolver timeout + sleeps) + 10 redrives of 10 s |
| `user_consumer.go:319-338` unknown-DID `actor.profile` resolved before the bridge-trust check, on the unbounded connector ctx | any repo writes `social.coves.actor.profile`; tarpit did:web | ~44 s of users lane + 10 redrives; tarpit did:plc ≈ 10 s per event |

`identity/caching_resolver.go:46-52,79` deliberately never caches
`handle.invalid` or errors, which is correct for freshness but is what makes
every retry re-resolve. Related p2: `authorpost.go:854-900` resolves every
postv2 author with no users row (5 s bound, never negatively cached).

Fix: return `ErrUnresolvedReference` (or a permanent classification where the
input is malformed) at all three sites; add a short negative cache for resolver
failures keyed on DID; bound the user-consumer resolve with a context deadline.
The existing test `acceptance_consumer_test.go:202` asserts only
not-permanent, so switching to the sentinel keeps it green.

### 1.4 Record-asserted counts rank communities — Confirmed by trace

`community_consumer.go:743-744` copies `memberCount` and `subscriberCount` from
the record into `communities.member_count` / `subscriber_count`. Neither field
is declared in `lexicon/social/coves/community/profile.json`. The default
community list sorts by `c.subscriber_count DESC` (`community_repo.go:496,519`)
and search by `member_count DESC` (`:645`), with no hosted-by filter. The update
path never rewrites the columns, so later increments move from the seeded
baseline.

Precondition: a domain serving a `did.json` with `alsoKnownAs at://<domain>`.
Record `{"name":"nba","hostedBy":"did:web:attacker.example","subscriberCount":2000000000,...}`
at rkey `self` tops Popular and search permanently.

This is the undeclared-field passthrough class `CONSUMER_TRUST_AUDIT.md` §4
declared unique to `handle`. `atprotoHandle`, `federatedFrom`, and
`federatedId` are also decoded but undeclared (not ranking today).

Fix: drop the two fields from `CommunityProfile`, always initialise the columns
to 0 on create, and add a test asserting the record cannot seed them.

## 2. Medium (p2)

### Session lifecycle and web auth

- **Web `/delete-account` accepts a sealed token without consulting the session
  store** (`internal/web/handlers.go:114-148`). Sealed tokens carry an 18-month
  expiry; server-side sessions expire 7 days after last refresh and are deleted
  on logout. Any previously valid token for a victim deletes their AppView
  account months after logout. The XRPC `deleteAccount` route is not affected
  because `RequireAuth` loads the session. Confirmed.
- **Login CSRF on the web OAuth callback** (`oauth/handlers.go:531-869`). State
  is validated only against the server-side `oauth_requests` row; nothing binds
  it to the browser that started the flow. An attacker completes their own
  login up to the callback URL and gets the victim to open it; the victim's
  browser now holds the attacker's session, and whatever they post lands in the
  attacker's account. Mobile-started flows fall through to the web path when
  the mobile cookies are absent. Confirmed.
- **Mobile sessions cannot be revoked, and a stolen mobile token is renewable
  forever.** `HandleLogout` reads only the cookie and ignores `Authorization:
  Bearer` (`handlers.go:1145-1155`); `HandleRefresh` extends `expires_at` by 7
  days and mints a fresh 18-month sealed token on every call
  (`handlers.go:1194-1289`). Confirmed server-side.
- **Sealed token, DID and session id in server logs via the Universal Link
  fallback** (`handlers.go:1098-1117`): the 302 to
  `/app/oauth/callback?token=...` is served by the AppView behind chi's default
  `Logger`, which prints the full `RequestURI`. The same logger records
  `/oauth/callback?code=...&state=...`. Confirmed. Fix: a log formatter that
  drops the query string under `/oauth/*` and `/app/oauth/*`.

### Read-path exposure

- **`actor.getPosts` echoes internal resolution errors to anonymous callers**
  (`handlers/actor/get_posts.go:55-63`). `resolutionFailedError` formats its
  wrapped cause (pq / DNS / PLC text) and falls through to a 400 with
  `err.Error()`. `get_comments.go:57-62` handles the same error correctly.
  Confirmed.
- **Private-community visibility is enforced on write only.**
  `posts/admit.go:544` refuses posting to `visibility=private`, but
  `discover_repo.go:64-75`, the community feed, `community.get`, and
  `list`/`search` (which filter visibility only when the caller asks) all serve
  private communities to anyone. `PRD_COMMUNITIES.md:294` lists visibility
  enforcement as an unchecked item. Rated Medium rather than High because the
  records are public on the firehose regardless; "private" can at best mean
  unlisted on atProto. Confirmed.
- **Handle→DID resolution is DB-first** (`users/service.go:355-366`), so after a
  handle changes hands the old owner's DID is still returned for
  `getProfile` / `getPosts` / login resolve. Already retracted in the prior
  audit (§1.5), still open. Plausible.

### Comment path policy gaps

- **Comment write path forwards reply refs with no policy, and ingest indexes
  anything** (`comment_service.go:843`, `comment_consumer.go:873-926`). No ban
  check, no removed/deleted-root check, no viewer-block check, and no
  parent-in-root consistency check. A user banned from a community can reply
  in it; a record with `root=PostA, parent=CommentX_in_PostB` renders under
  PostB's thread while incrementing PostA's `comment_count`. Confirmed; this is
  prior-audit §1.4 and §3, both still open.
- **Comment embeds, labels and langs are signed verbatim** while posts run
  `normalizeAndValidatePostContent` (`comment_service.go:862-871`, `:1030-1039`
  vs `posts/service.go:1064-1141`). A comment embed with a `javascript:` URI is
  signed and published. The in-progress `pds.RecordCID` change also turns a
  non-integer float in an embed into an unmapped 500. Confirmed.
- **Firehose ingest never scheme-checks facet link or embed URIs.**
  `richtext.SanitizeFacets` has no `#link` arm and `NormalizeLinkURIs` runs only
  on the API write path; `posts/service.go:1060` acknowledges this. A federated
  repo can index `javascript:` links that clients receive verbatim.
  Exploitability depends on client href handling. Plausible.

### Vote integrity

- **Comment vote-state divergence persists and is amplified by the in-progress
  auto-upvote.** Thread views read Postgres, toggles decide from the PDS cache
  (`votes/service_impl.go:135-149`), and `actor.getComments` reads the cache, so
  the two comment surfaces disagree. `CreateComment` now commits an author
  upvote and invalidates the cache; until the firehose indexes it the thread
  shows the comment un-voted, and the author's first tap deletes their own
  upvote. Confirmed by trace.
- **Auto-upvote races its own post across lanes.** Post and vote share one
  `applyWrites` but land on lanes with independent cursors. If the vote lane
  wins, the subject gate dead-letters it with 3 redrives at 5-minute intervals;
  a posts lane lagging over ~15 minutes (which §1.3 makes cheap to arrange)
  loses the author's vote permanently, and every self-post produces a dead
  letter. Plausible.

### Erasure and account deletion

- **`DeleteAccount` leaves DID-bearing rows including a still-valid aggregator
  API key** (`user_repo.go:278-407` never touches `aggregators`,
  `aggregator_authorizations`, `aggregator_posts`, `admin_reports`,
  `community_suggestions`, `identity_cache`; `apikey_service.go:169` does not
  join `users`). Confirmed for schema; key-still-authenticates Plausible.
- **Comment consumer has no erasure gate and the sweep leaves ghost counters.**
  `CommentEventConsumer` lacks `DeletedAccountLookup`, so comments dead-lettered
  before erasure are re-indexed after it and fresh comments from the erased DID
  keep indexing. The hard delete of votes/comments/subscriptions adjusts no
  subject vote count, `comment_count`, or `subscriber_count`.
  `erasure_integrity_test.go` covers only posts and acceptances. Confirmed.

### Unbounded input at ingest

- **postv2 title/content/embed/labels have no size cap at ingest**
  (`authorpost.go:432-476`); comments cap content at 30 000 bytes but not
  embed/labels/langs. The lexicon says 100 k for content; the only bound is the
  16 MiB websocket frame. A hostile repo's 15 MB post is indexed and served
  verbatim in every feed page that includes it. Plausible.

### Egress and infra

- **Unfurl circuit breaker is one global key** (`unfurl/service.go:116` uses
  the constant `"opengraph"`; opens after 3 failures for 5 minutes; only
  `ErrBlockedAddress` is exempt). Three posts linking to a 500-ing or slow host
  disable link previews for every user; repeat every 5 minutes to make it
  permanent. Same per-provider trick for oEmbed keys. Confirmed.
- **Aggregator registration is an unauthenticated identity-cache purge**
  (`handlers/aggregator/register.go:107-113` calls `Purge` then `Resolve`
  before any ownership proof; 10 req/10 min per IP). Cannot poison the cache;
  can thrash it and burn PLC rate limit. Confirmed, low impact.
- **Rate limiter keys on Cloudflare edge IPs for the media host.** No
  `trusted_proxies` in the Caddyfile global block, so `X-Real-IP` for
  `img.coves.social` is a CF egress address and all users behind it share one
  100/min bucket that also covers `/img/*`. Plausible (depends on the host
  being proxied as the Caddyfile says it must be). Fix: `trusted_proxies
  static <cloudflare ranges>` + `client_ip_headers CF-Connecting-IP`, or exempt
  `/img/*` from the global limiter.
- **`PDS_JWT_SECRET`, `HS256_ISSUERS`, `AUTH_SKIP_VERIFY` are injected into the
  AppView and read by nothing** (`docker-compose.prod.yml:95,120,122`). The PDS
  signing key sits in the AppView environment for no benefit. Confirmed.
- **AppView holds full PDS admin credentials** to mint invite codes
  (`docker-compose.prod.yml:171`, `request_signup_token.go`). Blast radius of an
  AppView compromise is every account on the PDS. Accepted design; document it
  or move invite minting to a sidecar.
- **Aggregator containers run as root and copy `COVES_API_KEY` /
  `ANTHROPIC_API_KEY` into world-readable `/etc/environment`**
  (`aggregators/*/docker-entrypoint.sh:26`); `export $(grep ... | xargs)`
  word-splits values; `python:3.11-slim` floating; `anthropic` unpinned.
  Single-tenant, so low exploitability, but the key can post as a trusted
  aggregator, which bypasses per-community authorization. Confirmed.

## 3. Low (p3)

Authn/authz
- `/oauth/refresh` returns the raw PDS access token (`handlers.go:1285-1288`);
  DPoP-bound so not replayable, but nothing in the client needs it.
- Service JWTs validated with `lexMethod=nil` (`middleware/auth.go:455`); any
  `lxm` an aggregator minted is accepted on `post.create`.
- CSRF on cookie-authenticated POSTs rests on `SameSite=Lax` alone; no Origin
  or `Sec-Fetch-Site` check. `DualAuth` accepts the cookie on `post.create`.
- Session ids logged at `handlers.go:1244,1259`.

Input handling
- Malformed comment cursor timestamps reach Postgres unparsed and answer 500
  instead of 400 (`comment_repo.go` `parseCommentCursor`; verified against the
  test database).
- Community search `q` is unbounded and unescaped into `ILIKE` plus two
  `pg_trgm similarity()` calls per row (`community_repo.go:605`). Cap at ~200
  bytes.
- Aggregator registration `log.Printf`s an unvalidated DID (newline forgery).
- `/static/` allows directory listing.
- Vote `createdAt` unclamped at ingest (`vote_consumer.go:139`).

Egress
- Jetstream `websocket.DefaultDialer` (`connector.go:318`) carries
  `ProxyFromEnvironment` and sits outside `scripts/ssrf-audit.sh` patterns.
  Operator config today; the invariant is unenforced for that surface.
- `DPoP` header not stripped on cross-host redirect; no https-only redirect
  policy on the generic guarded client (the OAuth metadata resolver has one).
- No `MaxResponseHeaderBytes` / `ResponseHeaderTimeout`; bounded by the 15 s
  overall timeout.
- Fetched `og:url` overwrites the unfurl result's URI
  (`unfurl/providers.go:258-260`). The post service never copies that field
  into the embed and `Domain` derives from the typed URL, so today this is
  inert; it would matter the day an unfurl endpoint is exposed to clients.
- No length caps on unfurl title/description before Postgres write.
- Reddit aggregator builds the Streamable oEmbed URL without encoding
  (`link_extractor.py:197`); allowlisted host, parameter injection only.

Consumers and data
- Dead-letter queue keeps raw bytes 7 days deduped only by md5;
  `jetstream_record_revs` grows one row per URI forever.
- No `recover()` around `HandleEvent` (`connector.go:495`, `redrive.go:263`);
  no reachable panic was found, so a future one takes the process down.
- Removal-delete hashes every `removed` row per event (`authorpost.go:1445`).
- `VoteCache` never evicts (`votes/cache.go`: expiry hides entries, only
  `Invalidate` deletes); memory ∝ users × votes. Every post/comment creation
  now invalidates it, so the next read re-paginates the author's entire vote
  collection at 100/page.
- `originalAuthor` / `federatedFrom` / `location` are untyped `interface{}`
  signed verbatim for any user; not indexed.
- Block enforcement is one-directional on every read (blocker sees nothing from
  blocked; blocked still sees blocker); `community_blocks` not enforced on
  reads. Consistent across paths.

Infra
- `/health/consumers` is public through the Caddy allowlist: consumer names,
  cursors, dead-letter counts, queue backlog. `LastError` is hidden.
- AppView and PDS published on `127.0.0.1`; any host-local process bypasses
  Caddy and the allowlist and sets `X-Real-IP` freely.
- `scripts/aggregator-setup/5-create-api-key.sh:107` shows an example key in
  the real `ckapi_` format; confirm it was never live.
- AppView fallback CSP allows `script-src 'unsafe-inline'`.
- Dev compose binds Postgres 5435, PDS 3001, Jetstream 6008/6009, PLC 3002 on
  `0.0.0.0` with default passwords.
- Unauthenticated `aggregator.getMetrics` exposes two failure counters.

## 4. Validation of the existing audits

### 4.1 `SSRF_SECURITY.md`

`bash scripts/ssrf-audit.sh`: TOTAL 0, all categories clear (38 in-source
exemptions, each spot-checked as operator-config or fixed-vendor).
`go test ./internal/atproto/oauth/ -count=1`: pass, 39 guard tests, 0 skips.

| Claim | Verdict |
|---|---|
| Hostname normalisation consistent with Go's stack | HOLDS (`transport_idn_host_test.go`) |
| IP literals refused by default | HOLDS (`transport_ip_literal_test.go`) |
| DNS once per request, dial only the vetted address | HOLDS (`transport_toctou_test.go`) |
| Mixed public/private answers fail closed | HOLDS (`transport_revetting_test.go`) |
| No environment proxy | HOLDS for HTTP (`client_guard_test.go:184`); websocket dialer is out of scope (§3) |
| Redirects limited and re-vetted per hop | HOLDS |
| Connection and overall deadlines | PARTIAL: values exist, but the only test asserts non-zero; eight call sites overwrite `Timeout`; no `ResponseHeaderTimeout` |
| 32 MiB cap on decompressed bytes | HOLDS (`transport_body_cap_test.go`) |
| Sentinel errors | HOLDS |
| IPv4-in-IPv6, NAT64, 6to4, Teredo handling | HOLDS |
| Hatch only via option functions, never ambient config | HOLDS (`tests/audit/ssrf_audit_test.go`, `cmd/server/allow_private_hosts_test.go`) |
| `WithWellKnownHosts` requires the hatch | HOLDS |
| Production build is `CGO_ENABLED=0` | HOLDS |
| Every exemption declared in-source with a reason | HOLDS |
| "Every non-static destination uses the guarded client" | DOES NOT HOLD as stated: `websocket.DefaultDialer` at `connector.go:318` is unaudited. Not exploitable today. |

Suspected and found mitigated: DNS rebinding, redirect to IP literal, gzip
bombs (cap counts decompressed bytes), IPv6 embedding tricks, did:web
port/path (indigo restricts to bare hostname with TLD filter), image cache
path traversal (DID/CID syntax-validated before path build), unfurl `og:image`
re-fetched through the guarded blob client with a 6 MB cap and MIME allowlist.

### 4.2 `CONSUMER_TRUST_AUDIT.md` and `COMMUNITY_HANDLE_BINDING.md`

`go test ./internal/atproto/jetstream/ -count=1`: pass.

| Claim | Verdict |
|---|---|
| Ownership keyed on `event.Did` + rkey in every consumer | HOLDS |
| Delete paths resolve rows via `event.Did`-built URIs | PARTIAL: community profile delete ignores rkey (`community_consumer.go:430`), self-scoped |
| Handle stored from resolution, never the record | HOLDS (four pinning tests) |
| Record/resolution contradiction is a warning not a rejection | HOLDS |
| Shared `identity.VerifiedHandle` predicate at every site | HOLDS |
| Identity cache no longer memoises failure | HOLDS (and is what makes §1.3 repeat per event) |
| `handle` was the only undeclared passthrough | DOES NOT HOLD: `memberCount`, `subscriberCount`, `atprotoHandle`, `federatedFrom`, `federatedId` also decoded; two rank (§1.4) |
| Future `createdAt` clamped on posts and comments | HOLDS (post-consumer clamp has no direct T0 pin) |
| Hot-rank negative-age clamp and migration 041 | HOLDS |
| Legacy `community.post` ingestion retired | HOLDS |
| f43de4c redrive bounding mechanism | HOLDS for the listed sites; three unlisted plain-transient sites remain (§1.3) |
| §1.3 per-site classification table | HOLDS |
| Acceptance fetch classification | HOLDS |
| `verifyDIDDocument` cache, limiter, 5 s ceiling, SSRF guard | HOLDS |
| Acceptance/removal binding to indexed community | PARTIAL: indexed-path cross-community branch at `authorpost.go:1154` untested (as §4 flagged); "must be indexed" refusal is plain transient |
| Direct fetch via `sync.getRecord` with CID recomputation, 1 MiB, 5 s | HOLDS |
| Deletes never decrement counters for never-indexed rows | HOLDS |
| Vote uniqueness by partial unique index | HOLDS |
| Subscription counts gated on fresh insert | HOLDS by trace, no dedicated test |
| Bridge trust provenance from resolved PDS endpoint | HOLDS |
| Erasure marker gates posts, votes, acceptances | PARTIAL: comments ungated, sweep leaves counters (§2) |
| User consumer never indexes unknown users | PARTIAL: it resolves every unknown DID before deciding (§1.3) |
| `users.handle` is not an addressing key | DOES NOT HOLD; already retracted in TRUST §1.5, still open |

Prior audit OPEN items, status in the working tree: all nine remain open
(§1.4 comment root/parent consistency, §1.5 handle DB-first, §1.6 aggregators
posting directly unmetered, §2 vote `update` dropped, §2 community delete
ignores rkey, §2 subscription/block `update` dropped, §2 `account` events
no-op (safe direction), §2 pre-emptive removals unbounded, §3 no ban gate on
comments or votes).

## 5. Tooling

- `govulncheck ./...`: 0 vulnerabilities in code or imported packages. One
  advisory in a required module (`GO-2026-5932`, `x/crypto/openpgp`) is not
  called.
- `gosec` (medium+): 62 raw issues. Triaged: every G201/G202 SQL-formatting hit
  is a fixed-allowlist fragment with bound parameters; G404 is retry jitter;
  G115 is bounded by a modulo; G101 hits are dev DSNs in tooling; G124 cookie
  flags are set conditionally on dev mode; G710 open-redirect hits are covered
  by `isSafeLocalPath` and the exact-match mobile allowlist; G304/G301/G306 in
  `cmd/` tooling and the image cache are not reachable from requests.

## 6. Verified mitigations worth not re-auditing

- Every write route behind `RequireAuth`; `registration_test.go` enforces auth
  kind and rate budget per route in both directions; `caddy_allowlist_test.go`
  pins the Caddy allowlist against the router.
- Acting DID always from context; body-supplied `authorDid` /
  `createdByDid` / `hostedByDid` rejected; `repo` in every PDS payload is the
  session DID; edits swap-guarded; create is deterministic-rkey create-only.
- Post admission order: private wall → ban (fail-closed) → aggregator auth →
  dedupe → quota. Unknown actor class fails closed.
- Seal: AES-256-GCM, random nonce, embedded expiry; production refuses empty,
  placeholder, or short seal and cursor secrets. Dev resolvers compile-time
  stubbed under `!dev`.
- API keys 256-bit `crypto/rand`, SHA-256 at rest, prefix-only display,
  revocation checked per use; key management OAuth-only.
- Rate limiter keys on `X-Real-IP`, which Caddy replaces on every appview block.
- Open redirect: post-login target must be a local path; mobile redirect URIs
  exact-match; OAuth error codes clamped.
- SQL: every dynamic fragment from a fixed map or switch; IN-lists from
  placeholders; feed cursors HMAC-signed; comment cursors size-capped.
- Bodies tiered and capped; limits/offset/depth clamped in every service;
  facets range-checked and capped at 200 at both API and ingest; no unchecked
  type assertions in non-test code; chi `Recoverer` on handlers.
- `html/template` only; no `template.HTML`; the only user value rendered is the
  session handle.
- Consumers: transactional rev gate with tombstones; erasure gate on posts,
  votes, acceptances failing closed; direct fetch with CID recomputation;
  `bridgedStats` default-deny with provenance from the resolved PDS endpoint;
  postv2 `community` immutable across updates; 16 MiB frame cap; cursor never
  advances on dead-letter write failure.
- Infra: images digest-pinned; AppView uid 1000; no docker socket, privileged,
  or `cap_add`; Postgres not published; no pprof/metrics/admin endpoints;
  HSTS, nosniff, `X-Frame-Options DENY`, Referrer-Policy; explicit-origin OAuth
  CORS; Telegram client redacts its token and disables previews; no committed
  secrets; no `curl | sh`.

## 7. Suggested order of work

1. Image proxy pixel budget + semaphore (§1.1). Small change, removes an
   unauthenticated OOM.
2. Sentinel errors at the three consumer sites + negative resolver cache
   (§1.3). Small change, closes a free lane-stall.
3. Drop `memberCount` / `subscriberCount` from the record decode (§1.4).
   One-line change plus a test.
4. `ENCRYPTION_KEY` actually used, rows re-encrypted, backups protected (§1.2).
   Needs a migration and a re-encrypt step.
5. Session lifecycle cluster: session-store check on web delete, state-to-browser
   binding on the callback, Bearer-aware logout, query-string-free logging under
   `/oauth/*`.
6. Comment path: parent-in-root check, ban check, embed/URI validation shared
   with posts, erasure gate.
7. Ingest size caps on postv2 and comment embeds.
8. Per-host unfurl breaker; error-message fix in `getPosts`; private-community
   read filtering; dead env vars removed from compose.
