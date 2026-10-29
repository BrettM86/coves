# Media

Formerly `docs/PRD_CSAM_SCANNING.md`, plus the image-proxy finding of the 2026-09-01 security audit. Code comments and config files cite this file by section heading, so keep the headings stable.

Cloudflare's CSAM Scanning Tool can only match images that cross its edge. Coves therefore serves every client-facing image through one hostname, `img.coves.social`, which is the only Cloudflare-proxied host.

## Choke point

While the proxy is enabled, the AppView never builds `com.atproto.sync.getBlob` URLs for images. All image URLs come from `internal/core/embeds` (`HydrateView`) and the `blobs` URL helpers, which use `IMAGE_PROXY_CDN_URL` when set and `IMAGE_PROXY_BASE_URL` otherwise. The proxy route is `/img/{preset}/plain/{did}/{cid}` (GET and HEAD), and the DID is the repo that holds the blob.

A non-dev config with `IMAGE_PROXY_ENABLED=false` refuses to start unless `ALLOW_UNPROXIED_MEDIA=true`. That flag is the deliberate opt-out for a self-hoster with no scanning CDN, and it also means widening the Caddyfile `img-src`. With the proxy on, a missing or localhost base URL is rejected: the mobile app cannot resolve a relative URL.

The apex `coves.social` 301-redirects `/img/*` to `img.coves.social`, so the same bytes are never served from an unscanned host.

## Proxied hosts

The apex, `pds.coves.me`, `coves.me` and `tdpl.io` stay DNS-only. DPoP `htu` matching, firehose WebSockets, on-demand TLS for bridged handles, and `*.*.tdpl.io` (which Cloudflare wildcards do not cover) all need a direct origin. The `img.coves.social` block in the Caddyfile serves `/img/*` only and 404s everything else, so a cache rule on that host can never cache an API response. Caddyfile changes need `docker compose up -d --force-recreate caddy`, because the file is bind-mounted.

## Accepted exceptions

- Video. The proxy decodes stills and cannot stream, so `embed.video#view` gives a direct PDS `getBlob` URL for the video blob (its thumbnail is proxied). The CSP `media-src` lists `pds.coves.me`, `coves.me` and `tdpl.io` for this. Video is unscanned.
- `record.embed`. Post and comment responses carry the author's record verbatim, blob refs included. The invariant is that the AppView does not construct unproxied URLs, not that no blob ref reaches a client. Neither client reads `record.embed`.
- Bluesky media (approved 2026-09-19). Resolved Bluesky posts keep validated HTTPS `cdn.bsky.app` URLs (`internal/core/blueskypost/cdn_url.go`). They bypass the Coves proxy and scanning; URL validation is not content scanning.
- Direct PDS `getBlob` stays publicly fetchable, because atproto sync needs it. The API hands such a URL out only for video.

Scanning is on serve, not on ingest, and matches only known hashes. Nothing hash-matches at upload, unfurl rehost or bridge ingest; user reports and admin review are the backstop.

## Decode budget

The proxy is unauthenticated and decodes blobs from any PDS, so an attacker can publish a small PNG that declares huge dimensions. `processor.go` reads the header with `image.DecodeConfig` and refuses an image whose declared pixel count exceeds `IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS` (default 50) before decoding. Decodes run in `IMAGE_PROXY_MAX_CONCURRENT_PROCESSES` slots (default 4). A request waits at most 5 seconds for a slot, then gets 503 with `Retry-After` derived from the same wait. `IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS` (default 64) caps cache-miss requests holding fetched bytes, and `IMAGE_PROXY_MAX_SOURCE_SIZE_MB` (default 10) caps the fetch.

## Cache-Control

`successCacheControlFor` and `writeErrorResponse` in `internal/api/handlers/imageproxy/handler.go` set the policy:

| Request | Cache-Control |
|---|---|
| Any query string, even a bare `?`; any error or blocked response | `no-store` |
| Purge configured, host is a purge base host, canonical DID and CID, no `Origin` header | `public, max-age=86400, s-maxage=31536000` |
| Every other 200 or 304 | `public, max-age=86400` |

A purge names only the bare URL, so query-string variants must never be shared-cacheable. A request with `Origin` gets one day because a CDN cache key may include Origin, which the bare-URL purge would miss. No proxied image response carries `immutable`.

`/img/default-avatars/` does not use this table. Every 200 or 304 from it sends `public, max-age=31536000, immutable`, whatever the query string or `Origin`, because the path is versioned (`v1`), the files never change, and moderation never removes them. Any other path under the route (an unknown version, an unknown slug, or a name not ending in `.webp`) gets `404` with `no-store`.

To check a host, use `curl -sD- -o /dev/null https://img.coves.social/img/avatar/plain/<did>/<cid>`. `curl -I` also works: both `/img` routes answer HEAD with the same status and headers as GET and no body. `https://img.coves.social/` must 404.

## Cloudflare zone

These are dashboard settings on the `coves.social` zone; nothing in the repo checks them.

