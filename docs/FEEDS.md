# Feeds

## Surfaces

Every post list is a Postgres query in the AppView. Coves serves its own built-in feed generators through `getFeed` (below) and serves no `getFeedSkeleton`. `getTimeline`, `getDiscover` and `getCommunity` are aliases of `getFeed`: each maps its `sort` to a built-in and calls the same code as `getFeed`.

| Route | Auth | Scope | Served by |
| --- | --- | --- | --- |
| `/xrpc/social.coves.feed.getFeed` | `OptionalAuth` (scope `home` requires a viewer) | `home`, one community, or `all` | The requested built-in |
| `/xrpc/social.coves.feed.getTimeline` | `RequireAuth` | `home`: communities the viewer subscribes to | getFeed; `sort` `hot` (default), `new`, `top` |
| `/xrpc/social.coves.feed.getDiscover` | `OptionalAuth` | `all`: every community | getFeed; `sort` `hot` (default) is `discover-hot`, `new`, `top` |
| `/xrpc/social.coves.communityFeed.getCommunity` | `OptionalAuth` | `community`: one community | getFeed; `sort` `hot` (default), `new`, `top` |
| `/xrpc/social.coves.feed.searchPosts` | `OptionalAuth` | Every community, or one | Its own query |

On the `OptionalAuth` routes a signed-in viewer gets their own vote state and block filtering. An anonymous caller gets neither.

