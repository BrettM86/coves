# PRD: Default avatars

**Status:** Draft. Nothing in this document is implemented. D1–D15 decided 2026-10-28. No questions are open.

**Date:** 2026-10-28

**Scope:** A sea-themed default picture for every user who has no avatar, chosen from their DID, served by the AppView through `img.coves.social`, and drawn the same way by `coves-frontend` and `coves-mobile`. Covers the image set, the assignment rule, the serving route, and both clients.

**Related:** [Media](MEDIA.md), [Image URL contract](IMAGE_URL_CONTRACT.md)

## 1. Summary

A user without an avatar currently gets a different fallback on each client. The web client draws DiceBear initials seeded by the `alt` text (`coves-frontend/src/lib/ui/generic/Avatar.svelte`). The mobile client draws the first letter of the name on one of six colours chosen by `name.hashCode` (`coves-mobile/lib/widgets/user_avatar.dart`). Neither keys on the DID, so the same user looks different on web and mobile, and changes look when their display name changes.

This PRD replaces both with one of 39 flat illustrations (anchor, octopus, puffin, …) on a tinted circle. The picture and the tint both come from a hash of the DID, so every client shows the same default for a user, and it never changes. Nothing is written to the user's PDS: the default is a display fallback only.

## 2. Goals and non-goals

### Goals

- Every user without an avatar shows the same default picture and tint on web and mobile.
- The assignment is computed from the DID alone, with no API call and no stored state.
- The images are served by the AppView under `/img/`, so production traffic goes through `img.coves.social` and the Cloudflare edge like every other image.
- The images work in local development, where the image proxy is disabled.

### Non-goals

- A picker that lets users choose a default. When it is built, a chosen picture is uploaded to the user's PDS as a normal avatar blob (D9). Its design is left to that work.
- Default pictures for communities and for generic icon rows. On the web they keep the DiceBear fallback in `Avatar.svelte`; on mobile `CommunityAvatar` is unchanged.
- Authors inside embedded Bluesky posts. They keep the Bluesky-styled fallback (D12).
- Changing what Tidepool sends to ActivityPub. Actors without an avatar keep sending no `icon`.
- Writing anything to a user's PDS or the AppView database.

## 3. Decisions

- **D1 (2026-10-28). Display fallback only.** The default is drawn by clients when a user's `avatar` is empty or fails to load. The API keeps returning no avatar for these users.
- **D2 (2026-10-28). The image set is the 39 pictures chosen in review**, listed in §4.1. The source PNGs come from `scripts/default-avatars/` (Codex image generation, then `fit_to_circle.py`).
- **D3 (2026-10-28). Transparent images on a client-drawn tint.** Each image has a transparent background. The client draws a filled circle in a tint chosen from the DID and places the image on top. Tint and picture are chosen independently, so 39 pictures × 8 tints give 312 combinations from 39 files.
- **D4 (2026-10-28). Assignment uses SHA-256 of the DID** (§4.2). Clients compute it locally.
- **D5 (2026-10-28). The pool is frozen as `v1`.** Adding, removing or reordering a picture would move most users to a different default, so the v1 list and its order never change. A change to the set is a new version with a new path, adopted deliberately.
- **D6 (2026-10-28). Images are keyed by slug, not by index.** URLs name the picture (`octopus.webp`), so a future picker can store a slug that survives list changes.
- **D7 (2026-10-28). The AppView serves the images from files embedded in the binary.** No PDS, no blob store, no database row.
- **D8 (2026-10-28). One size: 512 × 512 WebP.** 512px covers the largest avatar the clients draw (the profile header) on a 3× screen. The whole set at quality 90 is about 865 KB, about 22 KB per picture. Each client downloads a picture once and caches it.
- **D9 (2026-10-28). A future picker stores the choice as a real avatar in the user's PDS.** Out of scope here; recorded so this design does not block it.
- **D10 (2026-10-28). The tint palette in §4.1 is approved**, after review of a contact sheet of all 39 pictures on each tint at 72px and 24px.
- **D11 (2026-10-28). The 1254px PNG masters stay out of git.** The primary remote (tangled) has no Git LFS. Bretton keeps them locally, with a second copy and a `SHA256SUMS` file in the production PDS S3 bucket (`PDS_S3_BUCKET`, an OVH Object Storage bucket) under the prefix `coves-assets/default-avatars/masters/`. The PDS reads only its own keys, so the prefix does not affect it.
- **D12 (2026-10-28). Embedded Bluesky posts keep their Bluesky fallback.** The cards in `coves-mobile/lib/widgets/bluesky_post_card.dart` style the author avatar as Bluesky chrome on purpose, and do not show a Coves default.
- **D13 (2026-10-28). An empty DID draws a plain Mist (`#DCE0EA`) circle with no picture.** It should almost never happen. Hashing `""` would instead give every such author the same picture.
- **D14 (2026-10-28). Every Coves user avatar passes a DID.** A caller without one gets it from the API, not a second fallback.
- **D15 (2026-10-28). The team settings page is deferred.** `UserAutocomplete.svelte` and the moderator list on `routes/c/[handle=handle]/settings/team/` still use the legacy Lemmy-shaped client, and Coves has no user-search or team-management XRPC to carry a DID. They keep DiceBear until that page moves to Coves endpoints, which is separate work.

