# Discover Hot: community normalization and diversity

Status: implemented and reviewed; `make ci` passed with 7,329 T0/T1 and 537 E2E tests, zero failures and zero skips. Client expiry recovery is implemented in the sibling frontend and mobile branches; deploy those clients before this backend rollout.

Date: 2026-09-12. Branch: `tdd/discover-hot-ranking`.

## 1. Product decision and scope

The approved direction is:

> Existing freshness decay + partially community-normalized engagement + a repeat-community penalty.

The user also selected **stable snapshot pagination**: scrolling preserves a ranking session, while refresh gets newly eligible posts and updated rankings. Current visibility and blocks still apply on every request. The user accepted up to 30 seconds of same-scope snapshot reuse on refresh.

Apply this to **Discover Hot only**, including requests that default to Hot. Timeline Hot, community Hot, and Discover New/Top retain their existing ranking behavior. Do not change the shared Hot formula to implement Discover's new policy.

Native-specific slots, native-community multipliers, subscriber weighting, author penalties, and an additional age taper are outside this version. Do not infer human authorship from community hosting or imported vote counts.

**Amended 2026-09-18 (algorithm version 2):** the ranking is no longer source-neutral. Posts in Coves-hosted communities receive the deterministic numerator bonus specified in §3.5. Every other statement in this section stands.

### Intended outcome

- High-vote communities lose some automatic ranking advantage.
- A prolific community cannot buy most of Discover merely by supplying many competitive posts.
- Quiet communities are not rewarded for inactivity, and zero-vote imports do not become equivalent to successful posts simply because zero is normal there.
- Recent posts compete better with ordinary older high-vote posts, without requiring strictly chronological ordering.
- Diversity persists across page boundaries without losing deferred posts.

### Observed motivation

Read-only production sampling around 2026-09-12 04:55 UTC found comicstrips and selfhosted occupying 60% of both the first 20 and first 50 posts. Of 896 publicly visible posts created in the preceding two weeks, 632 were aggregator-authored, 263 bridge-authored, and one native human-authored. These are dated observations, not permanent assumptions or test expectations.

A later sample showed 46–50-hour-old comics with scores around 381–529 above approximately 15-hour-old zero-vote news imports. The exact reported two-day-old versus seven-hour-old ordering was not reproduced. Under the current formula, a 48-hour-old 400-point post loses to a seven-hour-old zero-vote post, but beats a 14-hour-old zero-vote post.

## 2. Integration boundary

- `internal/core/discover/service.go` validates/defaults the request and delegates to the repository.
- `internal/db/postgres/discover_repo.go` retains the legacy filtered SQL query and shared cursor for Discover New/Top, and dispatches Hot to the snapshot path.
- `internal/db/postgres/feed_repo_base.go` contains the Hot expression, HMAC cursor helpers, and shared hydration. Its Hot formula is also used by Timeline and community feeds.
- `internal/db/postgres/post_visibility.go` centralizes collection-aware admission visibility, including community/post join keys and accepted-CID equality.
- `internal/db/postgres/viewer_block_filter.go` supplies viewer author-block and community-block filters.
- `internal/atproto/lexicon/social/coves/feed/getDiscover.json` limits both request and response cursors to 500 characters.

The implementation adds a Discover-specific Hot path, keeps New/Top on the existing path, and retains the external response shape and service/repository entry point.

Visibility remains exactly as currently defined: public accepted content, grandfathered eligible legacy records, and the authenticated author's existing access to their own admission states. A missing indexed author must not hide an otherwise eligible post. Historical normalization observations use only publicly visible content, not author-only states.

## 3. Ranking specification

The following numeric values are the settled backend parameters evaluated in section 7. Snapshot lifetime still has a client rollout dependency.

### 3.1 Preserve freshness

For ranking time `T` captured with the snapshot:

```text
age_hours = max((T - created_at) / one_hour, 0)
freshness_denominator = (age_hours + 2)^1.5
```

