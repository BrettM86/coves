# Image uploads

Plan, not built. Drafted 2026-10-05; Q1, Q2 and Q5 were decided that day and the rest of the decisions table is still open. Delete this file when the work ships and move its lasting rules into [MEDIA.md](../MEDIA.md), [POSTS.md](../POSTS.md) and [MODERATION.md](../MODERATION.md).

Scope: backend (AppView) support for users uploading images and placing them in posts and comments. That covers the ingest pipeline, write-path validation, the image proxy, moderation integration, and what Coves must guarantee so Tidepool can federate the images. Related: [SSRF_SECURITY.md](../SSRF_SECURITY.md), [LEXICON_PUBLISHING.md](../LEXICON_PUBLISHING.md), [TEST_ARCHITECTURE.md](../TEST_ARCHITECTURE.md).

## Gaps

The read side exists. `postv2` and `comment` records can carry `social.coves.embed.images` (1 to 8 images, blob refs, optional alt text and aspect ratio). `internal/core/embeds` projects those embeds into proxy URLs, and admin moderation blocks and purges the blobs. What is missing:

1. An upload path. The AppView holds the OAuth/DPoP sessions and clients only get a sealed token, so a client cannot call `com.atproto.repo.uploadBlob` on its own PDS. No endpoint gets a blob ref for a post or comment image.
2. Write-path validation. Post image embeds are checked for shape only. Comment embeds are not checked at all and are copied into the record verbatim. The lexicon validator (`internal/validation`) runs only in tests.
3. In-body placement. There is no image facet, so images can only be a gallery under the body.
4. A proxy that matches the lexicon. GIF gets a 400, every image is re-encoded as JPEG, the EXIF orientation tag is ignored, fetched bytes are never checked against their CID, and the proxy serves any DID+CID on any PDS, not only content Coves has indexed.
5. Federation. Tidepool drops image embeds on posts and ignores comment embeds. Its renderer escapes every `![` and never emits `<img>`.

## Approach

- One server-side ingest pipeline behind a new `social.coves.media.uploadImage` procedure. It sniffs the format, enforces the pixel budget, strips location and other metadata, applies orientation, computes the aspect ratio, and uploads to the author's PDS with the author's session.
- Every image blob stays in `embed.images`. A new `social.coves.richtext.facet#image` feature places an image in the body by its index in that list. Moderation's CID extraction (`PostBlobCIDs`, `CommentImageCIDs`) stays the single source of truth, unchanged, and clients that do not understand the facet still show the gallery.
- A blob reference index (owner DID, CID, subject), written in the same transaction as indexing. The proxy serves only blobs it indexes, and moderation uses the index to find every owner of a CID.

## Lexicon constraints

`embed.images`, `embed.external`, `community.post`, `community.postv2`, `community.comment` and `richtext.facet` are published (`goat lex resolve` returned them from `_lexicon` DNS on coves.social on 2026-10-05). Live records already use them, including bridged Lemmy posts and aggregator posts. atProto evolution rules apply, so this plan makes additive changes only:

- a new facet feature `#image`;
- a new embed type `social.coves.embed.imagesWithAttachment`;
- a new `social.coves.media.uploadImage` procedure;
- changes to the moderation lexicons, which are unpublished and can still change freely.

`embed.images#image` does not change. Widening its `accept` or `maxSize`, or raising the 8-image cap, would also make Tidepool's strict validation against its vendored copy reject valid posts.

## Goals

- A signed-in user can attach up to 8 images to a post or a comment, with alt text, and place any of them in the body.
- No uploaded image keeps GPS or other EXIF/XMP metadata on the author's PDS.
- Every image Coves serves goes through the image proxy, is covered by admin removal, media blocks and CDN purge, and passes the Cloudflare edge with its CSAM Scanning Tool (see [MEDIA.md](../MEDIA.md)).
- Records written by other clients or by the bridge, including malformed or hostile ones, cannot get an image served without also being covered by moderation.
- Images in native posts and comments reach Lemmy, PieFed, Mbin and Mastodon once Tidepool does its part.

## Non-goals