## 4. Design

### 4.1 The v1 pool

The pool is this ordered list. Index is the position used by §4.2. Clients hard-code it.

| # | Slug | # | Slug | # | Slug |
| --- | --- | --- | --- | --- | --- |
| 0 | `anchor` | 13 | `narwhal` | 26 | `sea-otter` |
| 1 | `beach-bucket-and-spade` | 14 | `nudibranch-sea-slug` | 27 | `sea-turtle` |
| 2 | `branch-of-coral` | 15 | `ocean-wave` | 28 | `sea-urchin` |
| 3 | `clownfish` | 16 | `octopus` | 29 | `seahorse` |
| 4 | `compass` | 17 | `oyster-with-a-pearl` | 30 | `seal` |
| 5 | `dolphin` | 18 | `pelican` | 31 | `ships-wheel` |
| 6 | `eel` | 19 | `penguin` | 32 | `snorkel-mask` |
| 7 | `hermit-crab-in-a-shell` | 20 | `pufferfish` | 33 | `spiral-sea-snail-shell` |
| 8 | `jellyfish` | 21 | `puffin` | 34 | `squid` |
| 9 | `kelp-frond` | 22 | `sand-dollar` | 35 | `starfish` |
| 10 | `life-ring` | 23 | `sandcastle` | 36 | `swordfish` |
| 11 | `lighthouse` | 24 | `scallop-shell` | 37 | `treasure-chest` |
| 12 | `message-in-a-bottle` | 25 | `sea-anemone` | 38 | `whale` |

`ships-wheel` is renamed from the generated file `ship-s-wheel.png`.

The tint palette is this ordered list of 8 colours, used in both light and dark themes:

| # | Name | Hex |
| --- | --- | --- |
| 0 | Peach | `#F3D9C7` |
| 1 | Sage | `#D8E8D4` |
| 2 | Sky | `#CFE3EC` |
| 3 | Lavender | `#E6DDF0` |
| 4 | Sand | `#F4E7C1` |
| 5 | Seafoam | `#D5E6E3` |
| 6 | Rose | `#EED6DA` |
| 7 | Mist | `#DCE0EA` |

These values are approved (D10).

### 4.2 Assignment rule

```
digest     = SHA-256(UTF-8 bytes of the DID string, exactly as returned by the API)
pictureIdx = big-endian uint32 of digest[0..4]  mod 39
tintIdx    = big-endian uint32 of digest[4..8]  mod 8
```

The DID is not normalised. Both clients and any Go helper must pass these test vectors:

| DID | First 8 digest bytes | Picture | Tint |
| --- | --- | --- | --- |
| `did:plc:ewvi7nxzyoun6zhxrhs64oiz` | `099e4ea96cd62c05` | `ocean-wave` (15) | 5 Seafoam |
| `did:plc:z72i7hdynmk6r22z27h6tvur` | `f5b57a549b7ad681` | `pufferfish` (20) | 1 Sage |
| `did:web:coves.social` | `e5417994bd0f54cd` | `sandcastle` (23) | 5 Seafoam |

Tests must assert these literal values, not compare one client's output against another's.

### 4.3 Serving