Preserve the existing exponent, offset, and future-date clamp. Do not substitute `indexed_at`, add a new post-age cutoff, or add the proposed post-24-hour taper in this version.

### 3.2 Community engagement baseline

Use posts created in `[T - 14 days, T - 24 hours)`. They must be publicly visible under the existing admission/CID rules and not deleted. Viewer-specific blocks do not change the community baseline. Zero-score posts participate in the baseline.

For each historical post:

```text
observation = ln(1 + max(net_score, 0))
```

Use the arithmetic mean of these log-transformed observations. This reduces viral-outlier influence without adding a separate winsorization policy. `net_score` is the existing stored score, including indexed atProto votes and imported aggregate votes; do not reweight votes by source in this version.

Let `n_c` be a community's cohort size and `sum_c` its summed observations. For each community with at least **five** eligible historical posts, calculate its mean `sum_c / n_c`. Define the reference `G` as the **median of those community means**, giving each qualifying community one observation regardless of posting volume. For an even number of communities, use the arithmetic mean of the middle two values.

This replaces the initial post-weighted pooled reference: a prolific community must not pull the reference toward itself merely by publishing more posts. Communities with fewer than five mature posts do not establish the shared reference, but their posts still contribute to their own smoothed baseline:

```text
B_c = (sum_c + 20 × G) / (n_c + 20)
```

When no community has five eligible historical posts, use neutral normalization for all candidates. Otherwise, a community with no historical cohort has baseline `G` and neutral normalization.

An empty-baseline community can therefore retain more of its raw engagement advantage during its first day than an established high-vote community. Treat this as a documented cold-start limitation, not an explicit boost: no multiplier exceeds one, and the repetition penalty still applies. Measure first-day share for these communities. Do not use immature posts as an unapproved provisional baseline in this version.

These are current scores of mature posts, not reconstructed scores at exactly 24 hours of age. Existing storage does not provide the historical aggregate-vote observations needed for the latter.

### 3.3 Partial normalization

For communities with a baseline, start with:

```text
community_adjustment = clamp(sqrt(max(G, 1) / max(B_c, 1)), 0.5, 1)
```

The floor is in log-engagement units. Apply the adjustment only to positive engagement:

```text
score > 0: numerator = 1 + community_adjustment × ln(1 + score)
score = 0: numerator = 1
score < 0: numerator = 1 - ln(1 + abs(score))

base_rank = numerator / freshness_denominator
```

This is intentionally partial, one-sided correction: no community receives a multiplier above one, and at most half the positive log-engagement contribution is removed. The positive vote contribution remains positive. Missing/all-zero history cannot create a large boost. Negative-score posts retain the existing numerator rather than becoming less negative through normalization.

Displayed scores and vote counts remain the actual counts. Ranking inputs are AppView-derived metadata only.

Compute the new Discover base rank in Go under `internal/core/discover` from frozen score, timestamp, baseline and `T`; the database supplies consistent inputs. Persist the computed value as PostgreSQL `double precision` and reuse it verbatim on continuation. Reject nonfinite computed values as errors rather than emit an unstable order. The existing SQL `hotRankSQL` remains authoritative for the unchanged legacy Hot behavior: add a T1 equivalence test comparing Go with that SQL at neutral normalization, using absolute/relative tolerance `1e-12`, across positive/zero/negative scores, future dates and representative ages. Do not use an epsilon comparator for ordering; use the stored values and deterministic tie-breakers.

### 3.4 Progressive community repetition penalty

Select posts sequentially using a sliding window of the preceding **20 emitted posts**, not the current HTTP page. Let `count_c` be a candidate community's appearances in that window:

```text
penalty = 1 + 0.5 × count_c

base_rank >= 0: adjusted_rank = base_rank / penalty
base_rank < 0:  adjusted_rank = base_rank × penalty
```

Choose the highest adjusted rank, with `created_at DESC, uri DESC` as deterministic tie-breakers. Add the selected community to the window and expire its oldest entry when necessary. This is a soft penalty, not a quota or a guaranteed interleave. Single-community supply still fills pages normally.