- Video upload. Video blobs bypass the proxy and media blocks (`video#view` points at the hosting PDS), and this plan adds no video upload path.
- Images from external URLs in the body, such as markdown `![](https://…)`. Inline images are uploaded blobs only.
- Per-image labels or per-image removal. Labels and removals stay per record.
- Hash matching at upload time (CSAM phase 2). The pipeline and the blob index are where it will attach later.
- HEIC/AVIF input. Clients convert to JPEG first (Q6).
- Community image rules such as "no images in this community". That belongs to community content rules (not built).

## Current state (`e26f8e6`)

| Area | State |
|---|---|
| Lexicon | `embed.images`: 1 to 8 images; `image` is a blob accepting png/jpeg/webp/gif up to 10,000,000 bytes; `alt` is optional, permanently (see [LEXICON_PUBLISHING.md](../LEXICON_PUBLISHING.md)); `aspectRatio` is optional. Post embed union: images, video, external, post. Comment embed union: images, post. No image facet. |
| Post write | `posts/service.go` `CreatePost` writes `postv2` through the author's OAuth session. `embed_validation.go` `validateEmbed` checks shape only: no count limit and no check of the blob's `$type`, `ref`, MIME, alt or aspect ratio. |
| Comment write | `comment_service.go` `CreateComment` and `UpdateComment` copy `req.Embed` and `req.Labels` verbatim. Posts restrict self-label values; comments do not. |
| Uploads today | Profile avatar and banner arrive as base64 in JSON, and the client-declared MIME is trusted. Community avatar and banner use `blobs.Service`. Unfurl thumbnails go through an SSRF-guarded fetch, then `AuthorRepo.UploadBlob`. The rematerializer re-uploads blobs. `AuthorRepo.UploadBlob` and `pds.Client.UploadBlob` exist, and `blob:*/*` is requested at OAuth. |
| Indexing | Embeds are stored verbatim as JSONB (`posts.embed`, `comments.embed`). No blob table: CIDs are extracted on demand by `embeds.PostBlobCIDs` and `embeds.CommentImageCIDs`. |
| Views | `ServableEmbed` drops URLs written by authors. `HydrateView` and `HydrateCommentView` project images to `content_preview` and `content_full` proxy URLs. `record.embed` is returned verbatim with blob refs. Facets are never hydrated. |
| Proxy | `GET /img/{preset}/plain/{did}/{cid}`. Order: block check, disk cache, resolve DID, fetch with the SSRF guard, decode (jpeg/png/webp only), JPEG out. Limits: 10 MiB source, 50 MP budget, 4 decode slots, 64 requests in flight. No CID verification. Serves any DID+CID. Registered with `r.Get` only. |
| Moderation | `moderation_media_blocks` holds owner-scoped blocks, plus every-owner blocks for `illegal-content`. Media is reconciled when indexed writes or rejected events arrive. Disk and Cloudflare purges are durable and retried. An ownerless purge can name only owners still in the disk cache. Admin NSFW labels apply to posts only. |
| Dev env | Neither `docker-compose.dev.yml` nor the hermetic `docker-compose.ci.yml` sets `PDS_BLOB_UPLOAD_LIMIT`. The upstream default is believed to be 5 MiB (unverified), below the 10 MB lexicon maximum. `docker-compose.prod.yml` sets 100 MB. |

### Absorbed backlog issues

| Issue | Severity | Disposition |
|---|---|---|
| `2026-07-28-image-proxy-gif-embeds-400` | p1 | Fixed by chunk 2 |
| `2026-07-28-image-proxy-fetch-ssrf` | p1 | Appears fixed in code (`fetcher_guard_test.go`); chunk 1 verifies and closes it |
| `2026-10-03-forged-view-embeds-served-from-firehose-records` | p1 | `ServableEmbed` appears to address it; chunk 1 verifies and closes it |
| `2026-09-05-imageproxy-resolvedid-bypasses-negative-cache` | p2 | Chunk 1 |
| `2026-09-22-imageproxy-resolvedid-never-populates-cache` | p2 | Chunk 1. The disk-cache-before-resolve half is already fixed (`GetImageResolvingPDS`); `ResolveDID` still writes no cache |
| `2026-09-01-imageproxy-replace-disintegration-imaging-with-x-image-draw` | p3 | Chunk 2 |
| `2026-09-29-rematerializer-sibling-post-escapes-owner-scoped-media-block` | p2 | Made visible by the blob index (chunk 3); not fixed here |
| `2026-09-01-imageproxy-do-not-embed-libvips-decision` | p3 | Binding constraint: the pipeline stays pure Go, no cgo |