Route: `GET, HEAD /img/default-avatars/{version}/{slug}.webp` on the AppView. Only `v1` exists.

- The 39 files live in the coves repo under the AppView's source tree and are embedded with `go:embed`. The route is registered whether or not the image proxy is enabled, so it works in development where the proxy is off.
- A known slug returns 200, `Content-Type: image/webp`, an `ETag`, and `Cache-Control: public, max-age=31536000, immutable`. A request whose `If-None-Match` matches the `ETag` gets 304 with the same `Cache-Control`. These files never change under a versioned path and can never be removed by moderation, so they are exempt from the one-day rule in [MEDIA.md](MEDIA.md). MEDIA.md gets a line recording the exception.
- An unknown slug or any version other than `v1` returns 404 with `Cache-Control: no-store`. The `{version}` parameter exists so the handler, not chi's default not-found response, answers these.
- The route takes no query parameters and does not read the request's `Origin`.

Production traffic path:

- `img.coves.social` already forwards `/img/*` to the AppView, and its Cloudflare cache rule respects origin headers. No Caddy or Cloudflare change is needed.
- `coves.social/img/*` already returns a permanent redirect to `img.coves.social`.
- In development `Caddyfile.dev` already sends `/img/*` to the AppView.

### 4.4 Client URL

Clients build the URL from the instance origin they already have:

- Web: `PUBLIC_INSTANCE_URL` origin + `/img/default-avatars/v1/{slug}.webp`.
- Mobile: `EnvironmentConfig.apiUrl` + the same path.

In production this resolves through the 301 to `img.coves.social`, once per client, and the redirect itself is cached. The web page CSP (`coves-frontend/src/lib/server/security-headers.ts`) already allows the instance origin and any `https:` source for `img-src`, which covers both the same-origin URL and the redirect target. No new client configuration is added.

### 4.5 Rendering

Both clients replace their current fallback with this:

1. Compute `pictureIdx` and `tintIdx` from the DID (§4.2).
2. Draw a filled circle in the tint, the size of the avatar.
3. Draw the picture on top, scaled to fill the circle. The images were fitted so nothing pokes out of the circle at full size.
4. Until the picture loads, or if it fails to load, show the tinted circle alone. No initials, no spinner.

The default is used when the user's `avatar` is missing, empty, or fails to load. A real avatar always wins.

The avatar components need the user's DID:

- Web: `Avatar.svelte` gains an optional `did` prop. Passing `did` means the avatar is a user's, and its fallback is the default avatar. Without `did` (communities, `Entity`, `CommonItem`, `CommandItem`) it keeps the DiceBear fallback. `EntityHeader` passes `did` only when it renders a user. The default picture is drawn by its own branch (a tinted element plus an `img`), not by putting its URL into `url`: `withPreset` logs a warning for non-proxy URLs and would repeat the URL in `srcset`.
- Mobile: `UserAvatar` gains a required `did` parameter. `ProfileHeader` and `EditProfileScreen` pass the DID and drop their `fallbackIcon` and `fallbackColor` overrides, so a user's own profile shows their default. The Bluesky cards keep `fallbackColor` and `fallbackTextColor` (D12) and never draw the default. `fallbackIcon` is deleted once no caller passes it.

Every Coves user call site passes the DID from the author or profile view. Two web callers have no DID today: `UserAutocomplete.svelte` and the community team settings page (`routes/c/[handle=handle]/settings/team/`), which still use the legacy Lemmy-shaped `Person` type through `legacy-client.ts`. They keep DiceBear (D15).

The `alt` text of the avatar stays the user's name or handle. The picture is decorative and gets no alt text of its own.

## 5. Work

1. **Assets (coves).** Convert `scripts/default-avatars/selected/*.png` to 512px WebP at quality 90 with the v1 slugs, add them to the repo, and add the conversion to `scripts/default-avatars/` so the set can be rebuilt. Commit the generation scripts and subject list. The 1254px PNG masters (about 19 MB) stay out of git (D11).
2. **Route (coves).** The embedded file handler and route in §4.3, with tests for 200, HEAD, 404, headers and every v1 slug present.
3. **Web (coves-frontend).** The assignment function with the §4.2 vectors, the `Avatar.svelte` change, and every Coves user call site passing `did`.
4. **Mobile (coves-mobile).** The same in Dart, the `UserAvatar` change, and every call site.