The sign-safe branches ensure repetition never improves a negative rank. The adjustment preserves base-rank order within a community. Therefore only the best remaining candidate from each community can win the next selection.

### 3.5 Hosted-community bonus (algorithm version 2)

Approved 2026-09-18. Bridged communities import vote counts in the hundreds while posts in Coves-hosted communities sit at zero or one vote, so fresh hosted posts ranked below much older bridged posts. A flat boost was rejected because it would stack every fresh hosted post at the top of the feed.

```text
hosted  = communities.pds_refresh_token_encrypted IS NOT NULL
mac     = HMAC-SHA256(key = CURSOR_SECRET, message = "discover-hot-hosted-bonus\0" + post URI)
u       = (first 8 bytes of mac, big-endian) >> 11, divided by 2^53    # [0, 1)
bonus   = 3.0 × u          when hosted and score >= 0
bonus   = 0                otherwise
numerator = (numerator from §3.3) + bonus
```

- `hosted` is the same fact as `internal/db/postgres/community_hosted.go`: the presence of the stored credential. It is never decrypted, and `hosted_by_did` is a claim anyone can write, so it is not used.
- The bonus depends only on the post URI and the server's `CURSOR_SECRET`. It has no clock and no per-process randomness, so it is identical across the 30-second snapshot rebuilds for the post's lifetime and the feed does not reshuffle on refresh.
- It is keyed because the author chooses the URI: a `postv2` record lives at `at://<author DID>/social.coves.community.postv2/<author-chosen rkey>`. An unkeyed digest could be ground offline, about 1,000 rkeys for a near-maximum bonus on every post. The label prefix separates the bonus from cursor signatures, which use the same key. Rotating `CURSOR_SECRET` reshuffles every bonus; rotation already invalidates every cursor.
- It is additive and independent of score, so an upvote never lowers a post's rank. Negative-score posts receive no bonus. This is a deliberate hard cutoff: the first downvote on a zero-vote hosted post removes the whole bonus, taking the numerator from as much as 4.0 to `1 - ln 2` (about 13x), against about 3.3x for a non-hosted post. Hosted posts sit at 0-1 votes, so one account can do this. A taper over the first few negative scores is the alternative if it is abused.
- A post with `u` near zero ranks where it did under version 1. Freshness decay, partial normalization and the repetition penalty are unchanged, and the repetition penalty remains the guard against one hosted community filling a page.
- With a bonus of zero the Go rank still equals the legacy `hotRankSQL` expression at neutral normalization.
- The algorithm version moved from 1 to 2. Version 1 snapshots are not reused and their cursors return `InvalidCursor`, which clients already recover from (§7.2). Version 1 candidate rows keep counting against the stored-candidate limit until they expire, so stored-candidate headroom is reduced for up to one snapshot lifetime (one hour) after the deploy.
- Eligibility is every post in a hosted community, aggregator-authored or human. Revisit this as native human posting grows.

Read-only production simulation (run with the unkeyed digest; the keyed value has the same uniform distribution), snapshot `2026-09-19T00:41Z`: 1,217 publicly visible posts from the preceding 14 days. Posts under 24 hours old: 44 hosted (all aggregator-authored, scores 0-1, 5 communities) and 59 bridged.

| Maximum bonus | Hosted posts in first 20 / 50 / 100 | Longest hosted run in first 100 | Posts older than 40h ranked above a hosted post younger than 18h |
|---:|---:|---:|---:|
| 0 (version 1) | 1 / 2 / 19 | 4 | 21 |
| 1.5 | 1 / 7 / 30 | 2 | 5 |
| 2.0 | 1 / 7 / 33 | 3 | 4 |
| **3.0** | **2 / 11 / 36** | **4** | **1** |
| 4.0 | 3 / 13 / 38 | 3 | 0 |

`3.0` was chosen: hosted posts reach 22% of the first 50 against 43% of fresh supply, without lengthening the longest hosted run. This is one snapshot, and aggregator posts arrive in batches, so the mix varies by time of day. These are dated observations, not test expectations.