The community feed is served as `social.coves.communityFeed.getCommunity`, and no `communityFeed.*` lexicon exists. The frontend and mobile clients call the served name. The `feed/getCommunity.json` and `feed/getAll.json` lexicons were published with no route; both files are deleted and the published schemas are to be unpublished (see [Retired schemas](LEXICON_PUBLISHING.md#retired-schemas)). `social.coves.feed.getFeed` (below) replaces the post list endpoints. The old endpoints stay as aliases so released clients keep working, and are to be removed after the clients switch. Register every new endpoint under its lexicon id.

An alias accepts only `sort` `hot`, `new` or `top` (absent is `hot`); any other value is 400 `InvalidRequest`. `limit`, `timeframe`, `cursor` and `community` pass through, so validation, error codes, viewer state and moderation are getFeed's. Two old rules are kept: getTimeline answers 401 `AuthenticationRequired` without a signed-in viewer, and getCommunity validates `sort`, `limit` and `timeframe` before it resolves the community and applies no length cap to `community` (an unknown long identifier is 404 `CommunityNotFound`). Responses are getFeed's (`feed`, `cursor`, `reqId`).

## getFeed and feed generators

`social.coves.feed.getFeed` takes `scope`, an optional `feed` (a generator at-uri), and for scope `community` a `community` identifier. The built-in generators are `hot` (scopes `home` and `community`), `new` and `top` (scopes `home`, `community` and `all`), and `discover-hot` (scope `all` only). `hot` reproduces the old Timeline Hot and community Hot queries: the same ranking, candidate bound, blocks and visibility. `new` and `top` reproduce the old New and Top queries on Timeline and the community feed: the same order, blocks, visibility, and on `home` the level 1-2 frequency thresholds, with no candidate bound; on `all` they reproduce Discover New and Top, including Discover's blocks and its exclusion of instance-removed communities. `discover-hot` is Discover Hot: the same snapshot, ranking, diversity selection, hydration-race refill and `discover-hot-v1` cursor (carried inside the envelope), which getDiscover with sort `hot` or no sort also serves; its pages come back already hydrated, and its capacity refusals are 503 `DiscoverUnavailable` with `Retry-After: 30`. An omitted `feed` uses the instance default for the scope: `hot` for `home` and `community`, `discover-hot` for `all`. `hot` is not offered at scope `all`, and `discover-hot` is not offered at `home` or `community`; both return 400 `UnsupportedFeedScope`.

`timeframe` is validated for every feed, as the old feeds did: one of `hour`, `day`, `week`, `month`, `year`, `all`, anything else is 400 `InvalidRequest`. It defaults to `day`. `top` keeps only posts created within it (`all` keeps every post); `hot` and `new` ignore it. The cursor does not bind it, as the old Top cursors did not. Scope `thread` returns 400 `UnsupportedFeedScope`. An unregistered `feed` returns 400 `UnknownFeed`; scope `home` without a signed-in viewer returns 401.

Built-in generator at-uris are `at://<SERVER_DID>/social.coves.feed.generator/<rkey>`, built from the configured `SERVER_DID` without a PDS login. At startup the AppView logs into the server's own PDS account (`SERVER_HANDLE` / `SERVER_REPO_PASSWORD`, not the PDS admin) and writes each built-in `social.coves.feed.generator` record into that account's repo: a missing record is created, a changed one is rewritten with its original `createdAt`, an unchanged one is left alone. A missing credential, failed login or failed record write logs an ERROR and boot continues; getFeed serves regardless and the records sync on the next restart. This login replaced the earlier instance credential check, whose token nothing used.

Feed pickers use two public queries that take no auth. `social.coves.feed.describeFeedGenerator` returns `did` (the configured `SERVER_DID`) and `feeds`, the at-uris of the registered built-ins in registration order. `social.coves.feed.getFeedGenerators` takes up to 25 repeated `feeds` at-uris and returns a `generatorView` (`uri`, `did`, `displayName`, `description`, `kind`, `scopes`, `acceptsInteractions`, which is `false` for every built-in) for each registered one, in request order; unknown at-uris are omitted. A missing `feeds`, more than 25, or a value that is not an at-uri returns 400 `InvalidRequest`.

The getFeed cursor is described under [Cursors](#cursors).

### Cost (posts)

Measured 2026-11-19 by `TestFeedQueryCost_OldVersusRankAndHydrate` (`EXPLAIN (ANALYZE, BUFFERS)`, page size 15) on the PostgreSQL test database after `ANALYZE`, before the old queries were removed: 24,000 posts, 12,000 community admissions, 12 communities, 301 users, 608 subscriptions (the viewer at levels 1-5, 3, 3, 3, with one community block and one user block). "Old" is the previous single query, measured through its verbatim copy in the oracle (`internal/db/postgres/feed_oracle_test.go`) while the old code still existed; "rank" is the built-in generator's query and "hydrate" the read by URI of the rank query's first page.

| Surface | Query | Execution | Planning | Shared hit | Shared read | Rows |
| --- | --- | --- | --- | --- | --- | --- |
| Timeline hot | Old | 261.832 ms | 4.229 ms | 128,708 | 0 | 16 |
| | Rank | 192.693 ms | 2.952 ms | 118,569 | 0 | 16 |
| | Hydrate | 0.346 ms | 1.172 ms | 214 | 0 | 15 |
| Timeline new | Old | 11.513 ms | 2.692 ms | 9,336 | 0 | 16 |
| | Rank | 11.216 ms | 1.767 ms | 9,282 | 0 | 16 |
| | Hydrate | 0.317 ms | 1.244 ms | 207 | 0 | 15 |
| Community hot | Old | 35.336 ms | 1.184 ms | 27,626 | 0 | 16 |
| | Rank | 31.050 ms | 0.827 ms | 21,689 | 0 | 16 |
| | Hydrate | 0.374 ms | 0.693 ms | 252 | 0 | 15 |
| Community top (day) | Old | 0.961 ms | 1.032 ms | 901 | 0 | 16 |
| | Rank | 0.686 ms | 0.502 ms | 544 | 0 | 16 |
| | Hydrate | 0.372 ms | 0.650 ms | 259 | 0 | 15 |
| Discover new | Old | 0.211 ms | 1.024 ms | 148 | 0 | 16 |
| | Rank | 0.131 ms | 0.579 ms | 97 | 0 | 16 |
| | Hydrate | 0.287 ms | 0.987 ms | 163 | 0 | 15 |
| Discover top (day) | Old | 0.217 ms | 1.190 ms | 150 | 0 | 16 |
| | Rank | 0.131 ms | 0.651 ms | 99 | 0 | 16 |
| | Hydrate | 0.287 ms | 0.980 ms | 175 | 0 | 15 |

Rank plus hydrate executes faster than the old query on timeline hot (193.0 ms against 261.8 ms) and community hot (31.4 ms against 35.3 ms), and the same on timeline new (11.5 ms against 11.5 ms). On the cheap surfaces the second read adds 0.10-0.21 ms of execution (community top 0.10 ms, Discover new and top 0.21 and 0.20 ms) and, counting planning too, 0.22 ms on community top, 0.75 ms on Discover new and 0.64 ms on Discover top. The hydrate plan reads posts by primary key or a `uri` index (a bitmap AND with another index allowed), at most one row per page entry; the test fails otherwise. Discover Hot already ranked then hydrated and is not compared.

#### Production (2026-11-19)

Measured on production with `EXPLAIN (ANALYZE, BUFFERS)` inside `BEGIN TRANSACTION READ ONLY` (10 s statement timeout, rolled back), the same query text as the test with real parameters, warm cache (second of two runs; shared reads were 0): the viewer with the most subscriptions (15), the community with the most live posts (3,519), the `day` timeframe for top, page size 15. Production then held 51,098 posts, 38,996 admissions, 1,730 communities, 15,077 users and 74 subscriptions. The hydrate input was the URIs the matching rank query returned (10 on community top, where only 10 posts were from the last day). Rank + hydrate is the sum of the two queries; the extra round trip between them is not included.

| Surface | Old execution | Rank + hydrate execution | Old planning | Rank + hydrate planning | Old shared hit | Rank + hydrate shared hit |
| --- | --- | --- | --- | --- | --- | --- |
| Timeline hot | 1,054.637 ms | 985.848 ms (979.582 + 6.266) | 41.499 ms | 32.791 ms | 278,276 | 258,380 |
| Timeline new | 805.409 ms | 606.527 ms (604.308 + 2.219) | 12.674 ms | 12.455 ms | 340,085 | 261,591 |
| Timeline top (day) | 5.534 ms | 6.246 ms (4.048 + 2.198) | 11.887 ms | 12.453 ms | 1,796 | 1,841 |
| Community hot | 467.413 ms | 439.230 ms (436.359 + 2.871) | 4.820 ms | 6.214 ms | 103,652 | 90,012 |
| Community new | 13.796 ms | 15.514 ms (12.742 + 2.772) | 4.005 ms | 6.473 ms | 716 | 1,077 |
| Community top (day) | 0.965 ms | 2.404 ms (0.535 + 1.869) | 4.115 ms | 4.504 ms | 197 | 464 |
| Discover new | 13.712 ms | 15.223 ms (12.893 + 2.330) | 5.016 ms | 9.278 ms | 797 | 1,080 |
| Discover top (day) | 13.659 ms | 15.055 ms (13.002 + 2.053) | 5.072 ms | 9.115 ms | 911 | 1,198 |

On production the expensive surfaces got cheaper (timeline hot 69 ms, timeline new 199 ms, community hot 28 ms less execution) and the cheap ones cost 0.7-1.7 ms more, almost all of it the hydrate read (2-3 ms on production against 0.3 ms on the test database). Every hydrate plan read posts through `posts_uri_key`, alone or ANDed with `idx_posts_community_created`. Timeline hot and new take 0.6-1.1 s on production with either query; that cost is in the rank query and predates this change.

## Visibility

Every post read goes through `visiblePostsPredicate` in `internal/db/postgres/post_visibility.go`, bound by `visiblePostsJoin` in the feed queries and Discover Hot; only the Discover Hot baseline calls the predicate directly. The public sees accepted posts whose accepted CID matches the post's CID, and legacy posts with no admission row. The author also sees their own posts in other admission states. An active moderation removal hides a post from everyone. `post.get`'s moderated-post gate and notifications use `admittedPostsPredicate`, the admission half without removal hiding. A new read path, count or search must use this predicate rather than restate it, or pending and removed posts leak through that path while the feeds hide them. The admission rules are in [POSTS.md](POSTS.md).

Community `visibility` (public, unlisted, private) is not consulted on any read path. Posts in a private community appear in Discover, Timeline, the community feed and search for anyone. This is an open backlog issue.

## Blocks

`viewerBlockFilters` in `internal/db/postgres/viewer_block_filter.go` renders both block kinds. Each caller names its surface:

- `aggregateSurface` (Discover, Timeline, searchPosts without `community`): author blocks and community blocks apply.
- `explicitSurface` (the community feed, searchPosts with `community`): author blocks only.

A community block is an aggregate-feed mute. This is a product decision that matches Reddit and Lemmy: the community's own feed, post permalinks and comment threads stay reachable. Comment queries filter author blocks with their own clauses on `commenter_did` and ignore community blocks. An authenticated `community.get` returns `viewer.blocked` so clients can draw the block toggle.

## Cursors

Every feed cursor is `base64(payload::hex(HMAC-SHA256(payload)))` keyed by `CURSOR_SECRET`. Production config refuses to start when `CURSOR_SECRET` is unset (it falls back to a public dev constant), a `CHANGE_ME` placeholder, or shorter than 16 characters. Rotating it invalidates every outstanding cursor on every feed and reshuffles the Discover hosted bonus. A tampered or malformed cursor returns 400 `InvalidCursor`.

The getFeed cursor is a signed envelope around the generator's own cursor, bound to the feed at-uri, scope, community DID and viewer DID; replaying it with any of those changed, or tampering with it, returns 400 `InvalidCursor`. Alias cursors are getFeed cursors: a getTimeline, getDiscover or getCommunity cursor pages the matching getFeed feed and scope, and a request for a different feed, scope, community or viewer, through any alias or getFeed, rejects it. Cursors issued before the old endpoints became aliases are not envelopes; each gets a one-time 400 `InvalidCursor` at deploy, with no compatibility shim, and clients restart from page one. Known limitation: the envelope stays within the lexicon's 500-character cursor bound only for post URIs up to about 200 characters. A longer post URI still pages, because the handler never rejects a server-signed cursor for its length, but that cursor exceeds the published bound.

Inside the envelope, the `hot` generator's cursor carries `created_at::uri::query_time`, not the rank. The next page recomputes the anchor post's rank from its current row at the pinned query time with the same SQL expression as the candidates, so float rounding is identical on both sides of the comparison and posts do not drift across pages as time passes.

## Hot rank

```text
(SIGN(score) * LN(ABS(score) + 1) + 1) / (GREATEST(age_hours, 0) + 2)^0.8
```

The age-decay exponent (`hotRankGravity` in SQL, `discoverHotGravity` in Discover) changed from 1.5 to 0.8 on 2026-11-06, with Discover algorithm version 3, to make the top of the feed less recency-heavy. In a production simulation the top 25 posts' median age went from about 1.6 hours to about 3 hours, and their median score from 10 to about 27.

The numerator is log-damped because bridged and federated communities import vote counts in the hundreds: going from 0 to 10 votes counts as much as going from 10 to about 120, so a bridged post with hundreds of votes falls below a fresh native post within about two days (a 782-vote post ranks about 0.57 at 24 hours and 0.34 at 48 hours, against 0.56 for a 0-vote post a few minutes old). A zero-vote post keeps a numerator of 1. `SIGN`/`ABS` keep the expression defined for negative scores, since `LN` of a non-positive value errors in Postgres. `GREATEST(age, 0)` ranks a future-dated post as brand new instead of boosting it, and ingestion also clamps future `createdAt`.

`hotRankSQL` in `internal/db/postgres/feed_repo_base.go` is the only builder of this expression. The live `ORDER BY` and the cursor comparison both call it so they cannot drift. Timeline Hot and community Hot use it. Discover Hot ranks in Go and must equal this formula at neutral settings. Do not change the shared formula to tune Discover.

Timeline Hot and community Hot rank only the newest 5,000 visible posts in scope, and the feed ends after them. New, Top with timeframe `all`, and search reach older posts. Discover Hot has the same bound on its candidates. Posts the viewer cannot see or has blocked do not count toward the 5,000. Timeline and community Hot recompute the bound on every page, ignoring the cursor, so posts added or removed during a scroll move it and the feed can end earlier or later. A page can come back empty after a cursor. Discover Hot fixes its candidates in the snapshot.

## Sorts and parameters

Feeds sort `hot` (default), `top` or `new`. `top` takes a timeframe of hour, day, week, month, year or all, and the feeds default it to day. `limit` is 1 to 50 with default 15. A limit above 50 is a 400, not a silent cap; zero, negative, non-numeric or absent becomes 15. `ORDER BY` clauses come only from the `feedSortClauses` whitelist, and ties break on `created_at DESC, uri DESC`.

The `community` parameter is a plain string, not `at-identifier`. It accepts a DID, a handle with or without the `c-` prefix, `name@origin`, `!name@origin`, or a bare local name, all resolved by `ResolveCommunityIdentifier` (see [COMMUNITIES.md](COMMUNITIES.md)). Dropping `at-identifier` was an accepted lexicon breaking change.

## Search

`social.coves.feed.searchPosts` lives under `feed.*` because that is the read-many namespace that returns `{feed, cursor}`, and it mirrors `app.bsky.feed.searchPosts`. `community.post.*` is single-post CRUD. The old `community.post.search` lexicon was deleted and unpublished in 2026 (see [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md)). Results use the feed shape so clients reuse their feed parsers.

The index is `posts.search_vector`, a stored generated `tsvector` (title weight A, content weight B) with a partial GIN index, added in migration 044. A stored column was chosen over an expression index so the planner always uses the GIN index and ranking never re-tokenises bodies. Title and content are cut to 3,000 and 100,000 characters to stay under the 1 MiB `tsvector` limit, because federated records from the firehose never passed the local API's length caps. The text search config is `english` because posts store no `langs` column (only comments do), so per-language configs are not possible.

Queries go through `websearch_to_tsquery`, which never errors on user input and supports quoted phrases, `-negation` and `OR`. Relevance is `ts_rank_cd(..., 1)`, which divides by log length so long bodies do not dominate. A stopword-only query parses to an empty tsquery and a negation-only query (`-rust`, or `fox OR -red`) would match the whole corpus; SQL refuses both with `querytree(...) NOT IN ('', 'T')` and returns an empty feed.

Sorts are `relevance` (default), `new` and `top`; `hot` is rejected. `timeframe` defaults to all and applies only to `top`. `q` is trimmed and must be 1 to 500 bytes. A supplied `limit` that is non-numeric or below 1 is a 400, which is stricter than the feeds.

A search cursor is a signed `search::<scope hash>::<position>` envelope. The scope hash covers `q`, the resolved community DID, the sort, and the timeframe when the sort is `top`. Replaying a cursor with any of those changed returns `InvalidCursor`. A relevance position carries the `float4` rank, which round-trips exactly through text because lib/pq sets `extra_float_digits`, so the filter compares with `= $n::real`.

## Rate limits

A global limiter allows 100 requests per minute per client IP on every route. Timeline, Discover and the community feed have no route limiter of their own. searchPosts adds a named `postSearch` limiter of 30 per minute per IP, registered in `internal/api/routes/communityFeed.go`. Discover Hot also has its own build budget, below.

## Discover Hot

Only the `discover-hot` built-in (getDiscover with sort `hot`, including the default sort) uses this ranking. Discover New and Top are the `new` and `top` built-ins at scope `all`. It exists because high-vote bridged communities took most of Discover: in a September 2026 sample two communities held 60% of the first 50 posts.

### Ranking

The ranking time `T` is captured with the snapshot. The baseline cohort is posts created in `[T - 14 days, T - 24 hours)` that are publicly visible and not deleted. Viewer blocks do not change it and zero-score posts count. Each post contributes `ln(1 + max(score, 0))`.

`G` is the median of per-community means over communities with at least five cohort posts. Each community counts once, so a prolific community cannot pull the reference toward itself. A community's smoothed baseline is `B_c = (sum_c + 20G) / (n_c + 20)`, and its adjustment is `clamp(sqrt(max(G, 1) / max(B_c, 1)), 0.5, 1)`. The adjustment applies only to positive scores: the numerator is `1 + adjustment * ln(1 + score)`. Zero gives 1 and a negative score gives `1 - ln(1 + |score|)`, as in the shared formula. No community gets a multiplier above 1. With no qualifying community, or for a community with no or all-zero history, the adjustment is 1.

The hosted bonus (algorithm version 2, approved 2026-09-18; the current version is 3, which changed the age-decay exponent to 0.8) adds `3.0 * u` to the numerator of posts with score at least 0 in hosted communities. `u` is the first 8 bytes of `HMAC-SHA256(CURSOR_SECRET, "discover-hot-hosted-bonus\0" + URI)`, big-endian, shifted right 11 and divided by 2^53. Hosted posts sit at 0 or 1 votes while bridged posts import hundreds, so fresh hosted posts ranked below much older bridged ones. A flat boost was rejected because it would stack every fresh hosted post at the top. The bonus is keyed because authors choose their rkeys, and an unkeyed digest could be ground offline in about 1,000 tries for a near-maximum bonus. The label prefix separates it from cursor signatures, which use the same secret. It is deterministic, so snapshot rebuilds do not reshuffle the feed. "Hosted" means `pds_refresh_token_encrypted IS NOT NULL` (see [COMMUNITIES.md](COMMUNITIES.md)). The 3.0 maximum came from a production simulation where hosted posts reached about a fifth of the first 50 without lengthening the longest hosted run. That was measured under version 2 (exponent 1.5); a 2026-11-06 re-ranking of a production snapshot at 0.8 kept hosted posts at 7 to 8 of the first 50, about the same as at 1.5.

Selection is sequential with a repetition penalty. The window is the last 20 emitted posts, not the HTTP page. A candidate's penalty is `1 + 0.5 * count` of its community in the window; a rank at or above 0 is divided by it and a negative rank multiplied, so repetition never helps a downvoted post. Ties break on `created_at DESC, uri DESC`. This is a soft penalty, not a quota, and a single community's supply still fills pages.

The rank is computed in Go (`internal/core/discover/hot_ranking.go`), stored as `double precision`, and reused verbatim on continuation. A nonfinite rank is an error. `discover_hot_rank_sql_equivalence_test.go` pins the Go rank to `hotRankSQL` within 1e-12 at neutral normalization with no bonus.

### Snapshots and checkpoints

A keyset cursor would filter on the last post's rank, but diversity selection defers higher-ranked posts, and advancing that boundary would lose them. Discover Hot instead freezes candidate metadata and ranks in a Postgres snapshot (migration 048). Each page stores an immutable checkpoint of stream positions and the emitted-community window. The cursor (`discover-hot-v1`, signed, at most 500 characters, bound to the viewer) names a snapshot and checkpoint. Snapshots and continuations are keyed by `discoverHotAlgorithmVersion`; bump it for any change to base-rank inputs or checkpoint semantics.

Checkpoints were chosen over a precomputed diversified order because a post hidden before delivery must not influence the diversity of what follows. Every page re-checks current visibility, blocks and deletions, skips excluded candidates and refills. If a selected post fails hydration because it became ineligible, it is marked excluded and selection reruns from the input checkpoint. Identical checkpoints are deduplicated. A GET never advances shared state, so replays and concurrent retries return the same page.

A request with no cursor reuses its scope's snapshot when it is under 30 seconds old. The scope is the anonymous public (shared) or one viewer DID, so author-only candidates never cross viewers. Reuse does not extend the snapshot's one-hour absolute lifetime.

### Budgets

| Budget | Limit |
| --- | --- |
| Snapshot reuse per scope | 30 seconds |
| Snapshot lifetime | 1 hour, absolute |
| Candidates per snapshot | Newest 5,000 visible posts; beyond that, the feed ends normally |
| Stored candidates, all snapshots | 10,000,000 |
| Stored checkpoints | 20,000, each at most 64 KiB (database constraint) |
| Snapshot builds | 20 per minute globally, failed and cancelled builds included; one at a time under an advisory lock |
| Work deadline | 5 seconds, or the caller's earlier deadline |
| Cleanup | Every minute, 10,000 rows per batch |

The caps are enforced atomically in the database so several AppView processes share them.

### Errors

An expired, missing, malformed, tampered, wrong-viewer, wrong-sort or old-version cursor returns 400 `InvalidCursor`, and clients restart from page one. Bumping `discoverHotAlgorithmVersion` invalidates every outstanding cursor, and the old version's candidate rows keep counting against the stored-candidate cap for up to an hour.

A full stored-candidate or stored-checkpoint cap, a checkpoint over 64 KiB, an exhausted build budget or a missed deadline returns 503 `DiscoverUnavailable` with `Retry-After: 30`. The candidate bound is not a capacity refusal: posts past it are left out and the feed ends normally. A 503 never comes with a partial or empty page and never falls back to the legacy ranking. A continuation that fails this way does not advance its checkpoint. When the caller's own deadline expires, the error matches both `context.DeadlineExceeded` and `ErrDiscoverUnavailable`; a plain cancellation stays a cancellation. A pooled connection that held an advisory lock is discarded after an acquisition error, a cancellation or a failed unlock, so a lock can never leak back into the pool.

The frontend and mobile 1.4.0+11 restart from page one on `InvalidCursor` and honour `Retry-After`. Older mobile installs need a pull-to-refresh after a cursor expires.

### Known limitations

- A community with no mature baseline keeps its full raw engagement during its first day. Immature posts are deliberately not used as a provisional baseline.
- The baseline uses the current scores of mature posts, not their scores at 24 hours, because historical aggregate votes are not stored.
- Every post in a hosted community gets the bonus, aggregator posts included. Revisit this as native posting grows.
- The first downvote on a zero-score hosted post removes the whole bonus, dropping the numerator from up to 4.0 to `1 - ln 2` (about 13x), against about 3.3x for a non-hosted post. One account can do this. A taper over the first few negative scores is the fallback if it is abused.

## Comments

`social.coves.community.comment.getComments` takes an optional `feed`, the at-uri of a comment generator. The built-ins are `comments-hot`, `comments-top` and `comments-new` (kind `comments`, scope `thread`). `sort` (`hot`, `top`, `new`) is a deprecated alias that maps to the matching built-in; `feed` wins when both are sent, and with neither the order is `comments-hot`. A `feed` that is not an at-uri is 400 `InvalidRequest`, an unregistered one 400 `UnknownFeed`, and a posts generator 400 `UnsupportedFeedScope`. `timeframe` is accepted only by `comments-top`; with any other feed it is 400 `InvalidRequest`. getFeed rejects the comment built-ins with `UnsupportedFeedScope`.

A comment generator orders the direct replies of one or more parents and returns URIs only; the AppView then reads those comments by URI and keeps the generator's order. The top level and a `parentRkey` subtree are one parent with a cursor and the request's limit and timeframe. Each nested level is one call with every parent at that depth, 5 replies per parent and no timeframe. The orders, filters and cursors are the ones the previous single queries used: hot ranked with the cursor's pinned `rankedAt` and live scores, author blocks applied when a viewer is signed in, deleted and removed comments kept as placeholders. The previous queries live on as test oracles in `internal/db/postgres/comment_oracle_test.go`, and the equivalence tests compare whole getComments trees and hot cursor drift against them.

### Cost

Measured 2026-11-19 by `TestCommentQueryCost_OldVersusRankAndHydrate` (`EXPLAIN (ANALYZE, BUFFERS)`) on the PostgreSQL test database after `ANALYZE`: 21,100 comments, a busy thread with 600 top-level comments and 500 replies plus 200 other threads, 401 users. "Old" is the previous single query (now the oracle); "rank" and "hydrate" are the generator query and the read by URI.

| Query | Execution | Planning | Shared hit | Shared read | Rows |
| --- | --- | --- | --- | --- | --- |
| Old, top-level hot page (limit 50) | 0.742 ms | 0.554 ms | 57 | 0 | 51 |
| Rank, top-level hot page | 0.369 ms | 0.077 ms | 46 | 0 | 51 |
| Hydrate, top-level hot page | 0.383 ms | 0.256 ms | 163 | 0 | 50 |
| Old, nested batch level (25 parents x 5) | 1.661 ms | 0.293 ms | 453 | 0 | 125 |
| Rank, nested batch level | 0.553 ms | 0.097 ms | 277 | 0 | 125 |
| Hydrate, nested batch level | 0.835 ms | 0.263 ms | 411 | 0 | 125 |

Rank plus hydrate costs about the same as the old query at the top level (0.75 ms against 0.74 ms execution, more buffer hits from the second read) and less at a nested level (1.39 ms against 1.66 ms). The hydrate plan reads comments by primary key or the unique `uri` index, at most one row per page entry; the test fails otherwise.

#### Production (2026-11-19)

Measured on production the same way as the posts (read-only transaction, warm cache, shared reads 0): the thread with the most comments (676 comments, 70 top-level), the viewer with the most subscriptions, hot. The nested batch is that thread's 25 top-level comments with the most replies, 5 per parent; they had 79 replies between them, so the level returned 79 rows rather than 125. Production then held 108,693 comments.

| Query | Old execution | Rank + hydrate execution | Old planning | Rank + hydrate planning | Old shared hit | Rank + hydrate shared hit | Rows |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Top-level hot page (limit 50) | 1.610 ms | 2.967 ms (0.256 + 2.711) | 1.746 ms | 1.821 ms | 528 | 647 | 50 |
| Nested batch level (25 parents x 5) | 2.330 ms | 4.519 ms (0.835 + 3.684) | 0.857 ms | 1.015 ms | 611 | 1,048 | 79 |

On production rank plus hydrate costs 1.4 ms more at the top level and 2.2 ms more at a nested level, the hydrate read by `comments_uri_key` being most of it.
