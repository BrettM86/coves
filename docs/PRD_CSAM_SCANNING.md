# PRD: CSAM Scanning via Cloudflare + Media Choke Point

**Status:** Workstreams 1 and 2 implemented (AppView + Caddy). Workstream 3 (Cloudflare zone config) is manual dashboard/DNS work, not yet done. Workstream 4 (takedown runbook) not started.
**Last updated:** 2026-07-27

## Problem

Coves hosts user media as atproto blobs on PDSes we operate:

1. **Native uploads** — images/video uploaded by users to `pds.coves.me` (or the community's PDS), and external-link thumbnails that the unfurl pipeline rehosts into the community repo (`internal/core/blobs/service.go` `UploadBlobFromURL`).
2. **Bridged Lemmy media** — the Tidepool bridge materializes federated content as blobs in virtual repos on the bridge PDS (`tdpl.io` stack). Lemmy has a documented history of CSAM-spam waves (2023 lemmy.world attacks); as the blob host, we carry the legal exposure for this content.

We currently have **no scanning of any kind**. Cloudflare's [CSAM Scanning Tool](https://developers.cloudflare.com/cache/reference/csam-scanning/) is free on all plans, compares content served through the Cloudflare cache against NCMEC (and partner) hash lists, blocks matches at the edge, emails us daily with matched paths, and files a third-party report with NCMEC on our behalf. Since Feb 2025 it requires no NCMEC credentials — just a verified notification email.

## Key architectural insight

Cloudflare can only scan what is **proxied through Cloudflare and cached at its edge**. Today, almost none of our media qualifies:

- Embed thumbnails are hotlinked directly to the origin PDS — `posts.TransformBlobRefsToURLs` emitted `{pds}/xrpc/com.atproto.sync.getBlob?...` (fixed in workstream 1; that code is gone).
- Image-embed blobs (`social.coves.embed.images`) are returned to clients as raw blob refs; clients fetch from wherever they resolve.
- `tdpl.io` **cannot** be CDN-proxied at all: on-demand TLS for bridged-handle certs requires DNS pointing directly at the origin (`Caddyfile` catch-all block), and Cloudflare wildcard proxying doesn't cover `*.*.tdpl.io` anyway.
- `pds.coves.me` serves the atproto sync surface (firehose WebSockets, relay traffic) — proxying it through Cloudflare is possible but risky and unnecessary.

However, we already have the right choke point built: the **image proxy** (`internal/core/imageproxy/`, route `GET /img/{preset}/plain/{did}/{cid}`). It resolves *any* DID to its PDS (including the bridge PDS), fetches the blob, transforms it, and serves it with `Cache-Control: public, max-age=31536000, immutable` + ETag — ideal for edge caching. The presets registry already includes `content_preview`, `content_full`, and `embed_thumbnail`, not just avatars/banners.

**Decision:** Do NOT put the whole site (or the PDS, or tdpl.io) behind Cloudflare. Instead:

> Route **all client-facing media** through the image proxy on a single dedicated hostname (`img.coves.social`), orange-cloud only that hostname, and enable the CSAM Scanning Tool on the `coves.social` zone.

This leaves DPoP `htu` matching, firehose WebSockets, on-demand TLS, and handle resolution untouched, while giving Cloudflare visibility into 100% of media that Coves clients display.

## Workstream 1 — Emit proxy URLs for all media (AppView) — ✅ implemented

Everything the API returns to clients must reference `img.coves.social`, never a PDS.

All embed projection now lives in one place: **`internal/core/embeds`** (`HydrateView`), called with the DID of the repository that owns the blobs. `posts.TransformBlobRefsToURLs` delegates to it for post embeds (community-owned), and `comments.buildCommentView` calls it for comment embeds (author-owned — comment records live in the user's repo).

1. **Config**: ✅ `blobs.ImageURLConfig` (`ProxyBaseURL` from `IMAGE_PROXY_BASE_URL`, with `CDNURL` taking precedence). Production defaults now point at `https://img.coves.social` in `.env.prod.example` and `docker-compose.prod.yml`. `IMAGE_PROXY_CDN_URL` was never passed into the container — fixed.
   - The URL config moved from a package global in `communities` to `blobs`, where its consumers already live, and its read/write race was replaced with an `RWMutex`.
2. **External embed thumbnails**: ✅ emits `{base}/img/embed_thumbnail/plain/{did}/{cid}` and declares `social.coves.embed.external#view`. The gallery `external.images[]` array is hydrated too; `#viewExternal.images` now refs `#viewImage` rather than the blob-bearing `#image`.
   - ⚠️ **Scanning-bypass invariant**: ✅ enforced in `internal/config`. A non-dev config with `IMAGE_PROXY_ENABLED=false` refuses to start unless the operator sets `ALLOW_UNPROXIED_MEDIA=true` — the deliberate opt-out for self-hosters not fronting media with a scanning CDN. An enabled proxy with a missing or localhost base URL is also rejected (relative URLs are unresolvable for the mobile app).
3. **Image embeds** (`social.coves.embed.images`): ✅ each image is projected into `#viewImage` with `thumb` (`content_preview`) and `fullsize` (`content_full`), preserving `alt` and `aspectRatio`. New `#view`/`#viewImage` defs added to the lexicon; both post and comment `embed` unions accept them.
4. **Avatars/banners**: ✅ already converged via `blobs.HydrateImageURL`. One leak fixed: `comments.buildPostView` hand-rolled a `getBlob` URL for community avatars (and its HTTPS-only guard silently dropped the avatar in dev).
5. **Video**: ✅ `social.coves.embed.video#view` — `thumbnail` goes through the proxy (`content_preview`); the `video` blob becomes a direct PDS `getBlob` URL, since the proxy decodes stills and cannot stream. This is the one deliberate, documented gap (Workstream 5).
6. **Mobile app** (`~/Code/coves-mobile`): both clients were already written against this shape (`coves-frontend` types `social.coves.embed.images#view` with `thumb`/`fullsize`; mobile reads `image['thumb'] ?? image['fullsize']`). Two small follow-ups remain — see below.

Also fixed alongside: `record["embed"]` in `post_repo.go` aliased `postView.Embed`, so hydrating the view silently rewrote the "verbatim record" too. The record now decodes its own copy.

**Acceptance:** ✅ `grep -rn "sync.getBlob" --include="*.go" internal/ cmd/` returns only the proxy's own fetcher, the `blobs.HydrateBlobURL` helper (used for the video gap and the proxy-disabled fallback), and comments/docs.

### Client follow-ups (not in this repo)

- `coves-frontend`: `EmbedImage.image` is typed as required but is no longer emitted — the server sends `thumb`/`fullsize` only (mirroring `app.bsky.embed.images#viewImage`). Make `image` optional, and change `extractEmbedUrl`'s `images#view` branch to return `fullsize` instead of `image`. Rendering paths (`bestImageURL`, `extractEmbedThumbnail`) already go through `imageUrl()` and are unaffected.
- Neither client reads `record.embed`, so the de-aliasing above is invisible to them.

## Workstream 2 — `img.coves.social` at the origin (Caddy) — ✅ implemented

The `img.coves.social` site block is in the production `Caddyfile`: `/img/*` reverse-proxies to `appview:8080`, everything else 404s, TLS via the existing Cloudflare DNS-01 token (`CLOUDFLARE_API_TOKEN`), `Access-Control-Allow-Origin: *` so images are embeddable cross-origin from the web app and mobile.

Also done:
- CSP: the `coves.social` `img-src` is now `'self' data: https://img.coves.social`.
- Error responses from the proxy carry `Cache-Control: no-store` (`writeErrorResponse` in `internal/api/handlers/imageproxy/handler.go`), so a transient PDS timeout or an unpropagated DID can't be pinned at the edge for the year the success path advertises.

Remaining at deploy time: **the bind-mount trap** — Caddyfile changes require `docker compose up -d --force-recreate caddy`, not just a `git pull` + reload.

## Workstream 3 — Cloudflare zone configuration

On the `coves.social` zone (we already own it — DNS-01 tokens exist):

1. **DNS**: `img.coves.social` A/AAAA → OVH origin IP, **Proxied** (orange cloud). All other records stay DNS-only (grey) — especially anything under `tdpl.io` and `coves.me`.
2. **Cache**: add a Cache Rule for `img.coves.social/*`: *Eligible for cache*, respect origin `Cache-Control`. Blobs are content-addressed (CID in URL) so immutable caching is correct. Optionally enable Tiered Cache.
3. **Enable CSAM Scanning Tool**: Dashboard → Caching → Configuration → CSAM Scanning Tool → Configure. Provide a monitored role address (e.g. `abuse@coves.social`, forwarded to admins) and verify it. Agree to the service-specific terms.
4. **SSL mode**: Full (strict) for the zone (origin has valid certs via Caddy).
5. Do **not** enable Cloudflare features that interfere with API semantics on other hostnames — only `img` is proxied, so blast radius is zero.

ToS note: since Cloudflare's 2023 self-serve ToS update, serving non-HTML assets like images through the CDN on free plans is explicitly permitted.

## Workstream 4 — Match response: takedown runbook + tooling

What Cloudflare does on a match: blocks the URL at the edge and sends a **daily digest email** listing matched file paths, and files a third-party NCMEC report. What it does *not* do: remove the blob from our PDS, purge our origin cache, take down the post, or handle our own reporting/preservation obligations.

Our matched-path format is self-identifying: `/img/{preset}/plain/{did}/{cid}` gives us the owning repo (DID) and blob (CID) directly.

Build an admin takedown flow (CLI or admin endpoint), input = DID + CID:

1. **Locate**: query the AppView index for all posts/records referencing the blob CID; identify the account (native user vs bridged actor via `TRUSTED_BRIDGE_PDS_HOSTS` origin).
2. **Preserve**: before deletion, export the record + blob + account metadata to an encrypted, access-restricted preservation store. US providers must preserve reported content for 90 days (18 U.S.C. §2258A); Cloudflare's docs suggest retaining documentation ~1 year. This store must never be publicly readable.
3. **Remove from serving**:
   - Delete the record + blob from the owning PDS (native: PDS admin API; bridged: tidepool bridge admin path).
   - Remove/tombstone the post in the AppView index.
   - Purge the imageproxy disk cache for **all presets** of that DID+CID (add a purge-by-blob admin method to `imageproxy.DiskCache` — cache keys are preset-scoped).
   - Purge the Cloudflare edge cache by URL for each preset variant (single-file purge is available on free plans). Cloudflare's own block covers the exact matched URL; we purge the sibling preset URLs.
4. **Report**: file our own NCMEC CyberTipline report (Cloudflare's third-party report does not replace the provider's own obligation).
5. **Act on the source**: ban the native account, or for bridged content: report to the origin Lemmy instance's admins and, on repeat, drop the instance at the bridge (instance blocklist) — this is where "rely on Lemmy moderation" plugs in.
6. **Log** the entire action (who, what, when) to an audit table. Never log or store the image content outside the preservation store.

Phase 1 can be a documented manual runbook using existing tools (psql, PDS admin API, `curl` to Cloudflare purge API); the admin tooling hardens it later.

## Workstream 5 — Residual gaps (known and accepted for phase 1)

| Gap | Why it remains | Mitigation / future |
|---|---|---|
| Scan-on-serve, not scan-on-ingest | Cloudflare only sees content when a client requests it through the edge | Phase 2: hash-match at ingest (unfurl rehost path + bridge ingest) via PhotoDNA or ROOST hash-matcher |
| Direct PDS `getBlob` remains publicly fetchable | Required by atproto sync (relays, other AppViews) | API no longer emits these URLs; optionally rate-limit `getBlob` at Caddy for non-relay UAs |
| Only known-hash CSAM is detected | Fuzzy hash lists can't catch novel content | Community reporting (`internal/core/adminreports/`) + moderator review remain the backstop |
| Video blobs unscanned | Image proxy is stills-only | Track as separate workstream |
| Bridge PDS stores blobs regardless of scanning | Blobs land before any serve-time scan | Phase 2 ingest scanning; instance allow/blocklist at the bridge is the coarse control |
| `record.embed` still carries blob references | Post and comment responses include the verbatim atproto record, whose embed is unprojected by design (the lexicon calls it verbatim). A client *could* build a `getBlob` URL from it | Neither client reads `record.embed` today. The invariant we actually hold is "the AppView constructs no unproxied URLs", not "no blob reference reaches a client" — the same bytes are public on the PDS regardless. Revisit if a client starts reading it |
| Resolved Bluesky quote-post images | `social.coves.embed.post` resolution returns `cdn.bsky.app` URLs for the quoted author's avatar and embed thumb. Not our blobs, not on our PDS | Bluesky scans its own CDN. These are also currently blocked by our CSP (`img-src` does not include `cdn.bsky.app`) — decide whether to allow-list, proxy, or drop the fields |

## Rollout order

WS1 and WS2 are both in the tree, so they ship together. The ordering constraint that remains is **DNS before deploy**: the AppView will start emitting `https://img.coves.social/...` URLs the moment it boots with the new config, so that hostname has to resolve and serve first or every image 404s.

1. **DNS + Cloudflare (WS3)** — create the `img.coves.social` A/AAAA record pointing at the OVH origin, **Proxied** (orange cloud). Every other record stays DNS-only, especially `tdpl.io` and `coves.me`. Set the zone to Full (strict). Add the cache rule for `img.coves.social/*`. Enable the CSAM Scanning Tool with a verified role address.
2. **Deploy Caddy** — `docker compose up -d --force-recreate caddy` (bind-mount trap). Verify `curl -sD- -o /dev/null https://img.coves.social/img/avatar/plain/<did>/<cid>` returns 200 with `Cache-Control: public, max-age=31536000, immutable`, and that `https://img.coves.social/` 404s. (Use `-sD- -o /dev/null`, not `-I`: the image route is registered GET-only and chi does not map HEAD to it, so `-I` returns 405 and shows none of the cache headers.)
3. **Deploy the AppView** with `IMAGE_PROXY_BASE_URL=https://img.coves.social`. Startup now fails loudly on a misconfigured proxy rather than silently falling back. Verify feeds render, then watch proxy cache hit rate and origin bandwidth.
4. **Client follow-ups** — ship the two `coves-frontend` type/`extractEmbedUrl` changes noted in WS1.
5. WS4 runbook written and dry-run before announcing Lemmy federation more broadly.
6. Phase 2 (ingest-time hash matching) scheduled after federation traffic is real.

## Open questions

- Should the `coves.social` apex also be orange-clouded eventually (DDoS/WAF benefits)? Not required for scanning; revisit separately — DPoP `htu` uses the Host header and should survive proxying, but needs testing.
- Preset URL for full-size originals: do we ever need un-transformed blobs client-side? If yes, add an `original` preset (still scanned/cached) rather than falling back to `getBlob`.
- EU users / DSA reporting equivalents once we have EU presence.

## References

- Cloudflare CSAM Scanning Tool docs: https://developers.cloudflare.com/cache/reference/csam-scanning/
- Feb 2025 onboarding simplification (no NCMEC credentials needed): https://blog.cloudflare.com/a-simpler-path-to-a-safer-internet-an-update-to-our-csam-scanning-tool/
- Changelog entry: https://developers.cloudflare.com/changelog/post/2025-02-04-easier-onboarding-for-csam-scanning-tool/