## 4. Candidate selection and hydration

Build ordered candidate streams per community from the complete snapshot-eligible metadata. Compare their leading candidates and refill the winning stream after selection. A global top-50 or fixed per-community cap must not hide an alternative or terminate pagination early.

Apply the existing viewer visibility and block filters before selection. Hydrate full post views only for selected candidates, preserving emitted order and existing vote-state/embed enrichment. Reuse centralized visibility predicates and shared scanners; do not copy policy SQL into a second divergent implementation.

The snapshot has no new age cutoff. That makes metadata volume, baseline-query cost, and first-page latency explicit feasibility checks before finalizing storage. Do not silently solve a performance issue by excluding older eligible posts or truncating the feed.

## 5. Stable snapshot pagination

### 5.1 Why a new cursor path is necessary

The existing cursor identifies the last displayed post and filters using its ranking boundary. After diversity selection, higher-base-rank posts may have been deferred. Advancing that boundary can make them unreachable. Recent-community history alone also cannot recover deferred candidates or freeze changing scores.

Freeze ranking inputs in short-lived Postgres state, rather than carrying an expanding candidate/history list in a 500-character cursor. Use existing Postgres infrastructure, not a new caching service.

### 5.2 Logical state

The exact physical schema follows the capacity evaluation, but must represent:

1. **Snapshot:** identifier, algorithm version, viewer scope (authenticated DID or anonymous), database-consistent ranking timestamp, creation time, expiry, and frozen candidate membership/ranking inputs.
2. **Candidates:** post identity, community, base rank and deterministic tie-breakers, organized into refillable ordered streams. Store metadata, not copied post bodies or credentials.
3. **Immutable continuation checkpoints:** snapshot reference, consumed stream positions and recent emitted-community window. Deferred candidates remain reachable.

Retain checkpoints rather than precomputing a complete diversified sequence with a position cursor. With a precomputed sequence, a post hidden before delivery still influences the diversity of posts that follow it. That contradicts the emitted-post window in section 3.4 and the live-filter-before-selection contract. Checkpoints are narrowly scoped to this requirement, not a generic feed-session framework. Deduplicate identical continuation state within a snapshot so repeated identical requests do not create unbounded duplicate checkpoint rows.

Capture cohort statistics, candidates and ranking time under a consistent database snapshot; independent queries at default read-committed isolation are insufficient. Do not hold a database transaction open across HTTP requests.

Use a signed, versioned cursor referencing a checkpoint. Bind it to Discover Hot and the request's viewer scope, and enforce the lexicon length bound. Validate malformed, oversized, tampered, unknown-version, wrong-viewer, expired, and wrong-sort cursors. Old Discover Hot cursors should return `InvalidCursor`, not silently restart the feed; other feed sorts retain their cursor formats.

### 5.3 Continuation semantics

- Refresh/no cursor begins at an empty emitted window using a new or reusable snapshot under section 5.4. Existing scrolls keep frozen ranking inputs even when votes or baselines change.
- Replaying a cursor with the same page size and unchanged visibility yields the same sequence. A GET must not destructively advance a shared mutable session pointer. Concurrent retries must be safe.
- Different page sizes produce the same concatenated selection sequence under unchanged visibility.
- Current blocks, deletions, and admission/CID rules override snapshot continuity. Revalidate before selection and serving, skip excluded candidates, and refill the page to eligible exhaustion. Excluded candidates do not consume newly emitted diversity history.
- Previously emitted posts remain part of the historical emitted window even if later hidden; do not retroactively rewrite a reader's prior pages.
- New posts, newly accepted content, and unblocked candidates excluded from or passed over in the snapshot become discoverable when refresh uses a new snapshot. Within the proposed 30-second reuse interval, refresh may reuse the earlier candidate membership. State this refresh contract explicitly.
- Snapshot membership is traversed exactly once, except current visibility exclusions. Hard deletion of a former page anchor must not invalidate unrelated remaining candidates.
- Lookahead used to decide whether a cursor is needed must not consume the next post from the returned checkpoint.
- State must survive a new repository instance or AppView restart and work for application instances sharing the same database and cursor secret.