- `img.coves.social` A/AAAA records proxied; every other record DNS-only.
- A cache rule on `img.coves.social/*`, eligible for cache, edge TTL respecting origin headers. Browser Cache TTL set to respect existing headers.
- The default cache key. URL purge only matches the default key, so never build a key from headers, cookies or query parameters.
- Always Use HTTPS on every purged host, because the purge names only the https URL.
- SSL Full (strict).
- CSAM Scanning Tool enabled, with a verified monitored address such as `abuse@coves.social`.

## Edge purge

Set both `CLOUDFLARE_CACHE_PURGE_ZONE_ID` and `CLOUDFLARE_CACHE_PURGE_API_TOKEN`, or neither; config validation rejects one alone and requires the proxy on with absolute base URLs. The token needs only Zone > Cache Purge on `coves.social` and is separate from Caddy's DNS-01 `CLOUDFLARE_API_TOKEN`.

A removal records durable purge targets (owner DID, CID) in `moderation_media_purges` inside the removal transaction. After commit it attempts a purge of every preset under each base URL, retries failures from a one-minute sweep, and completes each target with a final purge no earlier than `HTTP_WRITE_TIMEOUT` after the block committed. That final purge bounds how long the origin can still be writing a response it authorized before the block; it does not bound buffering inside the edge.

Edge copies stored under the old `public, max-age=31536000, immutable` header keep serving for up to a year, including images removed since. Run Purge Everything on the `coves.social` zone once after the first deploy that serves the one-day header; only `img.coves.social` is proxied, so this drops only cached images. The AppView logs this reminder at startup while purge is off.

With purge on, `IMAGE_PROXY_BASE_URL` and `IMAGE_PROXY_CDN_URL` must be served through the purged zone with the Host header unchanged. Nothing checks this at startup, and a shared cache the purge cannot reach keeps a removed image for up to a year.

An illegal-content (every-owner) block can only name the owners whose copies are in this instance's disk cache. That cache expires entries after `IMAGE_PROXY_CACHE_TTL_DAYS` (default 30) and evicts least-recently-read first above `IMAGE_PROXY_CACHE_MAX_GB`, while the edge may keep a URL for a year. An owner whose copy left the disk cache, failed to write to it, or was cached through another AppView is not purged. Settle this before enabling purge in production; the planned fix, a durable blob reference index, is in [plans/IMAGE_UPLOADS.md](plans/IMAGE_UPLOADS.md).

After enabling purge in production, check it by hand. Warm an image through the edge, remove its subject, and confirm the edge returns 404 for that URL. The same URL requested with an `Origin:` header must show only `public, max-age=86400`, and `http://img.coves.social/img/…` must redirect to https without serving the image.

## Removal exposure

| Cache | Purge off | Purge on |
|---|---|---|
| Browser | 1 day | 1 day |
| Cloudflare edge for a named URL | 1 day | until the purge |
| Shared cache the purge cannot reach | 1 day | up to 1 year |

Copies cached under the old `public, max-age=31536000, immutable` header keep their original lifetime. Browser copies cannot be reached; edge copies need the one-time Purge Everything described under [Edge purge](#edge-purge). See [MODERATION.md](MODERATION.md#media-blocks) for which blobs a removal blocks.

## CSAM matches

Cloudflare blocks the matched URL at the edge, files a third-party NCMEC report, and emails a daily digest of matched paths. A path `/img/{preset}/plain/{did}/{cid}` names the owning repo and the blob.

Cloudflare does not cover the provider's own duties, and no tooling exists for them yet.

Find the subject first: search `posts.embed` and `comments.embed` for the CID. A legacy bridged post (`social.coves.community.post`) is owned by the community DID, so that is the DID in the matched path. Then act in this order:

1. Preserve the record, blob and account metadata for 90 days (18 U.S.C. §2258A) in an encrypted, access-restricted store that is never publicly readable.
2. Remove the subject in the AppView with `removeContent` and reason illegal-content. This blocks the CID for every owner, purges the disk cache and, with purge on, every preset URL at the edge.
3. Take down the record and blob on the owning PDS (the PDS admin API for native accounts, the Tidepool bridge for bridged ones; `TRUSTED_BRIDGE_PDS_HOSTS` identifies bridged origins).
4. Act on the account. Ban a native account. For bridged content, report to the origin Lemmy instance's admins, and block a repeat instance at the bridge.

Also file Coves' own NCMEC CyberTipline report, and log each step (who, what, when).

Step 2 must come before anything deletes the record or the account. A comment delete blanks its embed and account erasure deletes the rows, after which the removal blocks no CID. If the subject is already gone, delete the CID's cached files under `IMAGE_PROXY_CACHE_PATH` (laid out `<preset>/<did>/<cid>`) and purge each preset URL at the edge by hand.

Avatar and banner matches have no moderation subject, because `removeContent` takes only posts and comments. They need the PDS takedown plus the manual cache and edge purge.

Never log or store the image outside the preservation store.
