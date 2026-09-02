# PRD: Post Search (community Search tab)

Status: backend implemented 2026-09-01 via /tdd and hardened after /second-opinion on 2026-09-02 (§1 describes what shipped; §2 and §3 are still the plan)
(10 red/green cycles, plan-reviewed). Clients cannot ship until the endpoint is
deployed.

Goal: cross-community post search, `social.coves.feed.searchPosts`. One entity
type (posts), searched across every community this AppView indexes, with an
optional `community` filter. The first consumer is a Search tab on the
community page (mobile first, frontend second), which always passes
`community`. A client-side filter over loaded feed pages would only see the
15-50 posts in memory, so that is explicitly not the design.

Scope words, so nobody re-litigates them:
- Cross-community post search: this PRD. Posts only, all communities, optional
  community filter.
- Global search: a later product surface (Home search box) that fans out to
  `feed.searchPosts`, `community.search`, and a future `actor.searchActors`
  and renders them as tabs. Not this PRD.
- Network-wide: not a thing in atProto. An AppView searches only what it has
  indexed. That is also Bluesky's limit.

## 0. Decisions (read these first)

| Decision | Choice | Why |
|---|---|---|
| NSID | `social.coves.feed.searchPosts` | The namespace says what comes back, not what filters it: `feed.getCommunity` is already a community-scoped read of posts under `feed.*`. `feed.*` is the read-many namespace and is already published/delegated in DNS (`docs/LEXICON_PUBLISHING.md`). Mirrors `app.bsky.feed.searchPosts`. `community.searchPosts` was considered and rejected: it would sit beside `community.search` (which returns communities) and would read wrong once `community` is optional. |
| Old lexicon | Delete `internal/atproto/lexicon/social/coves/community/post/search.json` | Never implemented and would duplicate the new query. It WAS published by the 2026-07 `community.*` sweep and still resolves on the network; decision 2026-09-01: keep the deletion and retire it with `goat lex unpublish social.coves.community.post.search` at the next publish (step added to `docs/LEXICON_PUBLISHING.md`). The `community.post.*` namespace itself is alive (create/update/delete/get/getStatus are served); it is the single-post CRUD surface, while `feed.*` is the read-many surface, which is where a search belongs. Only the `community.post` *record* ingestion was retired in `ff53627`. `docs/PRD_POSTS.md:464` marks search done; fix that line. |
| Output key | `feed`, not `posts` | Every `feed.*` query returns `{feed: [feedViewPost], cursor}`. Mobile `TimelineResponse` and frontend `FeedResponse` then parse search results with zero new code. |
| `community` param | Optional | Omitted: search every community, with exactly the predicate `getDiscover` uses (admission gate + viewer author-blocks). Present: add `p.community_did = $n`. Discover applies no community-level rules today, so the cross-community path costs one conditional clause. The community Search tab always passes it. |
| Sorts | `relevance` (default), `new`, `top` + `timeframe` | `new`/`top` reuse the existing signed-cursor branches unchanged. `hot` is meaningless for search. |
| Limit | 1..50, default 15 | Same as every other feed query, so the service validation is shared. |
| Index | STORED generated `tsvector` column + GIN | See §2. |
| Text search config | `english` | Matches the commented-out index in migration 011 and the PRD. Posts do not store `langs` (only comments do), so per-language configs are not possible yet. Stemming non-English (Lemmy-bridged) text as English is mostly harmless. |
| Query parser | `websearch_to_tsquery` | Never errors on arbitrary user input; supports `"quoted phrases"`, `-negation`, `OR`. |
| Route path | `/xrpc/social.coves.feed.searchPosts` exactly | Do not repeat the getCommunity drift (§1.6). |

## 1. Backend (`coves`) — implemented

### 1.1 Lexicon

New file `internal/atproto/lexicon/social/coves/feed/searchPosts.json`:

```json
{
  "lexicon": 1,
  "id": "social.coves.feed.searchPosts",
  "defs": {
    "main": {
      "type": "query",
      "description": "Full-text search over visible posts, across every community or restricted to one. Results honour the same admission, deletion and viewer-block rules as social.coves.feed.getDiscover and social.coves.feed.getCommunity.",
      "parameters": {
        "type": "params",
        "required": ["q"],
        "properties": {
          "q": { "type": "string", "maxLength": 500, "description": "Search query. Supports quoted phrases and -negation." },
          "community": { "type": "string", "maxLength": 320, "description": "Restrict results to one community: DID, handle, name@origin, or bare local name. Omit to search every community." },
          "sort": { "type": "string", "knownValues": ["relevance", "new", "top"], "default": "relevance", "maxLength": 64 },
          "timeframe": { "type": "string", "knownValues": ["hour", "day", "week", "month", "year", "all"], "default": "all", "maxLength": 64 },
          "limit": { "type": "integer", "minimum": 1, "maximum": 50, "default": 15 },
          "cursor": { "type": "string", "maxLength": 500 }
        }
      },
      "output": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": ["feed"],
          "properties": {
            "feed": { "type": "array", "items": { "type": "ref", "ref": "social.coves.feed.defs#feedViewPost" } },
            "cursor": { "type": "string", "maxLength": 500 }
          }
        }
      }
    }
  }
}
```

`tests/lexicon_fixtures_test.go` loads the whole lexicon directory into an
indigo catalog, so the new file is validated at T0 automatically. Query
lexicons need no fixture. No `//coves:ingestion-contract` marker either; that
gate only covers `jetstream.WantedCollections`.

### 1.2 Migration `044_posts_search_vector.sql`

```sql
-- +goose Up
ALTER TABLE posts ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(content, '')), 'B')
    ) STORED;

CREATE INDEX idx_posts_search_vector ON posts USING gin(search_vector)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_posts_search_vector;
ALTER TABLE posts DROP COLUMN IF EXISTS search_vector;
```

Why a stored column rather than the expression index from migration 011:

- The planner only uses an expression index when the query repeats the
  expression byte-for-byte; a column removes that trap.
- `ts_rank_cd` on a stored vector is cheap. Re-tokenising a 100k-char body per
  candidate row at rank time is not.
- The generated column backfills itself during the migration, so no backfill
  job and no consumer change. The postv2 consumer keeps writing `title` and
  `content` as today.

Cost: adding a STORED generated column rewrites the table under an ACCESS
EXCLUSIVE lock. Fine at current volume; note it in the deploy.

Limits: `tsvector` caps at 1 MiB per row. The local write API caps `content`
at 100,000 bytes and `title` at 3,000, but the firehose indexes federated
records that never passed that API, so the generated expression is bounded
with `left(coalesce(content, ''), 100000)` / `left(coalesce(title, ''), 3000)`
and the post consumer rejects oversized records as permanent events
(`MaxPostContentBytes` / `MaxPostTitleBytes` in `internal/atproto/jetstream`).
`ADD COLUMN ... GENERATED ... STORED` rewrites `posts` under an ACCESS
EXCLUSIVE lock; expect a brief pause on deploy.

### 1.3 Repository (`internal/db/postgres/feed_repo.go`)

Add `SearchPosts(ctx, req) ([]*FeedViewPost, *string, error)` next to
`GetCommunityFeed`, built from the same pieces:

- `feedPostSelectClause(...)` + `scanFeedPost` for hydration (no drift from
  `PostView`).
- `visiblePostsJoin(viewerParam)` from `post_visibility.go`. This is the only
  place the admission predicate is spelled; search MUST go through it, or a
  pending/removed post becomes findable by search while hidden from the feed.
- The same `user_blocks` NOT EXISTS filter and `p.deleted_at IS NULL`.
- Extra predicate: `AND p.search_vector @@ websearch_to_tsquery('english', $q)`.
- Community filter only when the request carries one: `AND p.community_did = $n`.
  Without it the query is `GetDiscover` plus the tsquery. Bind parameters are
  positional, so build the arg list and the numbering together (the feed repo
  hard-codes `$1`/`$2`; do not copy that).
- Extra bind: the tsquery. Bind `q` once and reference `websearch_to_tsquery('english', $n)`
  from WHERE, ORDER BY, and the cursor filter.

Ordering and cursors:

- `relevance`: `ORDER BY rank DESC, p.created_at DESC, p.uri DESC` where
  `rank = ts_rank_cd(p.search_vector, <tsquery>, 1)` (normalisation 1 divides
  by log(length) so long bodies do not dominate). Rank is deterministic for a
  fixed `q`, so keyset pagination is stable. The `relevance` ORDER BY lives in
  `feedSortClauses` beside `hot`/`new`/`top`.
- Every search cursor is a signed envelope `search::<scope hash>::<position>`
  (`parseSearchCursor` / `buildSearchCursor` in `feed_repo_base.go`). The scope
  hash is SHA-256 over the length-prefixed `q`, resolved community DID (or
  empty), sort, and timeframe (timeframe only when `sort=top`, since the other
  sorts ignore it). A cursor replayed with any of those changed is rejected
  with `ErrInvalidCursor`; `new`/`top` positions reuse the feed formats and
  `relevance` carries `rank::created_at::uri`. The rank is a float4 that
  round-trips exactly through the cursor text (lib/pq forces
  `extra_float_digits=2`), so the filter compares `= $n::real` directly.
  Cursors stay HMAC-signed with `cfg.CursorSecret`.
- Degenerate queries are refused in SQL, not the service: stopword-only
  (`"the"`) parses to an empty tsquery and negation-only (`-rust`, or
  `fox OR -red`, which `querytree` also collapses to `T`) would match the whole
  corpus. `querytree(websearch_to_tsquery('english', $q)) NOT IN ('', 'T')`
  returns an empty feed with no cursor for both; never a 500.