### 5.4 Snapshot reuse, expiry and resource lifecycle

**Refresh policy:** reuse the latest snapshot for a scope if its capture time is no more than **30 seconds** old. Scope is `(algorithm version, anonymous)` for the shared public snapshot, or `(algorithm version, authenticated viewer DID)` for a viewer's own snapshot. Never share author-only candidate membership between viewers. Readers sharing a snapshot start from the same root checkpoint but continue independently.

Coalesce concurrent creation for the same scope with a database-coordinated lock so multiple AppView processes cannot all materialize it. A recent snapshot's current visibility and block filters are still applied. Do not extend its absolute expiry when reused. The result is refresh freshness bounded by 30 seconds, rather than a full-feed materialization for every anonymous landing-page request. Confirm that refresh nuance before RED; if immediate refresh is required, revise the sharing design instead of silently violating it.

The backend uses a **one-hour absolute snapshot lifetime**, not a sliding expiry. Expired or missing state returns HTTP 400 `InvalidCursor`. Section 7 records why this cannot roll out safely until client recovery behavior changes or the product explicitly accepts the current failure mode.

Settled backend operational budgets:

| Budget | Initial ceiling |
| --- | --- |
| Concurrent full snapshot builds across the AppView instances | 1; a database session advisory lock serializes capacity admission and materialization |
| Full snapshot builds across all scopes | 20 per minute, plus at most one per scope per 30 seconds |
| Eligible candidate metadata in one snapshot | 100,000 posts |
| Aggregate stored/reserved candidate entries | 10,000,000 |
| Aggregate stored continuation checkpoints | 20,000 rows, with at most 64 KiB serialized state per row |
| Snapshot build or continuation request work deadline | 5 seconds, or the caller's earlier deadline |
| Cleanup cadence / maximum derived rows deleted per transaction | 1 minute / 10,000 rows |

Enforce shared caps atomically in the database. Durable build-attempt rows count admitted expensive work even when a build fails or is canceled; cleanup removes attempt rows after their one-minute accounting window. Count expired snapshots until actually purged. The 64 KiB checkpoint ceiling and 20,000-row cap bound serialized checkpoint payload to approximately 1.25 GiB before row/index overhead. Candidate rows remain separately capped at 10,000,000. One-hour retention still requires traffic/storage observation after rollout is unblocked.

If a build cannot obtain capacity, reuse a same-scope snapshot **only if it is within the 30-second refresh bound**. If none is reusable, return HTTP 503 with an explicit XRPC `DiscoverUnavailable` error and a bounded retry hint (initially `Retry-After: 30`). A continuation that cannot complete within its deadline or checkpoint capacity returns the same retryable error without advancing its input checkpoint. Expired/malformed cursors remain `InvalidCursor`; ordinary unexpected failures retain normal server-error handling. Add the new capacity error mapping in `internal/api/handlers/discover/errors.go`.

A snapshot exceeding the candidate ceiling must fail explicitly with the capacity error; never store the first 100,000 rows and report false exhaustion. Capacity refusal must not evict an unexpired reader's state, silently serve an older-than-promised refresh, fall back to legacy ranking, or return an apparently successful empty page. Test sharing, caps, capacity recovery, cancellation and these error distinctions. Use injected low budgets and clocks in tests rather than enormous fixtures or sleeps.

Use `cmd/server/jobs.go`'s existing `runTicker` lifecycle for bounded, batched expired-state cleanup, with context cancellation and an indexed expiry lookup. Delete expired candidates/checkpoints in bounded transactions before removing their parent snapshots; a large cascade must not bypass the deletion budget. Failed creation must roll back partial candidates and state. Measure state growth from repeated cursor branches and consider only compact metadata storage; do not ship unmeasured per-request full-feed materialization.

