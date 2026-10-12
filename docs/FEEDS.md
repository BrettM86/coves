# Feeds

## Surfaces

Every post list is a Postgres query in the AppView. Coves does not run atProto feed generators and serves no `getFeedSkeleton`.

| Route | Auth | Scope |
| --- | --- | --- |
| `/xrpc/social.coves.feed.getTimeline` | `RequireAuth` | Communities the viewer subscribes to |
| `/xrpc/social.coves.feed.getDiscover` | `OptionalAuth` | Every community |
| `/xrpc/social.coves.communityFeed.getCommunity` | `OptionalAuth` | One community |
| `/xrpc/social.coves.feed.searchPosts` | `OptionalAuth` | Every community, or one |

On the `OptionalAuth` routes a signed-in viewer gets their own vote state and block filtering. An anonymous caller gets neither.

The community feed is served as `social.coves.communityFeed.getCommunity`, but its lexicon is `social.coves.feed.getCommunity` and no `communityFeed.*` lexicon exists. The frontend and mobile clients call the served name; a third-party client that follows the published lexicon gets a 404. `feed/getAll.json` is also published with no route. The fix is an alias route under the lexicon name, then a client switch, then removing the old path. Register every new endpoint under its lexicon id.

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

Timeline and community Hot cursors carry `created_at::uri::query_time`, not the rank. The next page recomputes the anchor post's rank from its current row at the pinned query time with the same SQL expression as the candidates, so float rounding is identical on both sides of the comparison and posts do not drift across pages as time passes.

## Hot rank

```text
(SIGN(score) * LN(ABS(score) + 1) + 1) / (GREATEST(age_hours, 0) + 2)^1.5
```

The numerator is log-damped because bridged and federated communities import vote counts in the hundreds: going from 0 to 10 votes counts as much as going from 10 to about 120, so a day-old bridged post cannot bury fresh native posts. A zero-vote post keeps a numerator of 1. `SIGN`/`ABS` keep the expression defined for negative scores, since `LN` of a non-positive value errors in Postgres. `GREATEST(age, 0)` ranks a future-dated post as brand new instead of boosting it, and ingestion also clamps future `createdAt`.

`hotRankSQL` in `internal/db/postgres/feed_repo_base.go` is the only builder of this expression. The live `ORDER BY` and the cursor comparison both call it so they cannot drift. Timeline Hot and community Hot use it. Discover Hot ranks in Go and must equal this formula at neutral settings. Do not change the shared formula to tune Discover.

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

Only Discover with sort `hot`, including the default sort, uses this ranking. Discover New and Top use the shared feed query. It exists because high-vote bridged communities took most of Discover: in a September 2026 sample two communities held 60% of the first 50 posts.

### Ranking

The ranking time `T` is captured with the snapshot. The baseline cohort is posts created in `[T - 14 days, T - 24 hours)` that are publicly visible and not deleted. Viewer blocks do not change it and zero-score posts count. Each post contributes `ln(1 + max(score, 0))`.

`G` is the median of per-community means over communities with at least five cohort posts. Each community counts once, so a prolific community cannot pull the reference toward itself. A community's smoothed baseline is `B_c = (sum_c + 20G) / (n_c + 20)`, and its adjustment is `clamp(sqrt(max(G, 1) / max(B_c, 1)), 0.5, 1)`. The adjustment applies only to positive scores: the numerator is `1 + adjustment * ln(1 + score)`. Zero gives 1 and a negative score gives `1 - ln(1 + |score|)`, as in the shared formula. No community gets a multiplier above 1. With no qualifying community, or for a community with no or all-zero history, the adjustment is 1.

The hosted bonus (algorithm version 2, approved 2026-09-18) adds `3.0 * u` to the numerator of posts with score at least 0 in hosted communities. `u` is the first 8 bytes of `HMAC-SHA256(CURSOR_SECRET, "discover-hot-hosted-bonus\0" + URI)`, big-endian, shifted right 11 and divided by 2^53. Hosted posts sit at 0 or 1 votes while bridged posts import hundreds, so fresh hosted posts ranked below much older bridged ones. A flat boost was rejected because it would stack every fresh hosted post at the top. The bonus is keyed because authors choose their rkeys, and an unkeyed digest could be ground offline in about 1,000 tries for a near-maximum bonus. The label prefix separates it from cursor signatures, which use the same secret. It is deterministic, so snapshot rebuilds do not reshuffle the feed. "Hosted" means `pds_refresh_token_encrypted IS NOT NULL` (see [COMMUNITIES.md](COMMUNITIES.md)). The 3.0 maximum came from a production simulation where hosted posts reached about a fifth of the first 50 without lengthening the longest hosted run.

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
| Candidates per snapshot | 100,000; more fails with 503 rather than truncating |
| Stored candidates, all snapshots | 10,000,000 |
| Stored checkpoints | 20,000, each at most 64 KiB (database constraint) |
| Snapshot builds | 20 per minute globally, failed and cancelled builds included; one at a time under an advisory lock |
| Work deadline | 5 seconds, or the caller's earlier deadline |
| Cleanup | Every minute, 10,000 rows per batch |

The caps are enforced atomically in the database so several AppView processes share them.

### Errors

An expired, missing, malformed, tampered, wrong-viewer, wrong-sort or old-version cursor returns 400 `InvalidCursor`, and clients restart from page one. Bumping `discoverHotAlgorithmVersion` invalidates every outstanding cursor, and the old version's candidate rows keep counting against the stored-candidate cap for up to an hour.

Capacity refusal or a missed deadline returns 503 `DiscoverUnavailable` with `Retry-After: 30`. It never returns a truncated or empty page and never falls back to the legacy ranking. A continuation that fails this way does not advance its checkpoint. When the caller's own deadline expires, the error matches both `context.DeadlineExceeded` and `ErrDiscoverUnavailable`; a plain cancellation stays a cancellation. A pooled connection that held an advisory lock is discarded after an acquisition error, a cancellation or a failed unlock, so a lock can never leak back into the pool.

The frontend and mobile 1.4.0+11 restart from page one on `InvalidCursor` and honour `Retry-After`. Older mobile installs need a pull-to-refresh after a cursor expires.

### Known limitations

- A community with no mature baseline keeps its full raw engagement during its first day. Immature posts are deliberately not used as a provisional baseline.
- The baseline uses the current scores of mature posts, not their scores at 24 hours, because historical aggregate votes are not stored.
- Every post in a hosted community gets the bonus, aggregator posts included. Revisit this as native posting grows.
- The first downvote on a zero-score hosted post removes the whole bonus, dropping the numerator from up to 4.0 to `1 - ln 2` (about 13x), against about 3.3x for a non-hosted post. One account can do this. A taper over the first few negative scores is the fallback if it is abused.