- Viewer blocks go through `viewerBlockFilters`: `aggregateSurface` when
  `community` is omitted (author blocks and community blocks apply, like
  Discover), `explicitSurface` when scoped (author blocks only, like the
  community's own feed).

Fetch `limit + 1` rows to decide whether to emit a cursor, as the feed does.

### 1.4 Service (`internal/core/communityFeeds`)

- `types.go`: `SearchPostsRequest{Query, Community, ViewerDID, Sort, Timeframe, Limit, Cursor}`.
- `interfaces.go`: add `SearchPosts` to both `Service` and `Repository`.
- `service.go`: validate (`q` trimmed non-empty, byte length ≤ 500; sort in
  {relevance,new,top}; limit 1..50; timeframe as today), resolve the community
  via `communityService.ResolveCommunityIdentifier` (handles DID, handle,
  `name@origin`, bare name; `ErrCommunityNotFound` on miss) only when the
  request carries one, then call the repo.
  Reuse `validateRequest` pieces rather than copying them.

### 1.5 Handler, route, rate limit

- `internal/api/handlers/communityFeed/search_posts.go`: parse params, call the
  service, then the same post-processing as `HandleGetCommunity`
  (`PopulateViewerVoteState`, `TransformBlobRefsToURLs`, `TransformPostEmbeds`).
  Extract that trio into a shared helper in the package rather than copying it.
- `internal/api/routes/communityFeed.go`: register
  `GET /xrpc/social.coves.feed.searchPosts` with `OptionalAuth`.
- Rate limit: search is the most expensive read on the box. The route carries
  a named `postSearch` limiter (30/min per client IP, created and mounted in
  `internal/api/routes/communityFeed.go`) on top of the global 100/min cap.
- Handler validation: `q` required; a supplied `limit` that is non-numeric or
  below 1 is a 400 (absent `limit` defaults to 15 in the service); the
  lexicon's `minimum: 1` is the contract, so search is stricter than
  `getCommunity`, which silently defaults `limit=0`.
- Caddy: `/xrpc/*` is already in the `@appview` allowlist. No Caddyfile change,
  and `caddy_allowlist_test.go` skips `/xrpc/` routes.

### 1.6 Pre-existing drift to flag, not fix here

Go serves `/xrpc/social.coves.communityFeed.getCommunity` and both clients
call that name, but the only lexicon is `social.coves.feed.getCommunity`
(`internal/api/routes/communityFeed.go:26`, `coves-mobile/lib/services/coves_api_service.dart:243`,
`coves-frontend/src/lib/api/coves/client.ts:47`). There is no
`communityFeed.*` lexicon. Register the new endpoint under its lexicon NSID
and file the getCommunity rename as its own issue (alias route, switch
clients, drop the old path a release later).

### 1.7 Tests

T0 (no tag):
- Handler param parsing and error mapping (missing `q`, bad limit, bad cursor).
- Service validation with a fake repo (trim, empty, oversize `q`; sort/limit
  defaults; community resolution failures map to `CommunityNotFound`).
- Lexicon loads via the existing catalog test.

T1 (`//go:build integration`, in `internal/db/postgres`):
- Relevance: title match outranks body match; phrase and negation work;
  stemming (`running` finds `run`).
- Scoping: with `community` set, a matching post in another community is not
  returned; with it omitted, matches from several communities come back and
  the ranking is unaffected by which community a post is in.
- Visibility: reuse the fixtures in `post_visibility_test.go`. Pending,
  removed, rejected, pending_reacceptance, deleted, and wrong-CID posts are not
  returned to the public; the author sees their own pending post; a blocked
  author's post is filtered for the blocker.
- Cursor round-trip for all three sorts with no duplicates or gaps across
  pages; tampered cursor rejected.
- Stopword-only query returns an empty feed and no cursor.

T2 (`//go:build e2e`, `tests/e2e`):
- One contract test: author writes a postv2 via the PDS, the community accepts
  it, `feed.searchPosts` finds it by a title word; before acceptance the same
  query returns nothing. Follow `read_visibility_contract_test.go`. No
  `time.Sleep` (test-audit fails on it); poll with the suite's helpers.

### 1.8 Docs

- `docs/COMMUNITY_FEEDS.md`: add a `searchPosts` section beside `getCommunity`.
- `docs/PRD_POSTS.md`: the `community.post.search` line now points at
  `feed.searchPosts`; q, community filter, sorts, timeframe and the
  implementation items are ticked, author/type/tag filters stay open.
- `docs/LEXICON_PUBLISHING.md` (deploy-time, not done in the tree): the new
  NSID falls under the already-delegated `_lexicon.feed.coves.social`;
  publish it and unpublish `social.coves.community.post.search` in the same
  sweep (see §4).

## 2. Mobile (`coves-mobile`, Flutter) — ~half a day

Files: `lib/screens/community/community_feed_screen.dart`,
`lib/services/coves_api_service.dart`, `lib/models/post.dart`.

- API: add `searchCommunityPosts({community, q, sort='relevance', timeframe, limit=15, cursor})`
  to `CovesApiService`. Because the response is `{feed, cursor}` it can go
  through the existing `_getFeed` helper (add an optional `q` param) and parse
  with `TimelineResponse.fromJson` unchanged. No codegen in `lib/`
  (hand-written `fromJson`), but `CovesApiService` is mocked, so rerun
  `dart run build_runner build --delete-conflicting-outputs` to regenerate
  `test/test_helpers/test_mocks.mocks.dart`.
- Tabs: `_CommunityTabBar` (L917-953) is a plain `Row` of two `_TabItem`s
  driven by `_selectedTabIndex`; add a third (`Icons.search`, index 2). The
  body switch at L523-528 (`_buildPostsList()` vs `_buildAboutSection()`) becomes
  three-way.
- Search header: the Feed tab's sort chips live in a pinned
  `SliverPersistentHeader` (`_FeedSortDelegate`, 56px). Add a sibling delegate
  holding the `TextField` (style and clear button copied from
  `communities_discovery_screen.dart` L489-540; 300ms `Timer` debounce as in
  L232-233 there). There is no shared debounced search widget; consider
  extracting one now since this is the third copy.
- Pagination: a second `CursorPaginationController<FeedViewPost>` whose
  `fetchPage` calls the new API method with the current query. `refresh()` has
  a generation counter, so each debounced query change just calls `refresh()`
  and stale responses are dropped. The screen has one `ScrollController` and
  `_paginationListener.onLoadMore` is hardwired to `_feedController.loadMore`;
  dispatch on the active tab instead.
- Results list: `PaginatedSliverList<FeedViewPost>` exactly as
  `_buildPostsList()` (L668-724) with `footerKey: ValueKey('community_search_footer')`
  (the widget asserts the `_footer` suffix) and an empty state cloned from
  `_buildEmptyPostsState()` ("No results for …"). Empty query shows a prompt,
  not a request.
- Identifier: pass `widget.identifier` through as `getCommunityFeed` does; the
  backend resolves DID or handle. Skip viewport-fill auto-paging while a query
  is active (precedent: `communities_see_all_screen.dart` L120-124).
- Tests: one `DioAdapter` case in `test/services/coves_api_service_test.dart`
  (it matches the full query map, so list every param), and a first widget
  test for `community_feed_screen.dart` (none exists today; needs
  `AuthProvider`, `CommunitySubscriptionProvider`, `VoteProvider`,
  `CovesApiService` via Provider, see `communities_see_all_screen_test.dart`).
  The theme guard tests reject raw colour literals; use `AppColors`.

## 3. Frontend (`coves-frontend`, SvelteKit) — ~half a day

Files: `src/lib/api/coves/client.ts`, `src/lib/api/coves/types.ts`,
`src/routes/c/[handle=handle]/+page.ts` and `+page.svelte`,
`src/lib/feature/feeds/feed.svelte.ts`.

- Client: add `searchPosts: 'social.coves.feed.searchPosts'` to the `NSID`
  map and a one-line `searchPosts(params)` method. Types: `SearchPostsParams`
  next to `SearchCommunitiesParams` (types.ts L578); response is the existing
  `FeedResponse`, so `PostListShell`, `PostFeed`, `VirtualFeed`, and `loadFeed`
  need no mapping. Types are hand-written, no codegen.
- Route: keep search on the community route as `?q=`. In `+page.ts`, when
  `q` is present call `searchPosts` instead of `getCommunityFeed` and default
  sort to `relevance`; thread `q` into both the initial load and `loadFeed`.
  The feed cache is keyed by route id and invalidates on `recursiveEqual(params)`,
  so `q` must be part of the `'/c/[handle=handle]'` params in `FeedTypes`
  or a new query reuses the old results.
- UI: Photon's `/search` route and the per-community search box were removed
  in `2fd3fa52`; nothing to revive. Put a `SearchBar` (`src/lib/ui/layout/SearchBar.svelte`,
  input name `q`) in the page's `extended` snippet under `CommunityHeader`, as
  a GET form so it works without JS, following the URL-as-source-of-truth
  pattern in `src/routes/explore/+layout.svelte` L20-57. i18n keys
  `routes.search.*` already exist in every locale.
- Sort menu: `CovesSortType` is `hot|new|top`. While `q` is set, show
  `relevance|new|top` (or hide the menu and pin relevance). Decide in the PR;
  either is small.
- Tests (vitest, no Playwright): a `searchPosts` case in
  `src/lib/api/coves/client.test.ts` beside `getCommunityFeed`, and a loader
  case in `src/routes/c/[handle=handle]/page.test.ts` asserting that `q`
  switches the call and that a changed `q` busts the feed cache.

## 4. Order of work

1. Backend PR: lexicon, migration, repo, service, handler, route, limiter,
   T0/T1/T2 tests, docs. Gate with `make ci`. Done (branch `tdd/post-search`).
2. Deploy backend (`/deploy`). Migration rewrites `posts`; expect a brief lock.
   Then the lexicon sweep per `docs/LEXICON_PUBLISHING.md`: publish
   `social.coves.feed.searchPosts`, unpublish
   `social.coves.community.post.search`.
3. Mobile and frontend PRs in parallel against the deployed endpoint.
4. Follow-ups, separately: getCommunity NSID drift (§1.6); author/tag/type
   filters from `docs/PRD_POSTS.md`; a Home search surface that fans out to
   this plus `community.search`.

## 5. Read-path gaps found while scoping (not caused by search, inherited by it)

- `communities.visibility` (`public|unlisted|private`) is never consulted on
  any read path. Discover, the community feed, the timeline and therefore
  search all serve posts from private communities to anyone. Still open;
  tracked in the central issues backlog
  (`2026-09-01-community-visibility-never-consulted-on-read-paths`).
- `community_blocks`: resolved on main (70c36f5, 2026-09-01) as an
  aggregate-feed mute via `viewerBlockFilters` in
  `internal/db/postgres/viewer_block_filter.go`. Discover, the timeline and
  cross-community search hide blocked communities; a request scoped to one
  community still shows it. Search adopted the helper on 2026-09-02
  (`TestSearchPosts_CrossCommunitySearchHonoursCommunityBlocks`).