## 6. Implementation sequence and files

Use an isolated worktree, separate RED test-author and GREEN implementation agents, and the repository's TDD workflow. One behavioral chunk must reach a verified green state before the next is implemented.

1. **Evaluation and contract:** completed for ranking and backend budgets; client expiry behavior was validated and is a rollout blocker recorded in section 7.
2. **Outer RED:** real Postgres -> Discover repository/service -> in-process HTTP handler acceptance test demonstrating normalized community competition and diversity continuing across pages.
3. **Normalization:** T0 tests for baseline and scoring, then implementation; T1 verifies historical-cohort SQL eligibility.
4. **Sequential selection:** T0 tests for progressive repeats, negative scores, sliding-window expiry, ties and page-size independence, then implementation.
5. **Snapshots and checkpoints:** T1 tests for immutable inputs, filtered candidate streams, cursor scope/replay, expiry, restart, cleanup and rollback, then implementation.
6. **Discover wiring and regression gate:** connect only Discover Hot, complete the outer contract, prove other feed sorts retain their order, and evaluate the settled implementation against the data used in step 1.

Expected file areas:

| Area | Responsibility |
| --- | --- |
| `internal/core/discover/` | Pure baseline, normalization and sequential-selection logic with T0 tests |
| `internal/db/postgres/discover_repo.go` | Discover Hot dispatch while retaining New/Top |
| Discover-specific files under `internal/db/postgres/` | Baseline queries, candidate/snapshot storage, continuation and filtered hydration |
| `internal/db/migrations/` | Required snapshot/checkpoint tables and indexes; choose current migration number at implementation time |
| `internal/api/handlers/discover/errors.go` | Explicit retryable snapshot-capacity error mapping |
| `cmd/server/` and existing lifecycle wiring as needed | Expired-state cleanup lifecycle |
| `internal/core/discover/discover_feed_test.go` | Public service/HTTP acceptance coverage against real Postgres |
| `docs/DISCOVER_HOT_RANKING_PLAN.md` | Settled parameters, evaluation, implementation/review status |

Coordination: an existing `nightshift/discover-feed-full-table-scan` branch changes Discover queries, shared feed code and visibility SQL, and carries a migration numbered 047. **This worktree already has `047_web_oauth_binding.sql`**, so the nightshift migration conflicts and must be renumbered if it lands on this base. This plan's new migrations must use 048 or later, after checking the then-current sequence. The nightshift branch is not a dependency and must not be overwritten or merged implicitly. Recheck its merge state and preserve relevant query/index improvements if it lands first.

## 7. Evaluation and acceptance criteria

Compare the current algorithm and a small set of normalization/repetition strengths on fresh production snapshots. Historical replay must not pretend today's aggregate votes were the scores at an earlier date. Keep dated real-data findings separate from deterministic synthetic test scenarios.

Measure:

- Largest community's share of the first 20 and 50, unique communities, and repeated-community runs.
- Realized `community_adjustment` and reference/sample sizes for the five most represented communities, rather than assuming the maximum 0.5 correction occurs in real data.
- Aggregator share and age distribution, to detect replacing comicstrips dominance with automated-news dominance.
- First-day share of communities with empty mature baselines, including newly bridged bulk imports.
- Placement of ordinary 36–60-hour-old comics versus 7–15-hour-old zero-vote imports.
- First-page and continuation latency, candidates examined, snapshot storage and checkpoint growth.

### 7.1 Production-derived evaluation

A read-only sample captured at `2026-09-12T09:03:12Z` contained 14,101 eligible candidates, 830 mature-cohort posts, and 10 qualifying communities. The median community reference was `G = 1.4161`. Realized adjustments were comicstrips `0.538`, linux `0.666`, selfhosted `0.704`, fediverse `0.793`, Star Trek `0.798`, and `1.0` for the sampled aggregator communities.

