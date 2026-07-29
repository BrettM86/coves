# Coves Test Architecture

**Status: PROPOSED — this document is the spec for the test-suite refactor on branch `worktree-test-refactor`.**
It replaces `TESTING_SUMMARY.md` and `docs/E2E_TESTING.md`, both of which describe a system that no longer exists (wrong ports, references to files that were never written, no mention of the real gate `make ci`). Delete both when Phase 6 lands.

---

## 1. Goals

We work agentically. The test suite is the thing that lets an agent (or a human) change code quickly and *know* whether it caused a regression. That only works if:

1. **Green means deployable.** A passing run must certify the whole pipeline — endpoint → PDS → firehose → consumer → Postgres → serving endpoint — not just the parts whose infrastructure happened to be up.
2. **Red means a regression.** No flakes. A test that fails 1-in-20 runs trains agents and humans to retry instead of investigate, which is worse than not having the test.
3. **Skips are failures unless explicitly allowlisted.** Today there are 449 skip sites and `make test-all` counts every one as a pass. Stop the PDS and the suite still prints green. `cmd/ci-report` already inverts this — it becomes the only skip mechanism.
4. **Fast feedback tiers.** An agent iterating on a service should get signal in seconds (unit), on a repo in ~a minute (integration), and full pipeline certification in minutes (e2e) — each tier runnable independently.
5. **Hermetic means hermetic: tests NEVER touch public atProto infrastructure.** No `plc.directory`, no Bluesky relays/Jetstreams, no public PDSes — everything routes through our self-hosted Docker services (PLC directory, PDS, relay, Jetstream). This is a hard rule, enforced mechanically (3.7), with exactly one exception: the opt-in T3 `live` tier, which exists precisely so that reality checks are explicit instead of accidental.

## 2. Where we are (July 2026 survey)

The full survey lives in the PR description for this branch; the load-bearing facts:

- **~84k LOC of tests** (41k in `tests/integration` alone), **zero** `t.Parallel()`, **zero** `require.Eventually`, **zero** build tags. The only tier selector is `-short`, which silently skips ~161 guarded sites — `make test` runs almost none of the integration suite while printing green.
- **Taxonomy is inverted.** `tests/e2e/` contains two pure unit tests of the rate limiter (`httptest.NewRecorder`, no infra); `tests/integration/` contains the most end-to-end test in the repo (`user_journey_e2e_test.go`). 15 files named `*_e2e_test.go` live in `tests/integration`. Directory, filename, and function name each imply a different tier and none binds to what the test does.
- **Global mutable DB state.** `setupTestDB` (3 divergent copies) is called 222 times across 50 files; each call re-runs migrations and issues 6 unscoped `DELETE FROM`s against the one shared database on :5434. This is why `-p 1` is mandatory and parallelism is impossible. 109 more ad-hoc `DELETE FROM`s live inside test bodies.
- **~700 LOC of copy-pasted firehose plumbing.** 10 near-identical `subscribeToJetstream*` functions differing only in consumer type. The one correct anti-flake primitive — capture the Jetstream cursor *before* the PDS write, then subscribe from that cursor (`jetstreamCursorNow`/`withJetstreamCursor`, `helpers.go:152-177`) — is used at 1 of 29 dial sites. The other 28 paper over the subscribe-after-write race with `time.Sleep(500ms)` (22 sites of exactly that constant).
- **The consumer wiring is never tested.** Every "E2E" test instantiates its *own* consumer and feeds it events; `cmd/server`'s actual consumer wiring runs in production only. The single test that goes through a live AppView (`tests/e2e/user_signup_test.go`) exercises a path that bypasses Jetstream.
- **Coverage is inverted relative to risk.** `internal/core/communities` (7 src files), `internal/core/votes` (6), `internal/atproto/identity` (7), `internal/api/routes` (17), `timeline`, `discover`, `communityFeeds`: **zero unit tests** — verified only through the slowest, most-skipped tier. `internal/db/postgres`: 16 repos, 3 test files.
- **The good news:** the hermetic CI harness (`make ci` → `scripts/ci.sh` → `docker-compose.ci.yml` → `scripts/ci-runner.sh` → `cmd/ci-report` + `tests/ci/allowed_skips.txt`) is the highest-quality artifact in the tree: hermetic stack, skip-inversion, staleness enforcement, mandatory skip reasons. The refactor builds *on* it, not around it.

## 3. Target architecture

### 3.1 Four tiers, mechanically enforced by build tags

