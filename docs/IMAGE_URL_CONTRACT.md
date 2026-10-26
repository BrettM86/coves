# Image URL contract

An image URL has the shape `<base>/img/<preset>/plain/<did>/<cid>`. `blobs.HydrateImageURL` builds proxy URLs from the owner's DID, blob CID and preset without resolving a PDS or making a network call. The AppView's base is configured by `IMAGE_PROXY_BASE_URL` (or its CDN override when configured). Tidepool uses `COVES_MEDIA_BASE_URL`, set to the `IMAGE_PROXY_BASE_URL` of its paired AppView. `content_full` is the full image preset; `content_preview` is used for previews.

The supported presets and their maximum or target dimensions are:

| Preset | Dimensions (px) | Fit | JPEG quality |
| --- | --- | --- | --- |
| `avatar` | 1000 × 1000 | cover | 85 |
| `avatar_small` | 360 × 360 | cover | 80 |
| `banner` | 640 × 300 | cover | 85 |
| `content_preview` | width up to 800, proportional height | contain | 80 |
| `content_full` | width up to 1600, proportional height | contain | 90 |
| `embed_thumbnail` | 720 × 360 | cover | 80 |

Output format depends on the source: PNG remains PNG; JPEG and WebP produce JPEG; GIF `content_full` serves the original GIF, while other GIF presets produce a JPEG of its first frame. The JPEG quality column applies only to JPEG output.

## AppView ownership and serving

Tidepool emits image URLs only for records indexed by its paired AppView. Another AppView's bridge uses that AppView's base URL. A blob resides on its author's PDS, so separate AppViews that index the same record can each serve it under their own host. Moderation decisions apply per AppView.

The proxy serves an owner DID and CID only while that pair has an active indexed blob reference. It checks moderation first and canonicalises the CID before block and reference checks. An unreferenced pair returns 404 with `Cache-Control: no-store`, including for cached images and matching `If-None-Match` requests. A reference lookup failure returns 503, including on cache hits and conditional requests.

## Delivery ordering and URL stability

If Tidepool delivers a federated image post before the paired AppView indexes its record from the firehose, a remote fetcher gets 404. Tidepool's readiness gate (T11) must probe each image URL before the first delivery and delay delivery briefly on 404, within a bounded budget. Some remote platforms do not retry a failed thumbnail fetch.

Tidepool stores these URLs in ActivityPub payloads. Renaming or reshaping a preset requires a redirect from the old URL from then on so previously delivered URLs continue to work.