| Metric | Legacy Hot | Settled normalization + 0.5 repeat step |
| --- | --- | --- |
| First 20 largest-community share | 7/20 | 5/20 |
| First 20 unique communities / longest run | 5 / 4 | 6 / 2 |
| First 20 source mix | 20 bridge, 0 aggregator | 17 bridge, 3 aggregator |
| First 20 median / p90 / maximum age | 12.9h / 19.1h / 23.0h | 10.9h / 14.4h / 34.6h |
| First 50 largest-community share | 18/50 | 11/50 |
| First 50 unique communities / longest run | 7 / 4 | 10 / 2 |
| First 50 source mix | 44 bridge, 6 aggregator | 36 bridge, 14 aggregator |
| First 50 median / p90 / maximum age | 18.4h / 45.1h / 54.2h | 16.9h / 36.8h / 53.1h |

The best sampled 36-60-hour comic with score at most 400 moved from position 33 to 49. The best 7-15-hour zero-score post moved from position 28 to 15. The proposed `0.25`, `0.5`, `0.75`, and `1.0` repeat steps had similar largest-community counts at 20; `0.5` was retained because it halved the longest run without adding the stronger source-mix displacement of `0.75`/`1.0`. All 65 first-day posts belonged to communities with a mature baseline, so the cold-start limitation did not activate in this sample.

Production `EXPLAIN ANALYZE` measured 12.2 ms for the history query and 29.3 ms for the complete 14,101-row candidate scan. Candidate metadata averaged 129 bytes of raw column payload, or 1.78 MiB for this sample before table/index overhead. A local Go stress run over 100,000 synthetic candidates selected a 15-post page in 6.6 ms with 10 communities and 88.3 ms with 100,000 communities, allocating 32.4 MiB and 21.9 MiB respectively. The five-second request deadline turns growth beyond feasible work into retryable refusal rather than unbounded request time. These are feasibility measurements, not a substitute for post-rollout heap/index/WAL/autovacuum observation.

### 7.2 Client recovery rollout dependency

The original clients did not self-recover from the one-hour expiry contract:

- `coves-frontend` leaves loaded posts visible but its Retry action resends the same expired cursor indefinitely. A full browser refresh recovers. It also discards `Retry-After` while parsing 503 XRPC errors.
- `coves-mobile` leaves loaded posts visible but its Retry action resends the same expired cursor indefinitely. Pull-to-refresh recovers. It does not consume `Retry-After` for 503 responses.

The sibling `coves-frontend` and `coves-mobile` branches now restart Discover Hot from page one on `InvalidCursor`, preserve replacement semantics, and honor bounded integer `Retry-After` values on `DiscoverUnavailable`. Deploy those client changes before this backend so existing sessions cannot become stuck on expired cursors.

Example arithmetic, not a hard ordering policy: a 48-hour-old 400-point post with the maximum proposed normalization changes from approximately `0.0198` to `0.0113`, below a 15-hour-old zero-vote post at approximately `0.0143`, before repetition penalties. An exceptional older post may still win. If ordinary old-post dominance remains after calibration, return to the user about the additional age taper rather than add it unilaterally.

Behavioral acceptance:

- Community-relative correction changes competition in a controlled high-baseline versus low-baseline fixture while preserving a positive advantage for actual engagement.
- Increasing only an already well-sampled community's posting volume, while retaining its score distribution, cannot move the shared reference `G`; record a high-volume fixture where normalization remains materially below 1 (target at most 0.8 for the designed fixture). Small-sample smoothing may still change that community's own baseline as confidence increases.
- Empty/all-zero and sparse communities have bounded, nonrewarding normalization; outliers do not define the entire baseline.
- Repetition reduces a represented community's competitiveness and expires with the sliding window; no source-specific allocation is required.
- An alternative outside a naive global candidate cutoff can win, and deferred eligible posts remain reachable exactly once.
- Frozen scores/baselines/membership prevent pagination drift; current visibility exclusions remain authoritative.
- A single eligible community, tied ranks, negative scores, future timestamps, full final pages, partial final pages and true exhaustion work correctly.
- Timeline/community Hot and Discover New/Top retain their existing ranking and visibility behavior.

