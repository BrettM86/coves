# Coves Test Architecture

**Status: PROPOSED — this document is the spec for the test-suite refactor on branch `worktree-test-refactor`.**
Revision 2: incorporates an external design review (OpenAI Codex, `gpt-5.6-sol`, 2026-07-28) which surfaced several structural blind spots — most importantly that the original black-box E2E design could pass with a dead firehose for synchronously-indexed domains (§3.4), that per-package `TestMain`s race on template-DB creation (§3.3), and that the phase ordering broke its own green-boundary invariant (§4).

This document replaces `TESTING_SUMMARY.md` and `docs/E2E_TESTING.md`, both of which describe a system that no longer exists (wrong ports, references to files that were never written, no mention of the real gate `make ci`). Delete both when Phase 6 lands.

---

## 1. Goals

We work agentically. The test suite is the thing that lets an agent (or a human) change code quickly and *know* whether it caused a regression. That only works if:

1. **Green means deployable.** A passing run must certify the whole pipeline — endpoint → PDS → firehose → consumer → Postgres → serving endpoint — not just the parts whose infrastructure happened to be up.
2. **Red means a regression.** No flakes. A test that fails 1-in-20 runs trains agents and humans to retry instead of investigate, which is worse than not having the test.
3. **Skips are failures unless explicitly allowlisted.** Today there are 449 skip sites and `make test-all` counts every one as a pass. Stop the PDS and the suite still prints green. `cmd/ci-report` already inverts this — it becomes the only skip mechanism.
4. **Fast feedback tiers.** An agent iterating on a service should get signal in seconds (unit), on a repo in ~a minute (integration), and full pipeline certification in minutes (e2e) — each tier runnable independently.
5. **Hermetic means hermetic: tests NEVER touch public atProto infrastructure.** No `plc.directory`, no Bluesky relays/Jetstreams, no public PDSes — everything routes through our self-hosted Docker services (PLC directory, PDS, relay, Jetstream). This is a hard rule, enforced mechanically (3.7), with exactly one exception: the opt-in T3 `live` tier, which exists precisely so that reality checks are explicit instead of accidental.

## 2. Where we are (July 2026 survey)

The full survey lives in the PR description for this branch. Counts below are approximate and were sampled at spec-writing time; the migration's real accounting is the violation audit script (§3.6), not this prose. The load-bearing facts:

- **~84k LOC of tests** (41k in `tests/integration` alone), essentially no `t.Parallel()`, essentially no `require.Eventually`, **zero** build tags. The only tier selector is `-short`, which silently skips ~161 guarded sites — `make test` runs almost none of the integration suite while printing green.
- **Taxonomy is inverted.** `tests/e2e/` contains two pure unit tests of the rate limiter (`httptest.NewRecorder`, no infra); `tests/integration/` contains the most end-to-end test in the repo (`user_journey_e2e_test.go`). 15 files named `*_e2e_test.go` live in `tests/integration`. Directory, filename, and function name each imply a different tier and none binds to what the test does.
- **Global mutable DB state.** `setupTestDB` (divergent copies) is called 222 times across 50 files; each call re-runs migrations and issues 6 unscoped `DELETE FROM`s against the one shared database on :5434. This is why `-p 1` is mandatory and parallelism is impossible. 109 more ad-hoc `DELETE FROM`s live inside test bodies.
- **~700 LOC of copy-pasted firehose plumbing.** 10 near-identical `subscribeToJetstream*` functions differing only in consumer type. The one correct anti-flake primitive — capture the Jetstream cursor *before* the PDS write, then subscribe from that cursor (`jetstreamCursorNow`/`withJetstreamCursor`, `helpers.go:152-177`) — is used at 1 of 29 dial sites. The other 28 paper over the subscribe-after-write race with `time.Sleep(500ms)`.
- **The consumer wiring is never tested.** Every "E2E" test instantiates its *own* consumer and feeds it events; `cmd/server`'s actual consumer wiring runs in production only. Worse (external-review catch): several domains index **synchronously** on the client path — signup writes users directly, community creation persists to the AppView DB in the same request (`internal/core/communities/service.go`, "the Jetstream consumer will eventually index … but we must store now") — so "write via endpoint, read via endpoint" passes even with the firehose dead. Proving the pipeline requires writes the AppView did *not* see (§3.4).
- **Coverage is inverted relative to risk.** `internal/core/communities` (7 src files), `internal/core/votes` (6), `internal/atproto/identity` (7), `internal/api/routes` (17), `timeline`, `discover`, `communityFeeds`: **zero unit tests** — verified only through the slowest, most-skipped tier. `internal/db/postgres`: 16 repos, 3 test files.
- **The good news:** the hermetic CI harness (`make ci` → `scripts/ci.sh` → `docker-compose.ci.yml` → `scripts/ci-runner.sh` → `cmd/ci-report` + `tests/ci/allowed_skips.txt`) is the highest-quality artifact in the tree: hermetic stack, skip-inversion, staleness enforcement, mandatory skip reasons. The refactor builds *on* it, not around it.