| Tier | Tag | Lives in | Talks to | Wall-clock budget | Parallel |
|---|---|---|---|---|---|
| **T0 Unit** | *(none)* | in-package (`internal/…`, `cmd/…`) | nothing out-of-process | < 60 s total | yes, default |
| **T1 Integration** | `//go:build integration` | in-package, next to the code it tests | Postgres only | < 3 min total | yes (see 3.3) |
| **T2 Pipeline (E2E)** | `//go:build e2e` | `tests/e2e/` | full hermetic stack incl. running AppView | < 10 min total | per-domain |
| **T3 Live** | `//go:build live` | `tests/live/` | public internet (Bluesky API, unfurl targets, real PLC) | opt-in only | — |

Rules that give the tiers teeth:

- **Build tags are the only tier mechanism.** `-short`/`testing.Short()` is deleted everywhere. You cannot accidentally run (or accidentally *not* run) a tier.
- **Missing infrastructure is a FAILURE, not a skip.** If you invoked `-tags integration`, you asked for Postgres; if it's not there, `t.Fatal`. All 34 copy-pasted "PDS not running → t.Skipf" preambles are deleted. The harness (3.3) does one connectivity check in `TestMain` and fails fast with a message that says exactly which `make` target brings the stack up.
- **`t.Skip` is banned in test bodies.** The only legitimate skips are environmental gates owned by the harness, and every one must appear in `tests/ci/allowed_skips.txt` with a reason or `ci-report` fails the run. The 6 "DEBT — never run" entries in the current allowlist get deleted or implemented in Phase 2 — not carried forward.
- **`tests/integration/` and `tests/unit/` are dissolved** by the end of the migration. Repo/service/consumer tests move next to the code they test (T1); pipeline sagas become T2 contracts; the two rate-limiter "e2e" files become T0 tests in `internal/api/middleware`.

### 3.2 What each tier is *for*

**T0 — logic.** Validation, transforms, cursor encoding, error mapping, service logic against interface fakes. This is where behavioral breadth lives: every edge case, every error path. Packages currently at zero (communities, votes, identity, routes, timeline, discover, communityFeeds) get their matrix coverage here, not in 1,400-LOC integration files.

**T1 — the seams.** Two seams matter and both terminate at Postgres:
- *Repo tests*: real SQL against a real schema (`internal/db/postgres`, 16 repos, currently 3 tested).
- *Consumer tests*: synthetic `jetstream.JetstreamEvent` → `consumer.HandleEvent` → assert rows. This is today's "Strategy A" and it is legitimate — it just isn't E2E and stops being named that. Idempotency, out-of-order delivery, rev-gating, malformed records: all here, cheap and deterministic.

