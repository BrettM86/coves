# Coves test-refactor build-loop state

Spec: docs/TEST_ARCHITECTURE.md (revision 2, post-Codex-review). Branch:
`worktree-test-refactor`, worktree `/Users/bretton/Code/coves/.claude/worktrees/test-refactor`.
ALL work happens in that worktree — every worker agent must cd there first.

## Loop protocol (one iteration per wakeup)

1. **Analyze** (Fable, parent — persistent context): read this file, pick the
   first non-done task, write the worker brief (scope, files, spec sections,
   relevant cross-iteration notes, exit criteria).
2. **Work** (Opus 5 agent, foreground, `model: opus`): implement in the
   worktree. Returns a summary + file list + anything surprising. NEVER a
   full-diff dump into parent context.
3. **Review** — scaled to the iteration class (column R below):
   - `S` (substantive): Codex `gpt-5.6-sol` via `codex exec --sandbox read-only`
     on the iteration diff (per-run mktemp dir, last `===BEGIN_JSON===` pair)
     + one Opus general-purpose reviewer with a focused brief. Parent
     arbitrates findings; worker (or parent) applies accepted fixes.
   - `M` (mechanical): no model review. Gates only.
   - `F` (full panel): /second-opinion style — Claude specialty briefs +
     Codex, plus pragma:security when credentials/auth surface changed.
     Fires ONLY at end of Phase 1 and end of Phase 4.
4. **Verify** (gates, non-negotiable): `go build ./... && go vet ./...` with
   every tag set the iteration touched + the scoped test run for those tags.
   Full `make ci` at phase boundaries (tasks marked ⛩) and after any
   iteration that edits the CI harness itself.
5. **Commit** to the worktree — only on green. Red = fix now or mark
   `blocked: <reason>` and stop the loop for user input.
6. **Update this file** (status, commit hash, notes — surprises go to
   Cross-iteration notes) and ScheduleWakeup the next iteration (~60s).

Statuses: pending → in-progress → review → done (or blocked: <reason>).
Stop the loop when every task is done, or on any blocked task.

## Task table