## 3. Target architecture

### 3.1 Four tiers, mechanically enforced by build tags

| Tier | Tag | Lives in | Talks to | Wall-clock budget | Parallel |
|---|---|---|---|---|---|
| **T0 Unit** | *(none)* | in-package (`internal/…`, `cmd/…`) | nothing out-of-process | < 60 s total | yes, default |
| **T1 Integration** | `//go:build integration` | in-package, next to the code it tests | Postgres only | < 3 min total | yes (see 3.3) |
| **T2 Pipeline (E2E)** | `//go:build e2e` | `tests/e2e/` | full hermetic stack incl. running AppView | < 10 min total | **no — serial** (see 3.4) |
| **T3 Live** | `//go:build live` | `tests/live/` | public internet (Bluesky API, unfurl targets, real PLC) | opt-in only | — |

Rules that give the tiers teeth:

- **Build tags are the only tier mechanism.** `-short`/`testing.Short()` is deleted everywhere. You cannot accidentally run (or accidentally *not* run) a tier.
- **Tags classify files, so files must be single-tier.** Several current files mix tiers in one file (`identity_resolution_test.go`: DB cache tests + live-PLC tests; `bluesky_post_test.go`: pure URL parsing + live Bluesky API + DB; `post_unfurl_test.go`: local fixtures + third-party targets). These get **split by test function** during Phase 2, tracked in a migration manifest — tagging them in place is not possible and pretending otherwise was a hole in revision 1.
- **Missing infrastructure is a FAILURE, not a skip.** If you invoked `-tags integration`, you asked for Postgres; if it's not there, `t.Fatal`. All 34 copy-pasted "PDS not running → t.Skipf" preambles are deleted. The harness (3.3) does one connectivity check per package setup and fails fast with a message that says exactly which `make` target brings the stack up. The same applies to T3: an explicitly-invoked `make test-live` with missing config **fails**, it does not skip — skips are for the merge gate, and `live` is never on the merge path.
- **`t.Skip` is banned in test bodies.** The only legitimate skips are environmental gates owned by the harness, and every one must appear in `tests/ci/allowed_skips.txt` with a reason or `ci-report` fails the run. The 6 "DEBT — never run" entries in the current allowlist get deleted or implemented in Phase 2 — not carried forward. The 8 allowlisted "defs-only lexicon" subtest skips are fixed at the source in Phase 2: the validator stops *generating* subtests for defs-only lexicon files instead of generating-then-skipping them.
- **`tests/integration/` and `tests/unit/` are dissolved** by the end of the migration. Repo/service/consumer tests move next to the code they test (T1); pipeline sagas become T2 contracts; the two rate-limiter "e2e" files become T0 tests in `internal/api/middleware`.

### 3.2 What each tier is *for*

**T0 — logic.** Validation, transforms, cursor encoding, error mapping, service logic against interface fakes. This is where behavioral breadth lives: every edge case, every error path. Packages currently at zero (communities, votes, identity, routes, timeline, discover, communityFeeds) get their matrix coverage here, not in 1,400-LOC integration files.

