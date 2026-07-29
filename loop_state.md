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
| 1 | Gate green as imported: run `make ci`, fix what surfaces, record baseline timing + allowlist | 0 ⛩ | S | in-progress | | unknown failures likely; may split |
| 2 | Move public-network tests to tests/live/ (+`live` tag, move-only); flip compose nets `internal: true` + module-cache pre-pull; cold-cache egress-blocked `make ci` green | 0 ⛩ | S | pending | | order per spec §3.7/§4 |
| 3 | testkit core: db.go (template-clone, advisory lock), wait.go (WaitFor/Holds, terminal errs), fixtures.go (UniqueID run-prefix), scripts/test-db-prepare.sh, scripts/test-audit.sh (warn mode) + testkit's own tests | 1 | S | pending | | dependency rule: testkit imports NO internal/core/* |
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
- **Baseline (task 1 fills in)**: make ci wall-clock: ___; tests: ___;
  allowlist entries: 21.