## Upload endpoint

`POST /xrpc/social.coves.media.uploadImage`. The name is verb–noun, following the moderation precedent for new namespaces.

- Auth: `RequireAuth` with the user's OAuth session. If `blob:*/*` was not granted, fail with `MissingBlobScope`, not a warning.
- Input: raw image bytes with `Content-Type: image/*`. Not base64 JSON, which inflates by a third and doubles server memory. A dedicated `reqbody` tier caps the body at 10,000,000 bytes, the same size as `LimitImage`.
- Output: `{"blob": <blob ref>, "mimeType": "...", "size": n, "aspectRatio": {"width": w, "height": h}}`. The client copies `blob` and `aspectRatio` into `embed.images#image`. Clients should send the server-computed aspect ratio, because it is post-orientation and avoids layout shift.
- Errors, as codes: `ImageTooLarge`, `UnsupportedImageFormat`, `ImageDimensionsTooLarge`, `InvalidImage`, `UploadRateLimited`, `MissingBlobScope`, `PDSUploadFailed`.
- Aggregators can call it when their auth path supplies an author repo. Otherwise out of scope (Q9).

## Ingest pipeline

New package `internal/core/media`. Steps, in order:

1. Sniff the format from magic bytes. The client `Content-Type` is advisory only. Allow JPEG, PNG, WebP and GIF. Reject HEIC, AVIF, SVG and anything else with `UnsupportedImageFormat`.
2. Check the budget before decoding. Read dimensions with `DecodeConfig`. Reject anything over 50 MP (the proxy's budget) or under 1×1. For GIF, also cap frames × pixels so a small file cannot unpack into hundreds of full frames.
3. Decode fully to prove the image is valid, under a semaphore separate from the proxy's so uploads cannot starve serving.
4. Sanitize:
   - JPEG: remove APP1 (EXIF/XMP), APP13 (IPTC) and COM segments losslessly. If the EXIF orientation was not 1, rotate the decoded pixels and re-encode at quality 92 instead. Keep the ICC profile (APP2); dropping it shifts colours.
   - PNG: drop the `eXIf`, `tEXt`, `iTXt`, `zTXt` and `tIME` chunks losslessly.
   - WebP: drop the `EXIF` and `XMP ` chunks and clear the matching VP8X flags. If orientation was not 1, decode and re-encode to JPEG; the standard library cannot encode WebP.
   - GIF: drop comment and application extensions other than NETSCAPE looping.
5. Compute the aspect ratio from the post-orientation dimensions.
6. Upload the sanitized bytes with `AuthorRepo.UploadBlob(ctx, bytes, sniffedMIME)` under the caller's own DID. Never upload to a different repo: re-uploading under another DID escapes owner-scoped blocks, as the rematerializer issue shows.

The output must stay within the 10,000,000-byte lexicon limit. Re-encoding happens only to rotate, so output can exceed input only marginally; if it exceeds the limit, return `ImageTooLarge`.

Orphans: the PDS keeps an uploaded blob as temporary until a record references it and garbage-collects it otherwise (verify against the PDS version we run). Abandoned uploads then need no Coves-side cleanup.

### Existing upload paths

The profile avatar and banner (`user/update_profile.go`) and the community avatar and banner (`communities/service.go`) keep their endpoints and size caps, but run their bytes through the same sniff, budget and sanitize steps. That ends trust in the client-declared MIME and strips EXIF from avatars. Unfurl thumbnails (`posts/service.go` `uploadThumbnail`) also go through the pipeline in place of the Content-Type check in `blobs.FetchImageForURL`. The pipeline becomes the only place bytes are inspected before an upload, and the place CSAM phase-2 matching will attach.

### Abuse limits

A per-DID rate limit on `uploadImage`: proposed 30 uploads per 10 minutes and 300 MB per day (Q7). Logs carry the DID, size and format only, never bytes or tokens.

## Write-path validation

Applies to posts and comments, on create and update.

- Run the lexicon validator (`validation.NewLexiconValidator`) on the final record before it is written to the PDS, for `postv2` and `comment`. Shape checks then come from one source, the published schema, and comments are covered for the first time.
- Check separately what the lexicon cannot express:
  - every `embed.images[].image` is a blob object with `$type: blob`, a CID that decodes, a `mimeType` in the accept list, and a size within the limit;
  - `#image` facets are consistent with the embed;
  - comment self-label values come from the set posts allow (nsfw, spoiler, violence).
- Return error codes, not free text: `InvalidEmbed`, `TooManyImages`, `InvalidImageFacet`, `InvalidSelfLabel`.
- The PDS rejects a record whose blob was not uploaded to that repo, so Coves does not track uploads per session.

## Image facet

`social.coves.richtext.facet#image` is a new def in `richtext/facet.json` and a new ref in the open `features` union.

```json
"image": {
  "type": "object",
  "description": "Places embed.images.images[index] at this position in the body. The covered text is the fallback that readers without image support render.",
  "required": ["index"],
  "properties": {"index": {"type": "integer", "minimum": 0, "maximum": 7}}
}
```

Rules, enforced on write and re-checked on serve:

- The record's embed is `social.coves.embed.images` or `social.coves.embed.imagesWithAttachment`, and `index` is below the length of its image list. With `imagesWithAttachment`, `index` refers to that embed's `images` list.
- `#image` is the only feature in its facet, and each index is placed at most once.
- The range does not lie inside a `#code` or `#codeBlock` range.
- The covered text should be the alt text, or `[image]` when there is none. That is the client's convention; the server only requires a non-empty range.
- Images that are not placed render as a gallery after the body. A post where every image is placed has no gallery.

The facet holds an index, not a blob, for two reasons. `PostBlobCIDs`, `CommentImageCIDs`, `ServableEmbed`, `HydrateView`, media blocks, reconciliation and CDN purge all keep reading blobs from one place, with no change to moderation. And clients and bridges that skip unknown facets still show every image as a gallery, so nothing silently disappears.

### Views

- Facets already pass through in `record`. Clients resolve `#image.index` against `embed.images#view.images[index]`.
- When `ServableEmbed` or `HydrateView` drops the image embed (malformed, or all-or-nothing projection fails), clients render the facet's covered text. The view's lexicon description states this.
- `sanitizeFacets` on the firehose path keeps `#image` facets as stored. Clients treat an out-of-range index as fallback text, so the indexer does not rewrite records.
- A blocked image keeps its URL in the view, and the proxy returns 404 with `no-store` (current behaviour). Clients show a "removed image" placeholder on 404.

## Images with a link card or quote

Decided 2026-10-05 (Q2): a post can carry images and a link card, or images and a quoted post; a comment can carry images and a quoted post. A record has one `embed` field, so this is a new embed type, `social.coves.embed.imagesWithAttachment`, added to the post and comment embed unions. It has the same shape as Bluesky's `recordWithMedia` and is additive.

```json
"main": {
  "type": "object",
  "required": ["images", "attachment"],
  "properties": {
    "images": {"type": "ref", "ref": "social.coves.embed.images"},
    "attachment": {"type": "union", "refs": ["social.coves.embed.external", "social.coves.embed.post"]}
  }
}
```

The `#view` mirrors it: `images#view` plus `external#viewExternal` or `post#view`.

- Comments reject an `external` attachment, keeping the rule that comments have no link cards.
- `PostBlobCIDs` and `CommentImageCIDs` read the images, the external thumb and `external.images` from the new type, so they stay the one place moderation gets CIDs from. Removal, reconciliation and purge tests cover the new type explicitly.
- `ServableEmbed` and `HydrateView` project both parts. The quote redaction rules for removed quoted posts apply unchanged to the attachment.
- `enhanceExternalEmbed` fills in the thumbnail of a nested `external`.
- Federation (Tidepool T2): Lemmy shows one URL per post, so the link is the post URL and the images go inline in the body.
- Built as chunk 11 of this plan, not deferred to a later release.

## Proxy changes

The libvips decision stands: everything stays pure Go and in process.

1. DID resolution. The disk cache is already checked before the DID is resolved. Write the identity cache on a successful `ResolveDID` and apply the negative cache to it. This closes both p2 issues.
2. Decode and resize. Replace `disintegration/imaging` with `golang.org/x/image/draw`. Apply the EXIF orientation when decoding, because blobs from the bridge, from other clients, and uploads that predate the pipeline are not sanitized. Decode GIF: previews and thumbnails use the first frame; animated `content_full` is Q3.
3. CID verification. Hash the fetched bytes (sha256, raw codec) and compare with the requested CID before processing. A mismatch returns 502 and is never cached. Without this a PDS can serve different bytes under a CID, and CID-keyed blocks, the cache and future hash matching mean nothing.
4. Serve only indexed blobs. After the block check, the proxy requires an active blob reference for (owner DID, CID). Unknown pairs get 404 with `no-store`. This stops `img.coves.social` working as a free, Cloudflare-fronted CDN for any blob on any PDS, which today is an abuse and CSAM-liability vector. Views are built from indexed rows, so every URL Coves emits is already indexed. The block check stays first, so a blocked blob returns 404 for being blocked even if its reference is gone.
5. HEAD. Nothing in the repo uses `middleware.GetHead`, so a HEAD request probably gets 405. HEAD must return the same status and headers as GET, without a body (requirement C5 below).
6. URL shape. The canonical URL stays `/img/{preset}/plain/{did}/{cid}`. Tidepool bakes these URLs into stored ActivityPub payloads, so the shape becomes a documented contract (C1).

## Moderation

### Blob reference index

A table `blob_references(owner_did, blob_cid, subject_uri, kind, active)`, where `kind` is `post_image`, `comment_image`, `external_thumb`, `video_thumb`, `avatar` or `banner`.

- Writes: the post, comment, profile and community consumers write it in the same transaction as the content row, using the existing extractors (`PostBlobCIDs`, `CommentImageCIDs`, and the avatar/banner CID columns). An edit replaces the subject's references; a delete deactivates them.
- Backfill: a one-off migration fills it from existing `posts.embed`, `comments.embed`, `users` and `communities` rows.
- Ownership follows the existing rules: `postv2` → author; legacy `post` → community; comment → commenter (see [POSTS.md](../POSTS.md)).
- Uses: the proxy's serve-only-indexed check; ownerless purge (below); locating every subject that uses a CID for a takedown; and making the rematerializer sibling-post case detectable, without fixing it here.

### Ownerless purge

This plan owns the open decision on purging every owner's copy after an `illegal-content` block. Today the purge names the subject's owner plus every owner whose copy is in this instance's local image cache. That cache is the only record of other owners, and it lives far shorter than the edge copy: entries expire after `IMAGE_PROXY_CACHE_TTL_DAYS` (default 30) and are evicted least-recently-read once the cache passes `IMAGE_PROXY_CACHE_MAX_GB` (default 10), while with purge enabled the edge may keep a URL for a year (`s-maxage=31536000`). Edge-popular images leave the disk cache first because their hits stop reaching the origin, a failed cache write leaves that owner unnameable from the start, and an owner cached only through another AppView is never named.

The options were: cap `s-maxage` at the cache TTL (covers TTL expiry but not eviction or failed writes); a durable (owner, CID) index in Postgres; or Cloudflare Cache-Tag purge by CID (plan-dependent). This plan takes the index: an `illegal-content` removal purges every (owner, CID) in `blob_references`. The decision has to be in place before CDN purge is enabled in production.

### Behaviour checks

These are tests, not new code:

- An image placed with `#image` is blocked on removal exactly like a gallery image (same CIDs).
- An edit that adds an image to a removed post or comment gets an owner-scoped block through existing reconciliation, tested with an upload made through the new endpoint.
- Restoring lifts the blocks, and the blob references stay active throughout.

### NSFW on comments

Admin moderation scoped NSFW labels to posts and left comment labels for later. Comment images make them necessary. Current state:

| Layer | State |
|---|---|
| Lexicon | `comment.labels` accepts `com.atproto.label.defs#selfLabels`, so an author can self-label |
| Backend writes | Comment self-label values are not checked; posts restrict them to nsfw, spoiler and violence |
| Admin labels | `validateLabelRequest` in `moderation/label.go` rejects comment URIs, so `contentLabels` in `commentView.moderation` is always empty |
| Clients | The web client preserves comment labels on edit. Neither client appears to blur a comment on its labels (from a grep, not checked in the UI) |
| Tidepool | `BuildNote` emits no `sensitive` (T5) |

The work:

1. Validate comment self-labels the same way as posts (chunk 8).
2. Accept comments in `labelContent` and `retractContentLabel`, fill `contentLabels` in `commentView.moderation`, and keep label actions in the admin-only log. The moderation lexicons are unpublished, so this is a free change (chunk 10).
3. Clients blur a comment's body and images when its self-labels or `moderation.contentLabels` contain nsfw.
4. Tidepool sets `sensitive` on the `Note` (T5).

### Out of scope

- Reports that target one image. Reports target the record URI.
- Blocking the same image in an author's other, non-removed posts. Removal stays per subject, as decided for admin moderation ([MODERATION.md](../MODERATION.md)).
- Admin removals reaching copies already federated (Tidepool T7).

## Configuration

- Set `PDS_BLOB_UPLOAD_LIMIT` to the production value in `docker-compose.dev.yml` and `docker-compose.ci.yml`. Otherwise the 5 to 10 MB uploads the lexicon allows fail only locally and in E2E.
- New environment variables (add each to the clear list in `internal/config/testing.go`):

  | Variable | Default |
  |---|---|
  | `MEDIA_UPLOAD_MAX_CONCURRENT` | 4 |
  | `MEDIA_UPLOAD_RATE_PER_10MIN` | 30 |
  | `MEDIA_UPLOAD_BYTES_PER_DAY` | 300 MB |

- No public hosts. Uploads, DID resolution and proxy fetches all use the configured local PLC and PDS (the 2026-09-05 incident rule).

## Federation

### Coves requirements

#### C1. URL contract

The contract is the path shape `/img/{preset}/plain/{did}/{cid}` (`content_full` for images, `content_preview` for thumbnails), built from DID and CID with no network call. Document it in [MEDIA.md](../MEDIA.md). Tidepool translates when it enqueues, inside its gate transaction, and stores the payload verbatim, so it cannot resolve a PDS endpoint. Renaming or reshaping a preset needs a redirect from then on.

- No hard-coded host. Tidepool takes the base from `COVES_MEDIA_BASE_URL`, set to its paired AppView's `IMAGE_PROXY_BASE_URL`.
- Other Coves instances. The blob lives on the author's PDS, not on any instance, so any AppView that indexes a record can serve its images under its own host. A user from another Coves instance posting into a coves.social community is indexed by coves.social, so a coves.social URL works for that image; the other instance serves the same blob under its own URL. Tidepool federates only posts it accepted for communities its own AppView hosts, and that AppView indexes those posts and their comments.
- Invariant: Tidepool emits an image URL only for records its paired AppView indexes. A different AppView's bridge uses that AppView's base URL.
- Moderation is per AppView. A coves.social removal blocks the coves.social URL only. Another instance's copy follows that instance's decisions, or coves.social's once federated labels and trust configuration ship (not built; see [MODERATION_LABELS.md](MODERATION_LABELS.md)).

#### C2. Tidepool re-synced before Coves publishes

Lexicon changes stay additive. Tidepool validates outbound `postv2` strictly against its vendored lexicons (`accept/engine.go` `lexiconValid`), and a failure is a permanent `lexicon-invalid` rejection. A new union member passes as unknown, but once vendored it is validated, so the `#image` facet and `imagesWithAttachment` must land in Tidepool's vendored copy in the same release. Fix the existing drift at the same time: Coves `postv2` content is 500,000 bytes / 50,000 graphemes, Tidepool's copy is 100,000 / 10,000, so long posts already fail to federate.

#### C3. Remote fetchers can read images

Lemmy (pict-rs) and Mastodon fetch the image URL themselves. Serving only indexed blobs creates a race: if Tidepool delivers before the Coves AppView has indexed the post, the remote fetch gets 404 and the remote side caches a broken thumbnail. Both consume the same Jetstream and nothing orders them. Tidepool enqueues the `Create` in the same transaction as the acceptance record and its worker polls every second, so the window is real. With an open proxy there is no race, because the blob is on the PDS before the record commits; the race is the price of serving only indexed blobs.

Checked from source on 2026-10-05:

| Platform | Post attachment that got a 404 on first fetch | Inline body image | Does a later `Update` fix it? |
|---|---|---|---|
| Lemmy 0.19.20 (Tidepool's E2E version) | Permanent. Lemmy fetches once in the background at receive time and never checks the status. It records `url_content_type = text/plain` and no thumbnail, nothing retries, and lemmy-ui shows a link icon. | Heals: loaded at view time, and pict-rs does not cache failures. | Yes, but only with a new activity id and a `Page.updated` later than the stored one. Tidepool never sets `updated`, so every Update it sends today is rejected (T10). |
| Mastodon 4.7.3 | Heals: retries once 30 s to 10 min later (`RedownloadMediaWorker`) and downloads lazily through `/media_proxy`. | Not applicable (`content` is dropped for `Page`). | No |
| PieFed 1.7.17 | Permanent. It classifies the post with a synchronous HEAD request; an error status makes it a link post. | Heals (hotlinked; inferred). | No, unless the URL changes |
| Mbin 1.10.1 | Permanent until an Update arrives. Downloads synchronously and never retries. | Not checked | Yes. Every Update re-downloads it |

Recommended mitigation (Q8) is a readiness gate in Tidepool (T11). Before the first delivery of an object carrying Coves image URLs, the worker requests each URL from the paired AppView. On a 404 it parks the delivery with a short delay, as it already parks replies for causal ordering, inside a bounded budget (for example 2 minutes). When the budget runs out it delivers anyway: a post with a broken thumbnail beats a missing post. One mechanism covers every platform, and the probe also warms the proxy cache right before the remote fetch. The alternative, dropping serve-only-indexed and keeping the open proxy, removes the race but keeps the abuse vector. Not recommended.

#### C4. Titleless image posts

`postv2.title` is optional ("optional for media-only posts"), but Tidepool admission requires a title (`title-required`), so image-only posts never federate. Decided 2026-10-05 (Q5): no change for now. Titles stay optional on Coves, and titleless posts stay local.

#### C5. HEAD

PieFed classifies posts with HEAD, so a 405 would make every Coves image post a link post, race or not. HEAD returns GET's status and headers without a body (chunk 1). Confirm with `curl -I` on a live image URL, in case Cloudflare turns HEAD into GET.

### Tidepool work

Belongs to Tidepool's own plan.

| # | Work | Current Tidepool code |
|---|---|---|
| T1 | Re-sync lexicons and update the hard-coded caps (`materialize/blobs.go`, `materialize/posts.go`, `apobject.MaximumContent*`). Raise `MAX_BLOB_BYTES` from 5 MiB to 10 MB. | Drift already live |
| T2 | Outbound `Page`: the first image becomes `attachment: [{type: Image, mediaType, url, name: alt}]`, which Lemmy uses as the post's single URL. Other images render at their facet position, or appended, as markdown `![alt](url)` in `source` and `<img>` in `content`. | `apobject/object.go` drops images |
| T3 | Outbound `Note` (comments): images inline in `source` and `content` (Lemmy comments have no attachments), plus `attachment[]` for Mastodon. | `BuildNote` ignores `embed` |
| T4 | Renderer: a deliberate image path in `apobject/richtext.go` that emits `![…](…)` and `<img src>` only for proxy URLs Tidepool built, never for URLs written by authors. Update `TestBuildNote_LiteralExclamationBeforeLinkCannotFormImage` to pin that distinction instead of deleting it. | All `![` escaped |
| T5 | `sensitive` on comments, and from admin labels once those federate. | Post self-label only |
| T6 | Inbound: comment Note attachments and inline markdown images become `embed.images` plus `#image` facets, so Lemmy inline images round-trip. Sniff the MIME instead of trusting `Content-Type`. | Comments carry no embed; inline images degrade to links |
| T7 | Send Coves admin removals as Lemmy `Remove`/`Delete` so pict-rs drops its copy. Part of the federated-moderation milestone, but it matters more once images flow out: a CSAM removal on Coves otherwise leaves the copy on lemmy.world. | Not sent |
| T8 | Blob GC and refcounting for bridged blobs, which are orphaned on delete or edit and stay public through `getBlob`. | Listed in `FOLLOWUPS.md` |
| T9 | The E2E image-post test against real Lemmy that `tasks/18-e2e-deploy.md` makes mandatory, extended to comments and inline placement. | Not written |
| T10 | Set `Page.updated` on every Update, or Lemmy 0.19.20 rejects it (`Post::insert_apub` updates only when the timestamp is strictly newer). This already breaks federated post edits, images or not. | `apobject/object.go` sets only `published` |
| T11 | The readiness gate from C3. | Delivery about 1 s after enqueue (`next_attempt_at = now()`) |

## Client dependencies

- Both clients: compose with upload, alt-text entry, inline placement and gallery fallback; `#image` facet rendering; `embed.imagesWithAttachment` rendering; placeholders when the proxy returns 404; blur for NSFW comments.
- Mobile: convert HEIC to JPEG before upload, and fix the `2026-09-28-mobile-compose-media-lows` issues that touch upload limits.
- Frontend: `2026-08-26-markdown-external-images-autoload-tracking`, which inline images make more visible.

## Chunks

| # | Chunk | Depends on |
|---|---|---|
| 1 | `ResolveDID` identity and negative caching; HEAD support (C5); verify and close the SSRF and forged-view p1 issues; set `PDS_BLOB_UPLOAD_LIMIT` in the dev and CI compose files | none |
| 2 | Proxy decode rewrite: `x/image/draw`, EXIF orientation, GIF decode, CID verification | 1 |
| 3 | Blob reference index: migration, consumer writes, backfill, ownerless purge through the index | none |
| 4 | Proxy serves only indexed blobs | 2, 3, Q8 |
| 5 | `internal/core/media` pipeline (sniff, budget, sanitize, aspect ratio) with unit tests on real fixture images, including EXIF GPS, every orientation, animated GIF and decompression bombs | none |
| 6 | `uploadImage` lexicon and handler, rate limits, E2E upload to the local PDS | 5 |
| 7 | Profile, community and unfurl uploads moved onto the pipeline | 5 |
| 8 | Write-path validation: lexicon validator at runtime for posts and comments, blob-ref checks, comment self-labels | none |
| 9 | `#image` facet: lexicon, write validation, view contract, E2E (upload, post with an inline image, fetch through the proxy, admin remove, 404 and purge; the same for a comment) | 6, 8 |
| 10 | Admin NSFW on comments | Q4 |
| 11 | `embed.imagesWithAttachment`: lexicon, write validation, CID extractors, views, unfurl, moderation tests, E2E | 9 |
| 12 | Federation contract in MEDIA.md (C1); lexicon publish after the Tidepool re-sync (C2) | 9, 11 |

Every chunk is tested at the tiers in [TEST_ARCHITECTURE.md](../TEST_ARCHITECTURE.md). E2E runs against the local PDS, PLC and Jetstream with real accounts, and a skipped test is a failure.

## Decisions

| # | Question | Answer or recommendation |
|---|---|---|
| Q1 | Does "inline images" mean images placed in the body text, or attached images shown with the post? | Decided 2026-10-05: both. Attach through `embed.images`; place in the body with the `#image` facet |
| Q2 | Can a post with images also have a link card or a quoted post? | Decided 2026-10-05: yes, in this plan, as the last chunk |
| Q3 | Animated GIF: does `content_full` serve the original GIF bytes or a JPEG of the first frame? | Recommended: the original GIF bytes for `content_full` when the source is a GIF under the size cap. Previews use the first frame |
| Q4 | Admin NSFW labels on comments in this plan? | Recommended: yes (chunk 10) |
| Q5 | Titleless image posts and federation: should Coves require a title, or Tidepool derive one? | Decided 2026-10-05: neither for now. Titleless posts do not federate |
| Q6 | Accept HEIC/AVIF on the server? | Recommended: no. Clients convert. No pure-Go decoders exist, and libvips is excluded |
| Q7 | Upload limits per DID | Recommended: 30 per 10 minutes, 300 MB per day, adjustable by environment variable |
| Q8 | The proxy-versus-federation indexing race (C3) | Lemmy, PieFed and Mbin never retry a failed post-image fetch (checked from source). Recommended: a readiness gate in Tidepool (T11), keeping serve-only-indexed |
| Q9 | Can aggregators (kagi-news and others) upload images? | Recommended: not in v1, unless an aggregator needs it now |
| Q10 | Downscale large images at upload (for example a 4096 px long edge) or keep full resolution? | Recommended: keep full resolution up to the 50 MP budget. Forum screenshots need it, and the proxy handles display sizes |