**T1 — the seams.** Two seams matter and both terminate at Postgres:
- *Repo tests*: real SQL against a real schema (`internal/db/postgres`, 16 repos, currently 3 tested).
- *Consumer tests*: synthetic `jetstream.JetstreamEvent` → `consumer.HandleEvent` → assert rows. This is today's "Strategy A" and it is legitimate — it just isn't E2E and stops being named that. Idempotency, out-of-order delivery, rev-gating, malformed records: all here, cheap and deterministic.

Feed/timeline/discover sorting-and-cursor tests (today's `feed_test.go`, `timeline_test.go`, `discover_test.go`, `comment_query_test.go` — DB-fixture-driven, no PDS) are T1 repo/service tests and move accordingly.

**T2 — the pipeline contracts.** See 3.4. Narrow and deep, not broad: ingestion proof per consumed collection, API contracts per client-facing write surface, plus a small pipeline-reliability suite.

**T3 — reality checks.** Live Bluesky handle resolution, real unfurl targets (YouTube/Reddit/Streamable/Kagi), real PLC. Valuable (real data catches what fixtures don't) but never on the merge path. Run nightly/pre-release via `make test-live`; failures notify, don't block.

### 3.3 One harness: `tests/testkit`

A single package imported by all tiers. Everything below already exists in embryonic form somewhere in the tree — the harness is mostly consolidation, and it kills the current 3×`setupTestDB` + 10×`subscribeToJetstream*` + 5×image-fixture + 4×PDS-client-factory duplication (the four drifted `*PasswordAuthPDSClientFactory` adapters, both `createPDSAccount` definitions, and the hand-rolled XRPC clients all collapse into `pds.go`).

```
tests/testkit/
  testkit.go      // package setup helper: env, log silencing, infra fail-fast probe
  db.go           // Postgres isolation (below)
  pds.go          // account/session/record helpers (absorbs helpers.go:84-277)
  firehose.go     // cursor-gated subscribe, ONE generic implementation (T1/T3 debugging aid)
  wait.go         // the only wait primitives allowed in tests
  fixtures.go     // UniqueID, PNG/JPEG bytes, generic record builders
  appview.go      // T2 only: XRPC client against the running AppView
```

**Dependency direction (hard rule, prevents import cycles).** `testkit` imports **no** `internal/core/*` domain package — only infra-level packages (`jetstream` event types, DB driver, migrations). This matters because T1 tests are *in-package*: if `testkit` imported `internal/core/communities`, then `communities`' own test file importing `testkit` would be an import cycle. Domain-specific builders that need domain types live either in the domain's own `_test.go` files or in small leaf `<domain>test` packages — never in the shared kit.

**DB isolation — template-clone-per-test, harness-provisioned.** Template creation/migration does **not** live in per-package `TestMain`s: `go test ./...` runs each package as a separate binary, several in parallel, and N processes racing to create-and-migrate `coves_test_template` is a built-in flake. Instead:

- The **Make/CI harness** provisions the template once before `go test` runs (`make test-integration` and `ci-runner.sh` both call the same `scripts/test-db-prepare.sh`: create template if absent, run migrations, stamp it with a hash of the migrations dir; re-provision on hash mismatch).
- As a belt-and-suspenders for direct `go test -tags integration ./internal/foo` invocations, `testkit`'s package-setup path takes a **Postgres advisory lock** around a verify-or-provision of the template, so ad-hoc runs are safe too.
- `testkit.DB(t)` executes `CREATE DATABASE … TEMPLATE coves_test_template` with a sanitized unique name, returns the pool, and `t.Cleanup` force-drops it (`WITH (FORCE)`, so leaked connections can't wedge teardown). Panic-orphaned clones are swept by `test-db-prepare.sh` on the next run (name prefix + age).
- **Connection budgets are explicit.** Cloned-per-test pools multiply fast: pool size defaults to 2–3 per test DB, `-parallel` is capped by a computed budget (`max_connections` ÷ pool size, with headroom for the AppView), and the CI Postgres sets `max_connections` accordingly. This is configured once in testkit + compose, not per test.

Cost is ~100–200 ms per test on local NVMe — trivially bought back: all 222 `setupTestDB` call sites, all unscoped `DELETE FROM` wipes, and every per-file `cleanup*` function are deleted; tests cannot see each other by construction.

**Parallelism is earned, not assumed.** DB isolation removes the *biggest* blocker to `t.Parallel()`, not every blocker: the tree also has ~100 sites of process-global mutation (`t.Setenv` — which Go itself rejects under `t.Parallel()` — plus `os.Setenv`, logger and default-HTTP-client fiddling). Phase 3 therefore audits and classifies these first (inject env/clock/client via testkit instead of mutating globals), enables `t.Parallel()` only on proven-safe tests, and runs the race detector as part of the phase's exit criteria. `-p 1` is deleted at the end of that phase, not the start.

**Firehose helper — T1/T3 scope only.** One generic, cursor-gated subscriber replaces the 10 copies (capture cursor *before* the write, replay from it — the subscribe-after-write race becomes unrepresentable through this API; the gorilla/websocket `maxConsecutiveTimeouts` guard is written exactly once). But note its narrow role: **T2 contracts never dial websockets** — the AppView's own consumers do the consuming (3.4). The helper exists for T1-adjacent consumer plumbing tests and debugging, not as the E2E mechanism.

**Waiting — three primitives, everything else is banned:**

```go
testkit.WaitFor(t, timeout, probe)   // eventually-true; poll at 100ms
testkit.Holds(t, window, probe)      // stays-true: deletes STAY deleted, counts STAY stable
                                     //   (an eventually-check cannot catch resurrection-by-replay)
fh.Await(t, match)                   // firehose delivery (T1/debugging scope, see above)
```

Probes return `(done bool, err error)` where a non-nil `err` is **terminal** — a 401 or 500 from a serving endpoint fails immediately with that response attached, instead of being retried into an opaque timeout (agents debugging a timeout with no context is exactly the failure mode this tier exists to avoid). On timeout, `WaitFor` reports the last observation, and in T2 additionally snapshots the AppView's consumer-health endpoint (cursor positions, dead-letter counts) into the failure message.

`time.Sleep` in `*_test.go` fails CI via the lint gate. All sleeps, hand-rolled `select`/`time.After` blocks, and bare retry loops migrate to these.

**Identity — `testkit.UniqueID(t)`** is the only handle/ID generator: a per-run random prefix (seeded once per process) + atomic counter, ≤18-char PDS-safe (per the PDS local-label cap). The counter-only design of revision 1 was insufficient: counters reset across processes and local PDS state persists across runs, so two `make test-e2e` invocations in the same second could collide. The surviving `time.Now().Unix()` handle-collision sites (`user_signup_test.go:69,110,153`, `community_e2e_test.go:177,380` — one inside a loop) are migrated in Phase 1. Hermetic-stack runs get fresh PDS volumes; for kept stacks (`COVES_CI_KEEP_STACK`) accumulation is accepted and documented — the run-scoped prefix makes it harmless.

**Assertions — testify everywhere.** New/touched code uses `require` (fail fast) / `assert` (accumulate); no new bare `t.Errorf`.

### 3.4 The E2E methodology: pipeline contracts

This is the core opinionation and the reason the tier exists. The external review exposed a fatal flaw in revision 1's "write via endpoint, poll via endpoint" shape: **several domains index synchronously on the client path** (signup writes users directly; community creation persists to the AppView DB in the same request), so a black-box client-path test passes with the firehose completely dead. The tier is therefore split into three explicit contract classes:

**(a) Ingestion contracts — the pipeline proof. One per consumed collection, mandatory.**

```
Record written DIRECTLY to the PDS (bypassing the AppView entirely)
  → PDS commits → firehose → Jetstream
  → THE APPVIEW'S OWN CONSUMERS (cmd/server wiring, running as a container)
  → Postgres
  → XRPC serving endpoint, polled with WaitFor until the record appears
  → destructive steps additionally verified with Holds (deletes STAY deleted)
```

Because the AppView never saw the write, firehose delivery is the *only* way the data can appear — a dead consumer cannot false-pass. Shape: create → visible; update → visible; delete → gone *and stays gone*. This is honest **direct-PDS-path** testing, not federation (the record still lives on the one PDS the stack fronts) — true federation-path testing needs a second, independently-addressed PDS and a relay in the hermetic stack, which is Phase 5 scope, and the spec stops calling single-PDS direct writes "federation".

**The contract inventory is generated, not hand-curated.** Revision 1's hand-written list was already wrong (it missed `social.coves.actor.block` and `social.coves.community.block`, and collapsed `aggregator.service`/`aggregator.authorization` — two distinct record types). The source of truth is `jetstream.WantedCollections` (the map in `internal/atproto/jetstream/feeds.go`): a CI check walks every consumed collection and fails if any lacks a `//coves:ingestion-contract <collection>` marker in `tests/e2e/`, or if a marker points at a collection no longer consumed. Adding a collection to a consumer without a contract breaks the build — mechanically, not by review vigilance.

**(b) API contracts — the client-facing surface. One per write endpoint family.**

Client-path through the AppView's XRPC write endpoints, exactly as the mobile app calls them, asserting the *response* and the *synchronous* effects (session issued, record URI returned, synchronously-indexed rows present, blob accepted). These verify what third-party clients experience — including precisely the synchronous-indexing behavior that makes them unsuitable as pipeline proof. Avatars/blob uploads are covered here as steps of the `user` and `community` API contracts (they are blob-path cases of those record types, not record types of their own).

**Known limitation (July 2026, discovered writing the community contract): T2 cannot authenticate a write.** `OAuthAuthMiddleware.RequireAuth` accepts exactly one credential — a *sealed* session token naming a row in the OAuth session store — and the only thing that mints one is `/oauth/callback`, at the end of the browser authorization-code flow against the PDS' own HTML login pages. `social.coves.actor.signup` returns the PDS' `accessJwt`, which is not sealed and is rejected; `/oauth/refresh` requires a sealed token to begin with. The integration tier sidesteps this in-process (`store.SaveSession` + `client.SealSession`), which T2 cannot do without writing to the AppView's own database — the one thing §3.4's rules forbid. So until that changes, an API contract covers **the auth boundary** (every write NSID answers 401 to a session-less client — which also catches a route registered without the middleware, something a handler test structurally cannot see) and **the read surface** (a record indexed through the pipeline is served back by every identifier form a client may use), while authenticated write *behaviour* is proven at T1: handler tests against a mock service, plus write-forward tests that assert the record shape against a real PDS. A hard-gated, test-only session-minting path in the AppView would close the gap and is Phase-5 pre-work, not something a contract may improvise.

**(c) Pipeline-reliability suite — the failure modes CRUD never touches.**

The production ingestion path has machinery that steady-state contracts cannot exercise: persisted cursors, reconnect-and-replay, rev-gating (stale events must not resurrect or regress records), duplicate delivery, dead-letter capture, multi-feed consumers. One small suite covers it end-to-end: restart the AppView mid-stream and verify cursor resume; write during a Jetstream outage and verify replay indexes exactly once; deliver a stale rev after a delete and verify no resurrection (`Holds`); poison a record and verify dead-letter capture + consumer health reporting. CI's single self-feed topology differs from prod's multi-feed setup, so this suite also runs one overlapping two-feed configuration to exercise the rev-gating overlap path.

Rules that hold across all three classes:

1. **T2 tests never instantiate consumers and never dial websockets.** The AppView container consumes, exactly as deployed; tests observe via serving endpoints (plus the consumer-health endpoint for reliability assertions). This deletes the entire `subscribeToJetstream*` family from the E2E tier and finally exercises `cmd/server`'s wiring.
2. **T2 runs serially.** All contracts share one AppView, one `coves_dev` DB, one PDS, one PLC, one cursor stream — template-cloning isolates T1, not this tier. ~10–15 serial contracts fit the 10-minute budget; if it ever grows past that, the answer is stack-sharding, not interleaving writers into a shared eventually-consistent namespace. All identities are run-scoped via `UniqueID`.
3. **Behavioral breadth is out of scope.** "Does `sort=alphabetical` work" (17 of `community_e2e_test.go`'s 20 subtests) is a T1 service/repo test. If a T2 contract fails, the answer to "which layer broke?" should be "the pipeline," not one of 20 endpoint behaviors. This is how the 1,820-LOC and 1,001-LOC god-files get decomposed rather than moved.
4. **Sequential steps within a contract are legitimate here and only here** — eventual consistency is a pipeline, and the pipeline is what's under test.
5. **One cross-domain saga survives**: `user_journey` (signup → community → post → comment → vote → timeline), rebuilt on testkit, as the single "does the whole product hold together" smoke test.

### 3.5 Command surface

```
make test              # T0. No Docker. The inner loop. <60s.
make test-integration  # T1. Provisions template DB, starts postgres-test if needed. Parallel.
make test-e2e          # T2. Runs INSIDE the hermetic stack via the compose runner (see below).
make test-live         # T3. Opt-in, hits the internet. Missing config = failure, not skip.
make ci                # THE GATE: hermetic stack, T0+T1+T2, ci-report skip inversion.
```

- **`make test-e2e` runs through the compose runner, not from the host.** The hermetic stack publishes no host ports (by design, see 3.7), so a host-run `go test -tags e2e` cannot reach it; revision 1's "requires the ci stack or dev stack up" was incoherent. The target brings up the stack (or reuses a `COVES_CI_KEEP_STACK` one) and executes the e2e binary inside the runner's network namespace — the same path `make ci` uses, so there is exactly one way T2 executes. Debugging against the long-lived dev stack stays possible via an explicitly-named `make test-e2e-dev` escape hatch.
- `make ci` stays exactly what it is today (build from tree → staged compose up → bootstrap → run → `ci-report`), except the runner invokes tiers by tag: T0+T1 with `-tags integration` in parallel, then T2 with `-tags e2e` serially against the composed AppView.
- `make test-all` is deleted. It is a slower `make ci` with weaker guarantees (any-one-service-up counts as "infra ready", skips count as passes, `./internal/...` runs without `-p 1` — a live race today). One gate, not two.
- Silent-failure fixes ride along: `goose up || true` in `make test` loses the `|| true`; `sleep 3` becomes `pg_isready` polling; dead targets (`create-test-account`, `verify-stack` — scripts that don't exist) are removed.

### 3.6 Enforcement (so it doesn't rot back)

The suite got here by drift, so the invariants get mechanical guards, all inside `make ci`:

1. `ci-report` (exists): skip ⇒ failure unless allowlisted-with-reason; stale allowlist entries fail.
2. **Contract manifest check** (new, Phase 4): every collection in `jetstream.WantedCollections` has a live ingestion-contract marker in `tests/e2e/`, and no marker is stale (3.4a).
3. **Violation audit script** (`scripts/test-audit.sh`, new in Phase 1): counts `time.Sleep` in tests, `t.Skip` outside testkit, `testing.Short()`, `websocket.DefaultDialer` outside `testkit/firehose.go`, hardcoded endpoint literals outside testkit, and public atProto hostnames outside `tests/live/`. It is both the lint gate (warn during migration → hard-fail at Phase 6) and the migration's progress meter — every count must be assigned to a phase and reach zero, so "the final lint flip will pass" is tracked continuously instead of hoped for. These greps are **tripwires, not proof** — they catch drift cheaply and are bypassable by construction (concatenation, IP literals); the *guarantee* is layer 3 of 3.7.
4. `go vet ./...` with each tag set — tagged files that don't compile are otherwise invisible.

### 3.7 Network isolation: never the public network

Three layers, from convention to physics:

1. **All endpoints come from testkit, all defaults point at the Docker stack.** `testkit.Endpoints()` reads `PDS_URL`, `PLC_DIRECTORY_URL`, `JETSTREAM_URL`, `APPVIEW_URL`, `POSTGRES_TEST_*` once, defaulting to the compose-stack addresses. No other test code constructs a base URL. (This also retires the 3 leftover `localhost:2583` references — a stale upstream-PDS default from a different topology.)
2. **Lint tripwire** (3.6.3): public atProto hostnames in test/testkit code outside `tests/live/` fail CI. Scope-aware, not a blanket ban — hostname literals in *fixture data* (e.g. validating production config parsing) are fine and get an explicit exemption comment; the tripwire targets endpoint construction.
3. **Egress-blocked CI network (the actual guarantee)**: the hermetic stack's networks become `internal: true`, so an accidental public call — however constructed — fails at connect time instead of silently resolving against real infrastructure. Two prerequisites discovered in review, both handled in Phase 0 *after* the public-network tests are moved out (see ordering in §4): (a) the runner image downloads Go modules at runtime, so a cold cache + no egress = broken build — `test-db-prepare`/`ci.sh` populate the module cache volume *before* attaching the runner to the internal network (and `make ci-clean` notes that the next run re-populates); (b) a cold-cache, egress-blocked `make ci` run is an explicit Phase 0 acceptance check, not an assumption.

The env-gated real-handle tests (`TEST_REAL_HANDLES=1`) hit the real PLC by design — they become T3 `live` tests, where that is explicit and opt-in rather than an env var buried in a conditional.

## 4. Migration plan

Each phase lands as its own commit(s) on `worktree-test-refactor`, and **`make ci` must be green at every phase boundary**. Phases are sized to be a focused agent session each, except Phase 4 (multi-session, per-domain). LOC figures are rough deltas (added − deleted).

**Green-at-boundary is necessary but not sufficient for the destructive phases.** A green run proves the surviving tests pass, not that coverage survived. Phases 2 and 4 therefore follow a **strangler rule**: before an old test file is deleted, its distinct *behaviors* (not its LOC) are inventoried in the phase's migration manifest and mapped to their new home (T0/T1 test or T2 contract), and the mapping is reviewed in the PR. The net-LOC reduction below is an *expectation*, not a target — an agent optimizing for deletion is exactly the failure mode the manifest exists to catch.

- **Phase 0 — Make the gate real.** *(~200–500 LOC touched, net ~0)* Order matters here (revision 1 had it backwards): **(1)** get `make ci` green as imported (baseline commit `aa57ccc`); **(2)** relocate the public-network test files (`bluesky_post_test.go`, live-target unfurl tests, `TEST_REAL_HANDLES` identity tests) to `tests/live/` with the `live` tag — move-only, no rewrite; **(3)** flip the stack networks to `internal: true` with module-cache pre-population (3.7.3); **(4)** prove a cold-cache egress-blocked run is green and record timing + allowlist as the baseline. *Exit: green hermetic run with no public egress from a cold cache; timings captured.*
- **Phase 1 — testkit + audit script.** *(net +2,500: ~1,500 harness + ~800 harness tests + audit script)* Build `tests/testkit` (db/pds/firehose/wait/fixtures/appview) with the dependency-direction rule; `scripts/test-db-prepare.sh` (template provisioning + advisory-lock fallback + orphan sweep); `scripts/test-audit.sh` wired into CI as warnings with every count assigned to a phase; fix the 5 surviving handle-collision sites. No mass migration yet — testkit ships with its own tests. *Exit: testkit exists and is tested; audit baseline recorded with per-phase burn-down.*
- **Phase 2 — Kill the lies, split the mixed files, tag everything.** *(net −800)* Delete the 6 never-run debt tests. Fix the lexicon validator to not generate defs-only subtests (retiring those 8 allowlist entries). Move the 2 rate-limiter "e2e" files to `internal/api/middleware` as T0; fold `tests/unit/community_service_test.go` toward `internal/core/communities`. **Split multi-tier files by test function** (manifest-tracked) so every file is single-tier, then add build tags in place; retarget the Makefile to tags and delete `-short`. *Exit: tiers mechanically selectable; `make test` honest; every file single-tier; allowlist ≤ ~5 entries.*
- **Phase 3 — DB isolation, then parallelism.** *(net −1,800)* Migrate `setupTestDB`'s 222 call sites to `testkit.DB(t)`; delete all unscoped `DELETE FROM`s and per-file cleanups. Then the global-state pass: audit `t.Setenv`/`os.Setenv`/logger/http-default mutation sites, convert to testkit injection, enable `t.Parallel()` on proven-safe tests, set the connection budgets (3.3), run `-race` clean. Drop `-p 1` last. *Exit: parallel suite green under `-race`; wall-clock vs Phase 0 recorded.*
- **Phase 4 — Pipeline contracts.** *(net −8,000 to −12,000 — multi-session, one domain at a time: post, comment, community, vote, user, blocks, aggregators…)* Per domain, strangler-style: inventory the old files' behaviors → add the missing T0/T1 tests they imply → build the ingestion contract (3.4a) and API contract (3.4b) on testkit → run old + new together → delete the old file after reviewed parity. Build the reliability suite (3.4c) and the contract-manifest CI check. Delete the 10 `subscribeToJetstream*` copies as their callers migrate. `tests/integration/` ends empty and is removed. *Exit: every consumed collection has an ingestion contract (CI-enforced); reliability suite green; `tests/integration` gone.*
- **Phase 5 — Coverage debt + real federation topology.** *(net +7,500, additive)* Unit tests for the zero-coverage core packages (communities, votes, identity, routes, timeline, discover, communityFeeds — in that order); repo tests for the 13 untested repos. Then the topology work revision 1 hand-waved: add a **second PDS** (own hostname, storage, PLC registration) and a hermetic **relay** to the CI stack, and promote the highest-value ingestion contracts (post, comment, vote — cf. the known vote-federation gap) to true federation-path: record written to the *non-fronted* PDS, discovered and indexed purely via the firehose, remote identity resolution and blob fetching asserted explicitly. *Exit: no zero-test package in `internal/core`; federation contracts for the big three running against the two-PDS topology.*
- **Phase 6 — Enforcement flip + docs.** *(net −400)* Audit-script counts must be zero; flip it from warn to fail. This doc moves from PROPOSED to CANONICAL; `TESTING_SUMMARY.md` + `docs/E2E_TESTING.md` deleted; CLAUDE.md testing section points here.

Aggregate expectation: the suite lands around **~78k LOC (from ~84k) with materially more coverage** — duplication currently dwarfs the gaps, so the refactor is net-negative even while filling seven zero-coverage packages. Measured done-criteria (via `test-audit.sh`, not prose): 0 `time.Sleep` in tests · 0 `t.Skip` outside testkit · 0 `testing.Short()` · allowlist ≤ ~5 entries, all `~`-conditional · 0 packages in `internal/core` without tests · `-p 1` gone · every consumed collection contract-covered · `make test` < 60 s · `make ci` green at ≤ Phase-0 wall-clock.

## 5. Decisions & rationale (short form)

- **Build tags over directories-as-tiers**: the current tree proves naming conventions don't survive contact with velocity. Tags are compile-time, greppable, and un-forgettable — with the corollary (learned in review) that files must be split to single-tier first, because tags classify files, not tests.
- **T1 colocated with code, not in `tests/`**: repo tests next to repos get maintained when the repo changes; a 41k-LOC `tests/integration` catch-all demonstrably doesn't. `tests/` keeps only what is genuinely cross-cutting: e2e contracts, live tests, testkit, ci config.
- **Template-clone-per-test over shared-DB-with-cleanup**: cleanup-discipline is exactly what failed (331 DELETE statements). Isolation by construction is the only version that survives agents writing tests at speed — provisioned by the harness, not racing `TestMain`s.
- **Direct-PDS ingestion contracts over client-path-only E2E**: synchronous indexing on the client path means endpoint-in/endpoint-out tests can't prove the pipeline. Bypassing the AppView on the write side makes firehose delivery the only possible explanation for a passing read — un-fakeable by construction.
- **Serial T2 over parallel**: one shared AppView/PDS/DB namespace makes interleaved eventually-consistent writers a flake factory. Ten serial contracts inside a 10-minute budget is the boring, correct call.
- **Black-box T2 over test-instantiated consumers**: tests the wiring that actually ships, deletes the largest duplication cluster, and makes the E2E tier readable as product documentation.
- **Skips-as-failures stays and expands**: `cmd/ci-report` is the best idea already in the tree. The refactor makes the allowlist *shrink* to near-zero rather than institutionalizing it.
- **Generated contract manifest over reviewed inventory**: the hand-written inventory was wrong on day one (missed two collections). Deriving it from `WantedCollections` makes "every collection is pipeline-tested" a build invariant instead of a review habit.