## 8. Tests and completion gates

Follow `docs/TEST_ARCHITECTURE.md`. Put behavioral breadth at T0/T1, not in a large new E2E scenario.

**T0:** community-balanced reference, minimum sample and even/odd median definitions; baseline boundaries; neutral/cold-start fallback and smoothing; score monotonicity and bounds; unchanged age function and nonfinite rejection; sign-safe diversity; window expiry; deterministic ties; page-size-independent selection; cursor validation; injectable budget/error decisions.

**T1:** exact URI sequence through the real Discover HTTP read path; neutral Go/SQL score equivalence; baseline SQL exclusion of nonpublic/CID-drifted/deleted/out-of-window records; author-only visibility preservation for candidates; author/community blocks before selection; unknown authors; alternatives beyond oversampling cutoffs; immutable checkpoint replay and concurrent retries; anonymous sharing and authenticated scope isolation; coalesced creation and atomic global budgets across repository instances; checkpoint deduplication; score/baseline changes and new/backdated insertions between pages; live removals/blocks and refill; hard-deleted anchors; cursor scope/version/expiry; restart; lookahead/exhaustion; 503 versus InvalidCursor semantics and retry hints; no partial-success truncation on capacity refusal; bounded cleanup, cancellation and transactional rollback; exact-order checks for unaffected feeds.

**T2:** extend a narrow existing real-ingestion read-visibility check to Discover Hot if needed to verify the new path is wired into the running AppView. Do not duplicate the T0/T1 ranking matrix here.

Commands for implementation verification:

```bash
make test
POSTGRES_TEST_TEMPLATE=coves_discover_hot_<unique_run> make test-integration
make ci
```

Choose an actual unique template identifier per run; the placeholder above documents the required isolation. `make ci` is the merge gate. No skipped tests, sleep-based synchronization, or public-infrastructure dependencies in hermetic tests.

The implementation passed the full merge gate and multi-model review. Client recovery was reviewed separately in each client repository.

## 9. Fable 5.1 plan review and disposition

Reviewed on 2026-09-12 using **Claude Code `claude-fable-5-1`, medium effort**, read-only and scoped to this document. Only the Fable portion of second-opinion-mini ran, as requested; no Astra reviewer ran. The initial review rated the plan **good** and returned one high, three medium, and two low findings. This revision incorporates the decisions below; the revised text has not received a second Fable pass.

| Finding | Decision |
| --- | --- |
| Unbounded anonymous refreshes would repeatedly materialize the full feed | Accepted. Propose 30-second same-scope sharing, coalesced creation, explicit global budgets, retryable capacity failures, and bounded cleanup. The refresh-staleness refinement is recorded for confirmation before implementation. |
| Replace stream checkpoints with a precomputed order and position cursor | Not adopted. Simpler storage would change the promised emitted-post diversity semantics after live visibility exclusions. Retain minimal checkpoints and deduplicate identical state. |
| A post-weighted reference can be dominated by a prolific community | Accepted. Use the median of qualifying per-community means and measure actual adjustments. The review's reference to comicstrips having 60% of the historical cohort was inaccurate: 60% described comicstrips plus selfhosted's served-feed share. The structural weighting problem remains valid independently of that example. |
| Ranking computation location and operational limits were underspecified | Accepted. Define Go ranking, stored double-precision ranks, neutral equivalence to legacy SQL, numeric proposed budgets and observable error behavior. |
| Empty-history communities retain full raw engagement during cold start | Documented as an accepted limitation of this proposed version, with first-day monitoring and tests. Do not silently introduce immature-post baselines. |
| Migration 047 already exists on main | Accepted. Explicitly document the web-OAuth migration and the other branch's numbering collision. |

Implementation settled the 30-second refresh policy, coefficients, and bounded backend budgets. Client expiry/capacity recovery was implemented and remains a rollout-order dependency in section 7.2. The approved ranking direction and stable scrolling remain unchanged.
