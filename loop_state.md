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
| 4 | testkit pds.go (absorb 4 factories, createPDSAccount×2, XRPC clients), firehose.go (generic cursor-gated), appview.go; fix 5 handle-collision sites | 1 ⛩ | S | done | (see git log) | PHASE 1 COMPLETE. Worker found ALL 10 legacy subscribeToJetstream copies broken (gorilla corrupt-after-deadline → every "30s wait" gave up at ~5s — explains historical flakes). 5 factories not 4 (comments missed by helpers.go); old generateTID never emitted valid TIDs (now wraps indigo TIDClock); dep rule is TRANSITIVE (atproto/pds imports core/blobs → reimplemented). FULL PANEL (Codex+CR+SFH+TA+security): security CLEAN; ~28-item batch applied — same-time_us dedupe set, deadline-bounded dials, all-read-errors-recover, discard counting (clock-skew diagnosis), overflow=failure, lock-free blocking I/O, Event.Raw()/Into() (unblocks consumer migrations), XRPC-shaped 404 classification, PendingIfUnavailable, ConsumerHealth + WithConsumerHealth, option-pattern unification, testkit.Main(m, Require*...). CR false-positive on ParallelBudget wiring (discarded). testkit 117 tests -race -shuffle green; make ci GREEN 3429/14 |
| 5 | Kill the lies: delete 6 debt tests; lexicon validator stops generating defs-only subtests (retire 8 allowlist entries); move 2 ratelimit files to internal/api/middleware (T0); fold tests/unit into internal/core/communities | 2 | M | done | (see git log) | ALLOWLIST → 0 ENTRIES; make ci GREEN 3399/3399, 0 skips. Lexicon fix is a coverage GAIN (43 defs-only fragment resolutions previously asserted nothing + two-way naming consistency). tests/unit was 100% FAKE (servers never dialed, literals asserted against themselves, t.Log theater) — deleted wholesale, nothing to port; communities now at honest zero (task 17). 3 tautology "tests" deleted rather than moved. Audit 911→897 |
| 6 | Split multi-tier files by test func (manifest in commit msg); add build tags in place; retarget Makefile to tags; delete -short/testing.Short(); delete test-all | 2 ⛩ | S | done | (see git log) | PHASE 2 COMPLETE. 76 files `integration`, 3 `e2e`; 2 jetstream files split; 161 Short guards deleted (162nd was a doc comment); test-all + 4 dead targets gone. Honesty test: untagged suite green under --network none FIRST TRY (36 pkgs). make test = 11s no-Docker. Review (Codex needs-work / Opus safe-as-is): 8 fixes — GATE INTEGRITY closed (exit codes captured + mismatch rule; OOM-137-with-green-report now fails — was a silent pass since the harness was born; proved via truth table), -parallel 1 pinned on e2e (serial T2), readiness probe now hits the HOST endpoint tests dial, shared-DB migrate restored via testkit.MigrateSharedDatabase (advisory-locked, in testdbprepare), DSN redacted via url.Redacted, pure testkit files untagged (TestMain split into tagged harness_test.go + untagged harness_support_test.go), T0 socket-free (failingTransport). make ci GREEN 3399/0 skips 2m4s; audit 573 |
| 7 | Migrate setupTestDB call sites → testkit.DB(t), batch 1 (~25 files) + delete their DELETE FROMs/cleanups | 3 | M | done | (see git log) | 29 files (aggregator_e2e..concurrent_scenarios incl. 4 hand-rolled setup clones), 115 sites → testkit.DB(t) (2 needed NO db at all — migration doubles as unused-DB detector), 19 DELETE FROMs + 1 cleanup fn deleted, diff +142/−828. Isolation PROVEN: concurrent -count=2 on a hardcoded-PK pair green; 0 leaked clones. make ci GREEN 3399/0 @142s (+18s vs baseline: ~150ms/test = FORCE-drop + 2 lock RTs — task 9 pays it back). Audit 573→561. NO order-dependency failures surfaced |
| 8 | Migrate remaining call sites; delete all 3 setupTestDB defs + per-file cleanup fns | 3 | M | done | (see git log) | DB MIGRATION COMPLETE: 128 sites (31 files incl. live+e2e), all 4 defs + 4 cleanup fns + 18 goose pairs + 63 wipes deleted, +189/−1182. grep setupTestDB|goose in tests/ = EMPTY. e2e shared-DB hazard was HYPOTHETICAL (user_signup setupTestDB had ZERO callers; error_recovery all in-process) — SharedDB not needed. TestMain → testkit.Main(RequirePostgres, RequirePDS, RequireJetstream): make test-integration now FAILS without dev stack instead of skip-green (spec-honest, kept). FULL -shuffle=on INTEGRATION RUN GREEN — wipes were dead weight. make ci GREEN 3399/0 @2:39 (+17s ≈ 133ms/clone, consistent) |
| 9 | Global-state audit (t.Setenv/os.Setenv/logger/http-default → testkit injection); enable t.Parallel on proven-safe; connection budgets; `-race` clean; drop -p 1 | 3 ⛩ | S | done | (see git log) | PHASE 3 COMPLETE. 343 t.Parallel; audit: 0 convert / 4 sites deliberately-serial / rest safe. 9 internal straggler files migrated (goose now EXTINCT in test code; MigrateSharedDatabase deleted). THREE concurrency bugs -p 1 was masking: [A] template-destruction race (fixed: usePrivateTemplate) [B] legacy firehose 5s-behind-30s-promise, quantified (patched: jetstreamReadBudget, counter machinery deleted, non-timeout errors terminate) [C] Jetstream account/identity events BYPASS wantedCollections → parallel signup storms starve subscribers (measured 2/4 fail at -p 2; -p STAYS 1 with new documented reason). ConcurrencyBudget models both dims + nestedClonePools; -p 1 -parallel 26. Review: Codex good + Opus 3-high (binary-abort class, all fixed incl. fail-open Makefile splice PROVEN closed). make ci GREEN ×2 117/128s (clone tax repaid, beats 124s pre-clone); -race + -shuffle clean; peak 27/200 conns; 3401 tests/0 skips; audit 532 |
| 10 | Contract-manifest CI check (WantedCollections ↔ //coves:ingestion-contract markers) + T2 skeleton (serial runner via compose runner; make test-e2e; test-e2e-dev escape hatch) | 4 ⛩ | S | done | (see git log) | THE PIPELINE WORKS: TestPipelineSmoke green in hermetic stack (direct PDS write → Jetstream → container consumers → getProfile, 0.95s de-raced). cmd/contract-manifest (38 tests, MatchFile-based, pending_contracts.txt ratchet w/ task ownership, AST forbidden-imports in marker files); T2 skeleton (newPipeline, contractBudget=45s, per-contract synthetic IPv6 vs the ONE-BUCKET rate limiter — A/B proven 60+40=100); make test-e2e via compose runner 48s cold (lib/ci-stack.sh + runner-ready.sh factored); zero-skip T2 enforcement. Census: 10 collections = task mapping exact. Review: Codex needs-work + Opus 3-high → 8 fixes (manifest bypasses had live probes; smoke de-raced vs profile-backfill reconciliation path). make ci GREEN ×2 ~2:00, 3448/0 |
| 11 | Contracts: community (community.profile ingestion + API) — strangler: behavior inventory of community_e2e_test.go (1820 LOC) → down-tier T1s → contract → delete old | 4 | S | done | (see git log) | TEMPLATE PROVEN. 22-behavior inventory; 2,168 LOC deleted, +14 net tests; 2/9 serial firehose files gone. Ingestion contract SELF-REGISTERS the community's PDS repo (stronger than arming — no sync write exists; consumer has NO must-know-first gate, verified). FOUND+FILED prod defect: unverifiable handles → handle.invalid UNIQUE squat → federated communities silently dropped; second symptom pds_url permanently empty → BridgeTrust denies bridged votes (issue extended). STANDING TIER LIMIT (spec §3.4b amended): sealed sessions mint only in browser OAuth — T2 covers auth boundary + reads; authenticated writes proven at T1; test-only mint = phase-5 pre-work. Review: Codex 1 high (update-handler boundary died — restored w/ 8 tests) + Opus audit 17/20 equal-or-stronger, 3 gaps all closed. make ci GREEN ×2 3496/0 @2:12 |
| 12 | Contracts: post (community.post) + post_delete + decompose post god-files | 4 | S | done | (see git log) | post_e2e (662) + post_delete (841) deleted, net −1402 w/ more coverage; serial firehose files 7→5. TWO MORE PROD DEFECTS FILED (tally 4): p1 deleted posts served in full by getComments to anon callers forever (repo missing deleted_at filter); p3 DeletePost idempotency branch unreachable (PDS 400 ≠ ErrNotFound — pinned w/ loud require.Errorf). Spoof negative REWRITTEN intra-repo after Opus traced indigo parallel scheduler (cross-repo FIFO doesn't exist; 5s hold was the real bound; relay would break silently) — mutation-tested. Consumer gates pinned: author-not-found=transient+replay-accepted. Journey flake (2/6 gates, note-[C] starvation) mitigated: 60s/75s ordered waits + starvation-vs-dead tally; real fix task 16. make ci GREEN 3496/0 @2:21 |
| 13 | Contracts: comment (community.comment) + comment god-files (1821+1443+1229+999 LOC) | 4 | S | done | (see git log) | comment_e2e (1120) deleted; 4,263 LOC of T1-shaped satellites KEPT deliberately (right call — §3.4 rule 3 breadth). JOURNEY PULLED FORWARD from task 16: worker bisected its own deletion breaking the journey deterministically (serial tests were accidental SPACERS vs the note-[C] storm) → rebuilt as the §3.4 rule-5 saga in tests/e2e (3 actors, 3 repos, 4 read paths; old file was 2-real-of-11-steps, SQL-insert fallbacks, fakecid). Serial firehose files 5→3; ONE subscriber helper left tree-wide (vote_e2e — task 14 kills it). Comment consumer has NO must-exist gates (measured); comments live in AUTHOR repo; delete = placeholder. getComments capped 20/min → withReadCadence(2.5s) + FreshReadQuota + Holds@1s. Defect #5 filed (malformed URI → 500). Reviews: kill-list EMPTY. make ci GREEN ×2 3532/0 @~2:45; e2e 56/0/0; audit 473 |
| 14 | Contracts: vote (feed.vote) + user (actor.profile incl. avatar blob path) + subscription (community.subscription) | 4 | S | review | | THE LAST HAND-ROLLED SUBSCRIBER IS DEAD (tree-wide dialer count = 1, and it is production connector.go). 5 files / 3,908 LOC deleted; 3 contracts + community-avatar step added; 5 T1/T0 files gained. TWO DEFECTS FILED (tally 7), both spike-confirmed: #6 vote-before-subject is lost AND its later delete SUBTRACTS a real vote (unbounded downward drift, floored at 0); #7 same-rkey putRecord vote flip silently ignored (consumer handles create/delete only) — third-party/federated clients only, our own client does delete+create. Both PINNED, not asserted-as-intended. make ci GREEN x2 3547/0 @3:55; e2e 68/0/0 x2 kept-stack; audit 473->403. REVIEW BATCH (6 items) applied: item-6 verdict CODEX RIGHT (fake's two slices could not express ordering and the comment claimed they did — unified op log, mutation-proven); vacuous Hold dropped, p1 pin hardened with a second Holds + issue names in both pins; GetBinary added to testkit (Get accepts any 2xx and discards the body — a 204 passed the must-200 claim); community banner + nil->value transition + blob write-forward test, which FOUND the create/update indexing asymmetry. Post-review: make ci GREEN 3549/0 @4:06; e2e 68/0/0 |
| 15 | Contracts: blocks (actor.block + community.block) + aggregators (aggregator.service + aggregator.authorization) | 4 | S | done | (see git log) | ALL 10 COLLECTIONS CONTRACTED — pending_contracts.txt EMPTY (task 16 flips -allow-pending=false; task 20 verifies). 10 files deleted (5,848 LOC), ~4,575 re-homed. Blocks have NO anonymous observable (all enforcement viewer-scoped) → consumer-health MEASUREMENT WINDOWS (snapshot counters → block → same-repo visible bound → re-read; malformed → dead-letter delta==1; mutation-tested both ways; redriver PROVEN inert at code level — never touches connector counters). TWO MORE DEFECTS (tally 9): p2 community.block indexed-but-NEVER-ENFORCED (no query joins it, no route lists it — decorative, but federates); p3 aggregator quota write best-effort = free posts. Enforcement is ONE-directional viewer-side (product question for Bretton, not filed). Reviews: kill-list 6 small (worst MED-LOW restored: aggregator HTTP path w/ marker-header mutation-check; register→users row — found a T0 fixture that could NEVER pass the real service). make ci GREEN 3544/0 @4:08; e2e 78/0/0 @161s; audit 380 |
| 16 | Reliability suite (§3.4c: cursor resume, replay-once, rev-gate no-resurrection via Holds, dead-letter+REDRIVE recovery, 2-feed overlap) + relocate/dissolve tests/integration (38 files) + flip -allow-pending=false | 4 ⛩ | F | in-progress | | journey DONE in task 13; FULL PANEL on the whole Phase-4 output |
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
- **From task 4 (testkit complete — MIGRATION CONTRACTS for tasks 7-8+)**:
  canonical package setup is `testkit.Main(m, testkit.RequirePostgres,
  testkit.RequirePDS, ...)` (opt-in probes; see testkit.go Layout doc).
  Consumer-feeding tests use `ev.Into(&jetstreamEvent)` — never reconstruct
  wire structs by hand. Options are component-qualified: WithAppViewBearer,
  WithAppViewURL, WithFirehoseCursor. DeleteRecord is idempotent-silent on
  the PDS (200 + no commit for missing rkey) — pair Await with
  DeleteExistingRecord. `package testkit_test` (external test pkg) is the
  sanctioned way to pin testkit's duplicated types against internal pkgs it
  may not import (firehose_pin_test.go is the example). GORILLA TRAP
  (tree-wide): any websocket loop that `continue`s after a read-deadline
  timeout has a latent ~5s ceiling — the conn is CORRUPT after expiry and
  must be re-dialled; all 10 legacy subscribeToJetstream copies had this
  bug, so historical "Timeout: No Jetstream event received" failures were
  often this, not slow indexing. testkit firehose recovers on ALL read
  errors (only undecodable frames are terminal), counts cursor-predating
  discards (clock-skew diagnosis), and FAILS on pending-buffer overflow.
- **From task 5**: tests/unit autopsy — all four community_service tests
  were fake; task 17 intel: the deleted mockCommunityRepo (160-LOC complete
  Repository fake) is recoverable from git history, and
  NewCommunityServiceWithPDSFactory (service.go:106) is the injectable seam
  the fakes were groping at. middleware.NewRateLimiter has NO injectable
  clock (the task-3 note was about the votes limiter) — its 3 moved sleeps
  are task 9's; its cleanup goroutine needs t.Cleanup(rl.Stop) — the moved
  tests leaked ~20 before. Aggregator rate limiting is genuinely covered in
  aggregator_e2e_test.go Part 4 (the deleted tautologies never were).
  allowed_skips.txt is deliberately empty with the rationale in its header
  — new entries must argue for themselves.
- **From task 6 (MIGRATION RULE for tasks 7-8)**: a package's TestMain sets
  the floor for its whole test binary — mixed-tier packages keep the
  infra-gated TestMain in a TAGGED file and shared test support (fakes,
  helpers) in an untagged sibling; testkit is the worked example
  (harness_test.go tagged / harness_support_test.go untagged). Tag sets are
  additive: tagged halves may use untagged helpers, never the reverse.
  Fully-tagged directories are silently skipped by ./... (not an error).
  ci-runner now enforces exit/stream integrity: go test status >1 fails the
  gate regardless of ci-report, nonzero-status+ok-report = truncated
  capture = fail. T0 is socket-free (failingTransport pattern for
  transport-error paths; withPDSAdminClient test-only option in users).
  make test-integration's readiness gate is test-db-prepare's real
  connection to the published port — compose-up conflicts are tolerated by
  design because the connection probe is the decider.
- **From task 7 (for task 8 + tasks 10-16)**: BOUNDARY — task 8 takes
  discover_test.go onward + oauth_token_verification + user_journey_e2e +
  user_profile_avatar_e2e; 104 setupTestDB sites remain; cleanup fns still
  live (cleanupTestDB, cleanupUserBlockTestDB,
  cleanupUserBlockEnforcementTestData, CleanupOAuthTestData) + all 3
  setupTestDB defs; 8 files still goose.Up the shared DB (all task-8 set).
  Per-call setup helpers returning routers are safe only if no test holds
  TWO at once (each call = its own clone — check before migrating such
  helpers). community_repo_test.go ~:497 has a big commented-out Search
  test (dead code, delete some day, not mechanically). NO tests/integration
  file dials a running AppView (all in-process httptest routers) — the
  shared-DB-vs-clone desync hazard is confined to tests/e2e; tasks 10-16
  must keep it that way (T2 contracts observe via the REAL AppView's
  endpoints and never mix testkit.DB clones with container-side writes).
- **From task 8**: tests/ gets databases ONLY via testkit (grep-verified
  empty). make test-integration's contract CHANGED: fails loudly without
  the dev stack (Require floor) instead of skip-green — one-line revert
  possible (drop RequirePDS/RequireJetstream) if a Postgres-only local tier
  is ever wanted. Pre-existing dead code in oauth_helpers.go
  (CreateTestUserOnPDS, VerifySessionData, GenerateTestSealSecret — zero
  callers) left for tasks 10-16. purgeIdentityCache in tests/live KEPT:
  guards a within-run invariant (Method != cache), not a cross-run wipe.
  Cumulative clone cost at 3399 tests: ~35s over baseline (~133ms/clone) —
  task 9's parallelism must beat that. No AppView-written row is asserted
  from Go anywhere (clean T2 boundary for tasks 10-16).
- **From task 9 (THE SHARED-RESOURCE LADDER — governs phase 4+)**: DB
  (tasks 7-8) → template (fixed: private templates, sweepable
  tktmpl_test_ family) → JETSTREAM STREAM (unfixed: account/identity
  events bypass wantedCollections; signup storms visible to every
  subscriber). -p stays 1 for the STREAM, not the DB — flip
  packageParallelism (tests/testkit/db.go) only after phase 4 deletes
  tests/integration's 9 serial firehose files, then fix the GOMAXPROCS
  caveat (read in the PREPARE process, not the test process — documented
  at the read site). Budget rule: model the WORST case one test can
  create (nestedClonePools term); a test holding 3 clones would silently
  re-break the ceiling — cheap contract check = grep testkit.DB( counts
  per test func. Effective-vs-advertised timeouts are the tree smell:
  phase-4 contracts use ONE named budget constant. Serial firehose block
  = 25.8s of the 48.9s tier — phase 4's wall-clock prize. Dev Jetstream
  accumulates (29MB) and replays thousands of pre-cursor events —
  docker restart coves-dev-jetstream when firehose tests slow; testkit's
  discard counter is the diagnostic. Local postgres-test CAN take
  max_connections=200 from a worktree: docker-compose -p coves (project
  name, not checkout, was task-3's trap). TestOAuthSessionHandleSync_
  LiveJetstream is assertion-free (3 t.Logs) — phase 4 rebuilds or
  deletes. Reviewer calibration: Opus caught all 3 binary-abort highs
  this round (incl. fail-open Makefile splice); Codex strongest on
  budget arithmetic — both earning their seats.
- **From task 10 (CONTRACT RULES for tasks 11-15 — read before writing any
  contract)**: (1) identities enter the index ONLY via actor.signup (users
  consumer refuses never-seen DIDs by design) — pipeline proof is always
  the record written AFTER signup; (2) RECONCILIATION PATHS are
  sync-indexing's subtler sibling: profile backfill fetches from the PDS
  when the row is EMPTY — falsify a reconciliation's precondition (arm
  then prove with a SECOND write) before asserting; actor.profile's
  contract MUST assert a second update (pending_contracts.txt carries
  it); (3) rate limiter is ONE bucket per client IP across the shared
  netns and OUTLIVES runs on kept stacks — newPipeline assigns
  per-contract synthetic IPv6 (SyntheticClientIP), poll at
  contractPollInterval=250ms, 429s are rewritten to name the tier's own
  polling; (4) contracts must be RE-RUNNABLE on kept stacks (in-process
  limiter/cache state survives); (5) T2 zero-skip is enforced by
  e2e-runner; markers only count in files that build under -tags e2e
  (MatchFile) and may not import websocket/jetstream or call testkit.DB;
  (6) -count=N never exercises RunPrefix-keyed code (process-scoped) —
  assert structure, not substrings. FLAKE LEDGER:
  TestVoteE2E_JetstreamIndexing (legacy subscribeToJetstreamForVote copy)
  flaked 1/6 gate runs post-jetstreamReadBudget — known-flaky legacy;
  task 14 DELETES it with the vote contract, do not port or re-diagnose.
  COVES_CI_REBUILD=1 refreshes a kept stack's AppView (also resets
  limiter buckets).
- **From task 11 (TEMPLATE for tasks 12-15)**: copy community_contract_test.go's
  form. Reuse provisionCommunityRepo (package-scoped, tests/e2e) to hang
  posts/comments/votes on. SPIKE FIRST on a kept stack before writing any
  contract — the handle.invalid discovery came from a throwaway spike, not
  design. Records carrying their own handle skip PDS-host resolution
  (pds_url stays empty — known defect, don't re-file). hostedBy
  verification is OFF in CI (SKIP_DID_WEB_VERIFICATION) — contracts prove
  field transport, not verification; say so in doc comments. 401 matrices
  belong at T2 (only the running router shows a route that lost
  RequireAuth); authenticated writes at T1. Posts wrinkle (from tidepool
  cross-notes + survey): post consumer requires repo DID == record.community,
  community indexed BEFORE post, author user indexed BEFORE post — post
  records live in the COMMUNITY's repo, so the ingestion write uses the
  community's own session. API asymmetry noted for a future task: update
  silently overwrites client-supplied updatedByDid; create 400s on
  createdByDid — pinned in tests, unify someday. internal/core/communities
  tests are package communities_test (external, import cycle) — task 17
  must NOT add a second TestMain in package communities.
- **From task 12 (for tasks 13-15)**: endpoint NOT-FOUND SHAPES differ —
  community.get 404s; post.get answers 200 + notFound union member
  (PendingIfNotFound useless there; probe the field); actor feeds 200+empty
  for unknown DID. Spike the shape first. did:plc "nobody" fixtures must be
  REAL 24-char base32 literals (UniqueID doesn't emit base32; validator
  currently omits the length check — don't lean on it). NEGATIVE BOUNDS
  must be INTRA-REPO (Jetstream parallelizes across repos — write the bad
  record and the bounding good record into the SAME repo; cross-repo
  ordering is topology luck). Unresolvable-handle 404 branch unreachable
  under egress block (Phase-5 topology). Contracts leave one retired dead
  letter per run on kept stacks (task 16: know before asserting counts).
  internal/core/posts tests = package posts_test external (no second
  TestMain in package posts). FLAKE LEDGER: TestFullUserJourney_E2E
  starvation flake MITIGATED (60s/75s + tally); if it flakes again in
  13-15 → pull task 16 forward, do NOT raise constants again. git stash
  is unreliable in this worktree (index merge errors) — back up files
  before A/B tests. Serial firehose files remaining: 5 (comment_e2e,
  community_avatar_e2e, user_journey_e2e, user_profile_avatar_e2e,
  vote_e2e).
- **From task 13 (for tasks 14-15)**: getComments is capped 20/min per IP
  (only per-route cap) — use withReadCadence(2.5s) on getComments waits
  (costs ~2.5s per healthy wait; ramped interval is the future fix if the
  tier budget ever tightens), FreshReadQuota at phase boundaries (it
  REBUILDS the client — carries bearer now, but any future NewAppView
  option must be carried too). VOTE-CONSUMER HAZARD for task 14 (Opus
  audit): no must-exist gate AND no reconciliation — an out-of-order
  vote's count UPDATE matches 0 rows and only LOGS; vote-before-post
  across repos = count lost forever; production cross-repo ordering is
  not guaranteed → spike it, likely defect #6 to file. Task 15 inherits
  the orphaned blockedPost serving assertions (blocked:true,
  blockedBy:"author", nils). Subscription fan-out composition = task 14's
  contract; personalised getTimeline unreachable at T2 until Phase-5 mint
  (T1 coverage exists in timeline_test.go). voteCollection/voteRecord()
  helpers live in journey_test.go — reuse, don't redeclare. Deleting
  serial files re-packs the schedule — prefer deleting the fragile
  subscriber over re-spacing (proven). Saga leaves ~1 dead letter/run
  too (hijack) — task 16 dead-letter accounting.
- **From task 14 (for task 15-16)**: THE SUBSCRIBER ERA IS OVER — no test in
  the tree dials a websocket; `test-audit.sh`'s dialer count of 1 is
  production `connector.go` and will never reach 0 (task 20 must exempt it,
  not chase it). SPIKE FINDINGS worth not re-deriving: vote re-tap has THREE
  distinct paths keyed on three different things (same-rkey update = dropped
  by HandleEvent's switch; duplicate delivery = rev gate + ON CONFLICT(uri),
  reachable only from task 16's reliability suite; new-rkey re-tap = the
  (voter_did, subject_uri) stale-vote cleanup — this last one is what a real
  client produces, because votes.voteService always deletes and re-creates
  under a fresh TID). Deleting an already-superseded vote does NOT
  double-decrement. AVATAR OBSERVABLE: getProfile/community.get serve a
  HYDRATED URL, never the CID — `{proxy}/img/{preset}/plain/{did}/{cid}` with
  IMAGE_PROXY_ENABLED=true in .env.ci, the PDS' com.atproto.sync.getBlob URL
  without it. Assert `Contains(url, cid)`, which holds in both modes; pinning
  either shape makes the contract a test of one config value. A bogus CID on
  that path is a 502, so `AppView.Get(path, nil)` is a real end-to-end blob
  assertion. actor.profile DELETE clears the fields and keeps the row
  (getProfile still 200s) — the OPPOSITE of community.profile, which hard-
  deletes and 404s; the two look alike from outside and getting it backwards
  costs a debugging session. Profile updates are PARTIAL: an absent key means
  "leave alone", not "clear", so a record is not a snapshot. SUBSCRIPTIONS
  have two observables that drift apart on purpose — community.get's
  subscriberCount is a STORED COLUMN, actor.getProfile's stats.communityCount
  is a live COUNT(*); assert both, it is the cheapest detector for a missed
  increment. Duplicate subscribe under a NEW rkey re-points record_uri and
  does NOT increment (ON CONFLICT keys on (user_did, community_did), decided
  by `xmax = 0`). Subscription FAN-OUT is unreachable at T2: getTimeline is
  the only endpoint that joins posts to subscriptions and it is RequireAuth;
  getDiscover explicitly does not filter, communityFeed filters by community,
  community.list?subscribed=true 401s. Documented in the contract, T1 covers
  it (timeline_test.go), Phase-5 mint unlocks it. DEAD LETTERS for task 16's
  accounting: an unknown-community subscribe and an invalid-direction vote
  each leave ONE retired dead letter (permanent → redrive budget exhausted at
  birth); neither is in a contract, deliberately — direction validation is
  already at T1 (error_taxonomy_test.go:113) and was not worth a per-run DLQ
  row. Steady state after a full tier run is communities/posts/comments = 1
  each, users/aggregators/votes = 0. SCOPE JUDGMENTS: comment_vote_test.go
  KEPT as T1 (task-13 rule-3 breadth — it holds the ONLY real-SQL coverage of
  GetVoteStateForComments; its one vacuous `if len(...)>0` subtest was
  de-vacuumed); community_avatar_e2e_test.go handled HERE not task 15 (its
  create-path DB assertions were a §3.4 false-pass — CreateCommunity writes
  Postgres synchronously — and splitting the file three ways across tasks
  would have left the coverage in limbo). COST: the e2e tier went 65s → 131s;
  TestVoteIngestion alone is 29s, of which 25s is five Holds windows, and
  every one of them is load-bearing (a "did not double" claim cannot be made
  by an eventually-check). make ci 2:45 → 3:55, all of it here.
- **From task 14's review (rules tasks 15-16 inherit)**: PIN PLACEMENT is now a
  written rule in tests/e2e/contracts_test.go's package doc, not a per-task
  judgment — a pin MAY sit inside a marked contract provided the contract still
  passes once the defect is fixed (vote's same-rkey pin qualifies; the
  out-of-order one does not and lives in its own unmarked function). Two
  obligations either way: name the ISSUE FILE in the assertion message so a red
  run says which defect got fixed, and pin the wrong-but-current value with
  HOLDS as well as Await — an Await-only pin is satisfied by an ASYNCHRONOUS
  fix (reconciliation pass, lazy repair) on its way to the right answer and
  then passes forever against corrected code. NORMALIZATION HAZARD, worth a
  standing check: two production comments asserted the opposite of what the p1
  pin proves ("the Jetstream consumer handles orphaned votes correctly",
  "zero rows is OK") — a pin whose subject is documented as correct behaviour
  will be "cleaned up" by the next reader, so replacing those comments with
  KNOWN DEFECT notes naming the issue file is part of filing, not optional
  polish. TESTKIT: XRPCClient.Get asserts only 2xx and DISCARDS the body, so it
  cannot support a "this URL serves an image" claim (a 204 passes) — use
  GetBinary, which returns status + content-type + bounded body; the image
  assertions want 200 AND non-empty AND image/*, and deliberately do NOT
  compare bytes because the proxy re-encodes per preset. FOUND WHILE FIXING:
  the communities test harness passed a nil blobService, invisible until a test
  uploaded an image (the service fails closed, "blob service not configured");
  it is now the real one, as cmd/server wires it. And CreateCommunity writes
  the AppView row SYNCHRONOUSLY while UpdateCommunity does NOT — asserted
  explicitly in service_provisioning_test.go, because if update ever starts
  indexing synchronously the community contract's update step silently becomes
  a false pass.
- **From task 14 (for tasks 15-16, 20)**: MARKER-PIN RULE in
  contracts_test.go doc: pins may live inside marked contracts iff the
  contract passes once fixed; pins carry issue IDs + "IF THIS FAILED the
  defect is FIXED" in assertion strings; production comments adjacent to
  pinned defects carry KNOWN DEFECT notes (normalization hazard).
  CreateCommunity indexes synchronously, UpdateCommunity does NOT (by
  design — it's what makes the update step honest pipeline proof; a
  tripwire T1 asserts the asymmetry). requireServesImage = host-check +
  200 + non-empty + image/* via GetBinary (any-2xx Get was a false-200).
  Dead-letter steady state after full tier: communities/posts/comments=1
  each, users/aggregators/votes=0 (task 16 accounting). e2e tier 131s,
  gate ~4:00 — watch at task 15 but §3.1 budget fine. Task 20: audit
  dialer count floor is 1 (production connector.go:318 — exempt, don't
  chase). oauth UpdateHandleByDID + expires_at now covered (first time).
- **From task 15 (for tasks 16-18, 20)**: DEAD-LETTER BASELINE (fresh
  stack, one full tier run, measured): users 1, communities 1, posts 1,
  comments 1, aggregators 2 (one TRANSIENT attempts=0 — the orphan
  authorization, redriven every 5min, retires at 10 ≈ 50min, PROVEN inert
  to all windows: redriver calls handlers directly, connector counters
  untouched), votes 0. Task 14's older communities=1 note superseded.
  RELIABILITY-SUITE MATERIAL: authorization-before-aggregator recovery
  (declare the aggregator → redrive lands it — the best transient-redrive
  test in the tree; 5-min tick needs harness thought). PHASE-5 PRE-WORK
  LIST: (1) sealed-session test mint, (2) service-JWT helper
  (getServiceAuth vs INSTANCE_DID — unlocks dual-auth boundary incl.
  valid-JWT-from-non-aggregator refused; needs INSTANCE_DID in .env.ci).
  PRODUCT QUESTIONS for Bretton: block enforcement is one-directional
  viewer-side (Bluesky-style mutual invisibility unimplemented — intent?);
  community.block entirely unenforced (defect filed). CARRY LIST for the
  tests/integration dissolution: community_identifier_resolution_test.go
  :163-206 holds the ONLY malformed scoped-identifier tests (noted in
  pending_contracts.txt header too). Known flake vector (documented in
  requireRejected): reconnect within 5s cursor-rewind double-counts a
  permanent dead letter (ON CONFLICT dedup vs unconditional counter).
  Aggregators = the only identity with NO must-know-first gate.
  Reserved-TLD rule: .invalid never resolves ANYWHERE (RFC 2606) — the
  mechanism for unverifiable-domain fixtures, not the egress block.