| # | Task | Phase | R | Status | Commit | Notes |
|---|------|-------|---|--------|--------|-------|
| 1 | Gate green as imported: run `make ci`, fix what surfaces, record baseline timing + allowlist | 0 ⛩ | S | done | (no diff) | GREEN FIRST RUN: 3307 tests, 3288 pass, 0 fail, 19 skips (all allowlisted, 21 entries → 2 unused are ~conditional), 2.2 min WARM caches. No fixes → no review stream |
| 2 | Move public-network tests to tests/live/ (+`live` tag, move-only); flip compose nets `internal: true` + module-cache pre-pull; cold-cache egress-blocked `make ci` green | 0 ⛩ | S | done | (see git log) | COLD egress-blocked GREEN 2:21; warm 1:57. 16 funcs → tests/live (4 files + helpers). Egress block found 4 runtime deps, not 1: Turnstile URL hardcoded (→ WithSiteverifyURL, dev-gated env), PLC healthcheck redirected to public web (→ /_health), 3 blueskypost tests dialed public.api.bsky.app (→ blueskyAPI seam), DNS-dependent 404 test (→ DID). Review: Codex "good" + Opus "safe as-is"; 9 fixes applied (unfurl E2E de-mocked via httptest OG, gate vets -tags live, non-dev override warns, prod default pinned, stub 127.0.0.1-only, GOPROXY=off, 404/500 handler tests, golden 200 parse, live cache purge+method asserts). 3281 tests / 14 allowlisted skips |
| 3 | testkit core: db.go (template-clone, advisory lock), wait.go (WaitFor/Holds, terminal errs), fixtures.go (UniqueID run-prefix), scripts/test-db-prepare.sh, scripts/test-audit.sh (warn mode) + testkit's own tests | 1 | S | done | (see git log) | 47 testkit tests, -race -shuffle clean; clone ~30ms (cheaper than spec est). Review: Codex needs-work (5 high) + Opus not-safe-as-is (2 high) → 15-item batch applied: template-drop name rail (found a panic-in-error-path bug), drop-before-close teardown, 55006 retry, sweep de-FORCEd + error-accumulating, wait-primitive deadline contract, max_connections=200 + ParallelBudget (CI: -parallel 53, inert until ph.3), bounded lock wait w/ holder diagnostics, grep -H audit fix. ADJUDICATION: Codex RIGHT / Opus WRONG on advisory-lock leak (async cancel can grant lock after Go sees error; fixed via Conn.Raw(ErrBadConn) eviction). make ci GREEN 3347 tests / 14 allowlisted skips |
| 4 | testkit pds.go (absorb 4 factories, createPDSAccount×2, XRPC clients), firehose.go (generic cursor-gated), appview.go; fix 5 handle-collision sites | 1 ⛩ | S | pending | | then FULL PANEL review of tests/testkit (incl. pragma:security — PDS creds) |
| 5 | Kill the lies: delete 6 debt tests; lexicon validator stops generating defs-only subtests (retire 8 allowlist entries); move 2 ratelimit files to internal/api/middleware (T0); fold tests/unit into internal/core/communities | 2 | M | pending | | |
| 6 | Split multi-tier files by test func (manifest in commit msg); add build tags in place; retarget Makefile to tags; delete -short/testing.Short(); delete test-all | 2 ⛩ | S | pending | | identity_resolution, bluesky_post (what's left post-task-2), post_unfurl |
| 7 | Migrate setupTestDB call sites → testkit.DB(t), batch 1 (~25 files) + delete their DELETE FROMs/cleanups | 3 | M | pending | | mechanical; gates are the reviewer |
| 8 | Migrate remaining call sites; delete all 3 setupTestDB defs + per-file cleanup fns | 3 | M | pending | | |
| 9 | Global-state audit (t.Setenv/os.Setenv/logger/http-default → testkit injection); enable t.Parallel on proven-safe; connection budgets; `-race` clean; drop -p 1 | 3 ⛩ | S | pending | | wall-clock vs task-1 baseline recorded here |
| 10 | Contract-manifest CI check (WantedCollections ↔ //coves:ingestion-contract markers) + T2 skeleton (serial runner via compose runner; make test-e2e; test-e2e-dev escape hatch) | 4 ⛩ | S | pending | | build BEFORE first contract so every contract lands against it |
| 11 | Contracts: community (community.profile ingestion + API) — strangler: behavior inventory of community_e2e_test.go (1820 LOC) → down-tier T1s → contract → delete old | 4 | S | pending | | template for tasks 12-16; sync-indexing trap per spec §3.4 |
| 12 | Contracts: post (community.post) + post_delete + decompose post god-files | 4 | S | pending | | |
| 13 | Contracts: comment (community.comment) + comment god-files (1821+1443+1229+999 LOC) | 4 | S | pending | | biggest decomposition |
| 14 | Contracts: vote (feed.vote) + user (actor.profile incl. avatar blob path) + subscription (community.subscription) | 4 | S | pending | | vote re-tap idempotency invariant |
| 15 | Contracts: blocks (actor.block + community.block) + aggregators (aggregator.service + aggregator.authorization) | 4 | S | pending | | collections revision-1 inventory MISSED — see spec §3.4a |
| 16 | Reliability suite (§3.4c: cursor resume, replay-once, rev-gate no-resurrection via Holds, dead-letter, 2-feed overlap) + user_journey rebuild + rm -rf tests/integration | 4 ⛩ | F | pending | | FULL PANEL on the whole Phase-4 output |
| 17 | Unit-coverage debt A: communities, votes, identity (repo tests for their repos too) | 5 | S | pending | | behavior matrices at T0, repo seams at T1 |
| 18 | Unit-coverage debt B: routes, timeline, discover, communityFeeds + remaining untested repos | 5 | S | pending | | |
| 19 | Federation topology: 2nd PDS + hermetic relay in compose; promote post/comment/vote contracts to true federation-path | 5 ⛩ | S | pending | | vote-federation gap is the known prize |
| 20 | Enforcement flip: audit counts → 0, warn → fail; docs (spec → CANONICAL, delete TESTING_SUMMARY.md + docs/E2E_TESTING.md, CLAUDE.md pointer) | 6 ⛩ | M | pending | | |

## Cross-iteration notes for future iterations
(parent + workers append surprises, interface changes, deferred TODOs)

- **Worktree discipline**: subagents run FOREGROUND (CLAUDE.md rule). Workers
  cd to the worktree; parent never reads full diffs — summaries only. This
  file is the durable memory across context compaction: if you (parent) don't
  remember something, it belongs here or in the spec — read those, not git.
- **Stack facts**: test Postgres :5434, dev :5435, PDS :3001, PLC :3002,
  Jetstream :6008 (metrics :6009, healthcheck disabled — no HTTP client in
  image), AppView :8081. Hermetic stack shares a netns (`netns` service);
  publishes no host ports; `.env.ci` is the runner env. `make ci` discards
  `go test`'s exit code — `cmd/ci-report` (skip-inversion) is the verdict.
- **Known traps** (from memory + survey): PDS handle local-label cap is 18
  chars AND PDS accounts persist across runs — only testkit.UniqueID for
  handles. gorilla/websocket panics after ~1000 reads on a dead conn — the
  maxConsecutiveTimeouts guard must survive into testkit/firehose.go.
  Subscribe-cursor-before-write is the only legit anti-race (spec §3.3).
  Jetstream "cursor=now on a quiet stream replays entire store" (tidepool
  task-10 finding) — bound negative assertions by an observed event's
  time_us, never by cursor=now.
- **Sync-indexing domains** (spec §3.4 core insight): signup + community
  create index synchronously — their API contracts prove the client surface,
  NEVER the pipeline; only direct-PDS ingestion contracts prove the pipeline.
- **ci-bootstrap.sh** exists because the instance account on a kept PDS
  volume was created by hand years ago; fresh stacks create it via
  createAccount and prove createSession. Don't "simplify" it away.
- **Baseline (task 1, 2026-07-29)**: make ci wall-clock 2.2 min (WARM
  caches/images); 3307 tests / 3288 pass / 19 allowlisted skips; allowlist
  21 entries. **Post-task-2**: COLD egress-blocked 2:21, warm 1:57; 3281
  tests / 14 allowlisted skips; allowlist 14 entries (8 lexicon defs-only +
  6 debt — task 5 kills the 6, lexicon fix kills the 8).
- **From task 2**: egress block is a DETECTOR — expect it to keep surfacing
  hidden network deps in later phases (found 4 on day one; no grep catches a
  healthcheck redirect or SERVFAIL-vs-NXDOMAIN). Egress failures are FAST
  (DNS ~0s), so fallback-tolerant tests silently take their fallback path,
  never hang/skip. Turnstile: stub at 127.0.0.1:3003, env honored only when
  IS_DEV_ENV=true, non-dev override logs a warning. blueskypost has an
  unexported test seam blueskyAPI{baseURL,allowPrivateHost}; prod default
  pinned by TestService_DefaultAPITarget — keep it pinned. GOPROXY=off in
  the runner SERVICE env only (prefetch runs same image outside the stack
  and needs the proxy). tests/live still carries testing.Short() guards +
  network-tolerant skips (_CacheHit/_KagiKite) — task 6 sweeps them; an
  oEmbed-endpoint-map unit test in internal/core/unfurl is a small task-6
  add-on (E2E now goes through the OpenGraph path instead). ci-runner has
  stage 1b `go vet -tags live` so the live tier can't rot invisibly.
- **Docker flake**: containerd "failed to prepare extraction snapshot" can
  appear once on build; retry clears it — don't chase it as a regression.
- **From task 3 (testkit — later phases MUST know)**: testkit.DB(t) is the
  ONLY sanctioned DB path for migrated tests; template `coves_test_template`
  guarded by validateTemplateName (won't drop non-test-shaped names).
  `sql.DB.Close` WAITS on in-flight queries — always DROP (bounded ctx,
  FORCE) before Close in teardown. `Conn.Raw(func(any) error { return
  driver.ErrBadConn })` is the ONLY way to evict a session from a
  database/sql pool — required on every error path of anything holding
  session state (advisory locks, SET LOCAL, temp tables). CREATE DATABASE
  ... TEMPLATE can hit SQLSTATE 55006 for a few ms after a template
  connection closes (async backend exit) — cloneTemplate retries 3×.
  Advisory lock key ('COVE',1) on the maintenance DB (coves_test); template
  peeks need EXCLUSIVE (a source connection blocks cloning). goose: testkit
  uses NewProvider (no globals); hazard only if a package mixes legacy
  goose.Up with testkit in one binary (tasks 7-8 watch for it).
  testkit.ParallelBudget wired into both go test invocations via
  test-db-prepare.sh --print-parallel (CI computes 53); inert until phase 3
  drops -p 1. Dev postgres-test won't see max_connections=200 until
  recreated FROM THE MAIN CHECKOUT (worktree compose project-name mismatch
  vs pinned container_name); CI unaffected. Audit baseline 911: sleep 60
  (ph.3-4), skip 280 (ph.2), Short 162 (ph.2), dialer 30 (ph.4), host:port
  288 (ph.3-4), public-hosts 91 (ph.2, ~91 exemption annotations needed —
  big three: blueskypost/url_parser_test 19, aggregator_registration_test
  14, jetstream/user_consumer_test 10). Testkit tests deliberately untagged
  until task 6. REVIEWER CALIBRATION: on the lock-leak disagreement Codex
  was right, Opus wrong (missed the failure path, traced only cancellation)
  — weight Codex on DB/concurrency semantics.