Steps 3 and 4 depend on step 2 being deployed only for the images to load; the tint fallback works before that.

## 6. Acceptance

- The three test vectors pass in Go (if a Go helper exists), TypeScript and Dart.
- `curl -I https://img.coves.social/img/default-avatars/v1/octopus.webp` returns 200 with the headers in §4.3, and a second request shows a Cloudflare cache hit.
- `…/v1/not-a-slug.webp` returns 404 with `no-store`.
- In the local stack, a test account with no avatar shows the same picture and tint on web and mobile, and the picture matches the vector computed from its DID.
- On both clients, in the local stack: a user with a working avatar shows it; a user whose avatar URL fails to load shows their default; when the default picture itself fails to load, the tinted circle shows alone; an empty DID shows the plain Mist circle.
- Community avatars on the web still show DiceBear, and Bluesky embed authors on mobile still show the Bluesky fallback.
- `/v2/octopus.webp` returns 404 with `no-store`, and a matching `If-None-Match` returns 304.
- No request to a public host is needed to render a default in local development.

## 7. Risks

- **Unlucky combinations.** Some picture and tint pairs will clash, such as a blue whale on Sky. Both lists are frozen by D5. The contact sheet review (D10) found every pair readable at 24px; the weakest is `sand-dollar` on Sand and Peach.
- **Frozen list.** A bad picture found after launch can only be replaced by drawing a new file under the same slug in a new version (`v2`), which keeps everyone's assignment, or by a new pool, which reassigns everyone. Replacing a file in place under `v1` is not possible because of the `immutable` cache header.

## Implementation notes

Backend (coves loop, 2026-10-29):

- 01 Default avatar assets and serving route — cb676008
- Package `internal/api/handlers/defaultavatars` serves GET/HEAD `/img/default-avatars/{version}/{file}`. `cmd/server` `registerImageRoutes` wires it, and `registerRoutes` calls that unconditionally. The unit test covers `registerImageRoutes`. A hermetic-stack live check covered the full wiring with the image proxy enabled. The proxy-disabled case was not exercised live.
- The ETag is the quoted hex of the first 16 bytes of the image's SHA-256. The WebP set is 901,928 bytes.
- `docs/MEDIA.md` documents HEAD on both `/img` routes and the default-avatar cache exception.
- Deferred review suggestions (6) were not taken. The most notable is a `slog.Info` line at route registration.
- Still open:
  - The D11 master upload to `pumped_wigner` under `coves-assets/default-avatars/masters/` (Bretton).
  - The §6 production checks after deploy: `curl -I` headers on `img.coves.social`, then a Cloudflare cache hit on the second request.
  - The web and mobile client work is tracked in their own repos.

Web (coves-frontend loop, 2026-10-29):

- 01 Default avatars on the web — coves-frontend `prd/prd_default_avatars` 31d3b748
- `src/lib/app/util/default-avatar.ts` holds the assignment, with a synchronous local SHA-256 (no new dependency) so server rendering already has the tint and picture. Its tests use the §4.2 vectors plus multi-block and padding-boundary digests. `Avatar.svelte` takes an optional `did`. Without an origin, the URL falls back to the root-relative `/img/default-avatars/v1/{slug}.webp`.
- Authorless notifications take the DID from the record URI's authority; one with no DID authority draws the Mist circle.
- Live check, local dev:
  - Passed: the tint with a failing picture, communities on DiceBear, a failing real avatar falling back, and no console errors.
  - Blocked: the picture actually loading, because the dev backend did not yet serve the route.
  - Blocked: a working real avatar on the check's `:8090` pair, because the page CSP `img-src https:` blocks the backend's `http://127.0.0.1:8080` avatar URLs there.
- Still open:
  - Re-run the live check's picture-loading case once the backend route runs locally.
  - Cross-client check: `livecheck.local.coves.dev` (`did:plc:sk27ydgarb6vtpnpkjtuzqic`) should show `sea-turtle` on Sand on both web and mobile (Bretton, a screenshot from each).
  - Mobile is blocked: this host has no Flutter toolchain (see the coves-mobile loop).