Feed/timeline/discover sorting-and-cursor tests (today's `feed_test.go`, `timeline_test.go`, `discover_test.go`, `comment_query_test.go` — DB-fixture-driven, no PDS) are T1 repo/service tests and move accordingly.

**T2 — the pipeline contract.** See 3.4. One black-box happy-path-plus-key-invariants contract per record type, through the *running* AppView. Narrow and deep, not broad.

**T3 — reality checks.** Live Bluesky handle resolution, real unfurl targets (YouTube/Reddit/Streamable/Kagi), real PLC. Valuable (real data catches what fixtures don't) but never on the merge path. Run nightly/pre-release via `make test-live`; failures notify, don't block.

### 3.3 One harness: `tests/testkit`

A single package imported by all tiers. Everything below already exists in embryonic form somewhere in the tree — the harness is mostly consolidation, and it kills the current 3×`setupTestDB` + 10×`subscribeToJetstream*` + 5×image-fixture + 4×PDS-client-factory duplication.

```
tests/testkit/
  testkit.go      // TestMain helper: env, log silencing, infra fail-fast probe
  db.go           // Postgres isolation (below)
  pds.go          // account/session/record helpers (absorbs helpers.go:84-277)
  firehose.go     // cursor-gated subscribe, ONE generic implementation
  wait.go         // the only wait primitives allowed in tests
  fixtures.go     // uniqueTestID, community/post/comment/user builders, PNG/JPEG bytes
  appview.go      // T2 only: XRPC client against the running AppView
```

**DB isolation — template-clone-per-test.** `TestMain` (via `testkit.Main`) migrates a template database once (`coves_test_template`). `testkit.DB(t)` executes `CREATE DATABASE … TEMPLATE coves_test_template` with a unique name, returns the pool, and `t.Cleanup` drops it. Cost is ~100–200 ms per test on local NVMe — trivially bought back by what it unlocks:

- `t.Parallel()` becomes the default at T0/T1; `-p 1` is deleted from every target.
- All 222 `setupTestDB` call sites, all 6-way `DELETE FROM` wipes, all 109 in-body `DELETE`s, and every per-file `cleanup*` function are deleted. Tests cannot see each other by construction, so ordering dependencies die at the root instead of being policed by convention.
- Tests that only read can share `testkit.SharedDB(t)` (template-backed, read-only assertion) if per-test clones ever become the bottleneck; don't build this until measured.

**Firehose — one generic subscriber, cursor-gated by construction.** The 10 copies differ only in consumer type and collection filter:

```go
// Capture cursor FIRST, then write, then Await — the subscribe-after-write
// race (helpers.go:152-166) is impossible to reintroduce through this API
// because NewFirehose is the only exported way to get a subscription.
fh := testkit.NewFirehose(t, testkit.Collections("social.coves.community.post"))
uri := writePost(t, pds)                       // real PDS write
ev := fh.Await(t, testkit.MatchURI(uri))       // replays from pre-write cursor; deadline built in
```

Internally it keeps the `maxConsecutiveTimeouts` guard and read-deadline handling from the existing copies (the gorilla/websocket panic guard, per `project_firehose_e2e_test_gotchas`), written exactly once. Consumers are *not* threaded through it — T1 consumer tests call `HandleEvent` directly with synthetic events; T2 tests don't touch websockets at all (the real AppView consumes; tests poll the read side).

**Waiting — two primitives, everything else is banned:**

```go
testkit.WaitFor(t, 30*time.Second, func() (bool, error) { … })   // poll, 100ms interval
fh.Await(t, match)                                               // firehose delivery
```

`time.Sleep` in `*_test.go` fails CI via a lint gate (Phase 1). All 35 sleeps, 39 hand-rolled `select`/`time.After` blocks, and 56 bare retry loops migrate to these.

**Identity — `testkit.UniqueID(t)`** is the only handle/ID generator (base36, atomic counter, ≤18-char PDS-safe — per `feedback_test_handle_length`). The surviving `time.Now().Unix()` handle-collision sites (`user_signup_test.go:69,110,153`, `community_e2e_test.go:177,380` — one inside a loop, guaranteeing same-second collisions) are migrated in Phase 1.

**Assertions — testify everywhere.** 36 of 57 integration files currently use bare `t.Errorf`. New/touched code uses `require` (fail fast) / `assert` (accumulate); no new bare `t.Errorf`.

### 3.4 The E2E methodology: pipeline contracts

This is the core opinionation and the reason the tier exists. **Every lexicon record type the AppView indexes gets exactly one pipeline contract test**, and that test is *black-box*:

```
XRPC/HTTP write endpoint (or direct PDS write for federation-path records)
  → PDS (real)
  → firehose → Jetstream (real)
  → THE APPVIEW'S OWN CONSUMERS (the cmd/server wiring, running as a container)
  → Postgres
  → XRPC serving endpoint, polled with WaitFor until the record appears
```

The rules:

1. **The test never instantiates a consumer and never dials a websocket.** Today's "Strategy B" (test-built consumer fed by a hand-rolled subscriber) tests a consumer object nobody deploys, and leaves `cmd/server/consumers.go` — the code that actually runs in prod — permanently unexercised. At T2 the AppView container does the consuming, exactly as in production, and the test observes via the serving endpoint. This deletes the entire `subscribeToJetstream*` family from the E2E tier.
2. **One contract per record type, shaped as create → read → update → read → delete → gone**, plus the record's *distinct* pipeline invariants (e.g. votes: idempotent re-tap; comments: thread placement; communities: `hostedBy` security). Steps within a contract are sequential *by design* — eventual consistency is a pipeline, and that's what's under test. That is the only tier where sequential subtests are legitimate.
3. **Behavioral breadth is out of scope.** "Does `sort=alphabetical` work" (17 of `community_e2e_test.go`'s 20 subtests) is a T1 service/repo test. If a T2 contract fails, the answer to "which layer broke?" should be "the pipeline," not one of 20 endpoint behaviors. This is how `community_e2e_test.go` (1,820 LOC) and `user_journey_e2e_test.go` (1,001 LOC) get decomposed rather than moved.
4. **Two entry flavors, both required over time:** *client-path* (through the AppView's write endpoints, as the mobile app does) and *federation-path* (record written directly to a PDS the AppView doesn't front, arriving purely via firehose — this is what makes us honest about being an atProto AppView and not a monolith). Start with client-path; add federation-path contracts as Phase 5.
5. **One cross-domain saga survives**: `user_journey` (signup → community → post → comment → vote → timeline), rebuilt on testkit, as the single "does the whole product hold together" smoke test. Everything else is per-record-type.

Contract inventory (target: one file each, ~150–300 LOC, in `tests/e2e/`):
`user`, `community`, `post`, `comment`, `vote`, `subscription`, `userblock`, `community_avatar`/`profile_avatar` (blob path), `aggregator`, plus `journey`. That's ~10 files replacing 15 misnamed `*_e2e_test.go` files and 48 scattered `Test*E2E*` funcs.

### 3.5 Command surface

```
make test              # T0. No Docker. The inner loop. <60s.
make test-integration  # T1. Starts postgres-test if needed. Parallel, no -p 1.
make test-e2e          # T2. Requires the ci stack (or dev stack) up; fails fast if not.
make test-live         # T3. Opt-in, hits the internet.
make ci                # THE GATE: hermetic stack, T0+T1+T2, ci-report skip inversion.
```

- `make ci` stays exactly what it is today (build from tree → staged compose up → bootstrap → run → `ci-report`), except the runner invokes tiers by tag instead of one giant `-p 1` sweep: T0+T1 with `-tags integration` in parallel, then T2 with `-tags e2e` against the composed AppView. Expected effect: wall-clock drops even as coverage rises, because 200+ integration tests stop being serialized.
- `make test-all` is deleted. It is a slower `make ci` with weaker guarantees (any-one-service-up counts as "infra ready", skips count as passes, `./internal/...` runs without `-p 1` — a live race today). One gate, not two.
- Silent-failure fixes ride along: `goose up || true` in `make test` loses the `|| true`; `sleep 3` becomes `pg_isready` polling; dead targets (`create-test-account`, `verify-stack` — scripts that don't exist) are removed.

### 3.6 Enforcement (so it doesn't rot back)

The suite got here by drift, so the invariants get mechanical guards, all inside `make ci`:

1. `ci-report` (exists): skip ⇒ failure unless allowlisted-with-reason; stale allowlist entries fail.
2. Lint gate (new, trivial grep/vet step in `ci-runner.sh`): fail on `time.Sleep(` in `*_test.go`, `t.Skip` outside `testkit`, `testing.Short()` anywhere, `websocket.DefaultDialer` outside `testkit/firehose.go`, and hardcoded `localhost:` ports outside `testkit` (all endpoints come from env with defaults defined once in testkit).
3. `go vet ./...` with each tag set — tagged files that don't compile are otherwise invisible.

### 3.7 Network isolation: never the public network

Three layers, from convention to physics:

1. **All endpoints come from testkit, all defaults point at the Docker stack.** `testkit.Endpoints()` reads `PDS_URL`, `PLC_DIRECTORY_URL`, `JETSTREAM_URL`, `APPVIEW_URL`, `POSTGRES_TEST_*` once, defaulting to the compose-stack addresses. No other test code constructs a base URL. (This also retires the 3 leftover `localhost:2583` references — a stale upstream-PDS default from a different topology.)
2. **Lint gate**: any occurrence of public atProto hostnames (`plc.directory`, `bsky.network`, `bsky.social`, `bsky.app`, `public.api.bsky.app`, …) in a `*_test.go` or testkit file outside `tests/live/` fails CI. Today `bluesky_post_test.go` (33 skips) and `post_unfurl_test.go`'s third-party targets violate this — they move to T3 in Phase 2.
3. **Egress-blocked CI network (strongest)**: the hermetic stack in `docker-compose.ci.yml` gets `internal: true` networking (plus whatever the PLC/PDS containers need internally), so an accidental call to the public network fails loudly at connect time instead of silently succeeding against real infrastructure. Added in Phase 0 while getting the gate green. The `~`-conditional third-party unfurl tests currently tolerated by the allowlist move to T3, which is the only tier that runs with egress.

The env-gated real-handle tests (`TEST_REAL_HANDLES=1`) hit the real PLC by design — they become T3 `live` tests, where that is explicit and opt-in rather than an env var buried in a conditional.

## 4. Migration plan

Each phase lands as its own commit(s) on `worktree-test-refactor`, and **`make ci` must be green at every phase boundary**. Phases are sized to be a focused agent session each.

- **Phase 0 — Make the gate real.** *(~100–300 LOC touched, net ~0)* `make ci` green in this worktree (baseline commit `aa57ccc` imported the harness incl. `docker-compose.ci.yml`). Flip the stack's networks to egress-blocked (3.7). Record the run's timing + allowlist as the baseline. *Exit: green hermetic run with no public egress, timings captured.*
- **Phase 1 — testkit.** *(~2,500 LOC, net +2,500: ~1,500 harness + ~800 harness tests + lint gate)* Build `tests/testkit` (db/pds/firehose/wait/fixtures/appview — absorbing the 4 drifted `*PasswordAuthPDSClientFactory` adapters, both `createPDSAccount`s, and the hand-rolled XRPC clients into one `pds.go`), add the lint gate (initially warning-only), fix the 5 surviving handle-collision sites. No mass migration yet — testkit ships with its own unit tests. *Exit: testkit exists, tested, lint gate wired.*
- **Phase 2 — Kill the lies.** *(~1,800 LOC touched, net −800)* Delete the 6 never-run debt tests (allowlist section 4). Move the 2 rate-limiter "e2e" files to `internal/api/middleware` as T0. Move `tests/unit/community_service_test.go` content toward `internal/core/communities`. Move the public-network tests (`bluesky_post_test.go`, live-target unfurl tests, `TEST_REAL_HANDLES` tests) to `tests/live/` as T3. Delete `-short` guards; introduce build tags on existing files *in place* (tag ≠ move; moves come later). Retarget Makefile to tags. *Exit: tiers are mechanically selectable; `make test` is honest; no public-network code outside `tests/live/`.*
- **Phase 3 — DB isolation.** *(~4,000 LOC touched, net −1,800)* Migrate `setupTestDB`'s 222 call sites to `testkit.DB(t)` (mechanical, high-volume — good fan-out work), delete all unscoped `DELETE FROM`s and per-file cleanups, enable `t.Parallel()`, drop `-p 1`. *Exit: suite passes with parallelism on; wall-clock recorded vs Phase 0.*
- **Phase 4 — Pipeline contracts.** *(~20,000+ LOC touched, net −8,000 to −12,000 — the only multi-session phase; split per domain: post, comment, community, vote, user, blobs, …)* Build the ~10 T2 contracts on testkit + the running AppView, decomposing the god-files: each old saga's endpoint-behavior subtests move down to T1 next to their service/repo — rewritten onto testkit, not copy-pasted; pipeline steps become the contract. Delete the 10 `subscribeToJetstream*` copies and the 22×500ms sleeps as their callers migrate. `tests/integration/` shrinks toward empty and is removed. *Exit: `tests/e2e` = contracts + journey; `tests/integration` gone.*
- **Phase 5 — Coverage debt + federation path.** *(~8,000 LOC, net +7,000, additive)* Unit tests for the zero-coverage core packages (communities, votes, identity, routes, timeline, discover, communityFeeds — in that order); repo tests for the 13 untested repos; federation-path variants of the highest-value contracts (post, comment, vote — cf. the known vote-federation gap). *Exit: no zero-test package in `internal/core`; federation contracts for the big three.*
- **Phase 6 — Docs.** *(~600 LOC touched, net −400)* This doc moves from PROPOSED to CANONICAL; `TESTING_SUMMARY.md` + `docs/E2E_TESTING.md` deleted; CLAUDE.md testing section points here; lint gate flips from warn to fail.

Aggregate expectation: the suite lands around **~78k LOC (from ~84k) with materially more coverage** — duplication currently dwarfs the gaps, so the refactor is net-negative even while filling seven zero-coverage packages.

Definition of done, measurable: 0 `time.Sleep` in tests · 0 `t.Skip` outside testkit · 0 `testing.Short()` · allowlist ≤ ~5 entries, all `~`-conditional or opt-in-internet · 0 packages in `internal/core` without tests · `-p 1` gone · `make test` < 60 s · `make ci` green and ≤ Phase-0 wall-clock despite added coverage.

## 5. Decisions & rationale (short form)

- **Build tags over directories-as-tiers**: the current tree proves naming conventions don't survive contact with velocity. Tags are compile-time, greppable, and un-forgettable (you can't run a tier by accident).
- **T1 colocated with code, not in `tests/`**: repo tests next to repos get maintained when the repo changes; a 41k-LOC `tests/integration` catch-all demonstrably doesn't. `tests/` keeps only what is genuinely cross-cutting: e2e contracts, live tests, testkit, ci config.
- **Template-clone-per-test over shared-DB-with-cleanup**: cleanup-discipline is exactly what failed (331 DELETE statements). Isolation by construction is the only version that survives agents writing tests at speed.
- **Black-box T2 over test-instantiated consumers**: tests the wiring that actually ships, deletes the largest duplication cluster, and makes the E2E tier readable as product documentation.
- **Skips-as-failures stays and expands**: `cmd/ci-report` is the best idea already in the tree. The refactor makes the allowlist *shrink* to near-zero rather than institutionalizing it.
