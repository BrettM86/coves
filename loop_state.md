# Author-owned posts build-loop state

Protocol: `plan.md` §Loop protocol — one task per iteration:
analyze (parent) → **/tdd** (Opus 5 phase agents — never downgrade) →
**/second-opinion** (base: `feat/author-owned-posts`) → **/fix-pr** →
`make ci` → `--no-ff` merge into `feat/author-owned-posts` → delete task
branch/worktree → update this file → next task.
Spec authority: `docs/PRD_AUTHOR_OWNED_POSTS.md` (rev 2.1).
`main` is touched once, at the end, via /merge-to-main after task 8.
Stop when every task is done, or on any `blocked:` row.

## Task table

| # | Task | Phase | Status | Merge commit | Notes |
|---|------|-------|--------|--------------|-------|
| 1 | Lexicons: postv2 + acceptance + removal, deprecation note, fixtures, T0 validation | A | done | 4574151 | make ci 4640/0. 10-stream review; codex HIGH: rkey transform non-total → digest scheme (PRD rev 2.2) |
| 2 | Migration 034: community_post_admissions + posts FK drop; admissions repo + T1 transition/watermark matrix | A | done | 9491744 | make ci 4735/0. PRD → rev 2.4 (tuple watermark, pending-only rejection CAS, repo-side op-rank). Behavior flips: unknown authors indexable, explicit deletion sweep |
| 3 | admitPost extraction + NEW policy (bans, rate limits, dedupe) wired into existing write path | A | done | df97cb2 | make ci 4840/0. PRD → rev 2.6. Migration 035 ledger (plan review killed posts-table limiter). BANS NOW ENFORCED. T2 wire probe added |
| 4 | Acceptance engine: deterministic rkey, swap-safe acceptance writer, atomic applyWrites removal, repin/terminality rules | B | done | e00d97a | make ci 4944/0. Probe-driven plan review killed 4 assumptions pre-code. 2 production bugs fixed as side effects. Gate saga: 2 latent test defects fixed + Docker restart (150d uptime) |
| 5 | Ingestion: postv2/acceptance/removal consumers, watermark gating, direct-fetch convergence, WantedCollections + 3 e2e contracts | B | done | 0caeda4 | make ci 5047/0. PRD → rev 2.7. getStatus pulled forward; migration 036; queue driver + decider + factory; 6 production defects fixed as by-catch; CAR-recomputed CID verification |
| 6 | Write path flip: author-repo postv2 via session, author-PDS blobs, sync fast-path accept, post.delete flip, post.update NEW | C | done | 2c66287 | make ci 5120/0. pragma:security FIRED — bypass CLEAN. Seed-rewind (published false acceptance) + UpdatePost validation bypass + dead token step + SSRF fixed. DEPLOY GATE: not ahead of task 7 |
| 7 | Read path: centralized visibility predicate, full surface inventory, #removedPost, getStatus, alternate-endpoint invisibility T2s | C | pending | | |
| 8 | Cutover: re-materialization script (ledger, verify-before-delete, hermetic test), old-path removal, docs, tracker cleanup; panel on whole branch; /merge-to-main | D | pending | | prod script run is MANUAL, outside loop |

## Cross-iteration notes

- (2026-08-07, task 1) **rkey design changed under review**: acceptance/removal
  rkeys are SHA-256 → unpadded lowercase base32 digests of the subject AT-URI
  (PRD rev 2.2) — the readable transform broke on >512-byte / percent-escaped
  DIDs. TASK 4 MUST implement the digest helper with long-DID and
  percent-escape test vectors.
- (task 1) Fixture harness parity: fixtures containing blobs need
  atdata.Blob conversion before ValidateRecord (both harnesses now do this —
  convertBlobs in tests/lexicon_fixtures_test.go and cmd/validate-lexicon).
- (task 1) validate-lexicon's coverage report is now honest (parses
  defs.main.type): 4 pre-existing record types have zero fixtures
  (actor.block, community.block, aggregator.authorization,
  aggregator.service) — pre-existing gap, not this loop's scope.
- (task 1) One transient unreproducible `make test` FAIL observed after the
  fix batch (no package captured; cold-cache ×2 green; make ci 4640/0
  green). Watch for recurrence — if seen again, capture the package and
  /file-issue.
- (2026-08-08, task 2 incident) **Conductor wiped uncommitted GREEN work**:
  `git checkout <file>` used to revert a deliberate test-bite mutation reset
  admission_repo.go to the RED-stub commit because GREEN's gate-passed
  cycle-1 work was never committed. Recovered from the persistent GREEN
  agent's context. HARD RULES now: (1) commit at EVERY gate — RED gate AND
  GREEN gate, before the next phase starts; (2) revert deliberate mutations
  by re-editing the line, NEVER `git checkout`/`git restore` on files with
  uncommitted multi-agent work.
- (2026-08-08, task 2 → TASK 5 OBLIGATIONS): the engine/consumers must use
  the AdmissionRepository outcome taxonomy correctly (skips NEVER
  dead-letter); rejection = RecordRejection(judgedCID) from pending only —
  re-acceptance failure is REMOVAL not rejection (PRD §5.6); ingestion must
  gate events for DELETED accounts (stale replay could recreate swept
  admissions — codex catch, deferred); community deletion leaves orphan
  admissions rows (pre-existing-adjacent, sweep in task 5 or 8).
- (task 2 → TASK 7 OBLIGATION): post read paths INNER JOIN users — posts by
  unknown authors index fine but are INVISIBLE to every hydrating read
  (GetViewsByURIs/GetByAuthor). Task 7's visibility work must add
  opportunistic-hydration-tolerant joins or the write path's promise breaks
  silently at the read path.
- (2026-08-08, task 3 → OBLIGATIONS/DECISIONS): TASK 5 must reuse admitPost
  as the engine's decision core (it returns undecided on infra failures
  precisely so codes are never persisted for outages) and must gate events
  from deleted accounts. TASK 6: derive postv2 rkeys deterministically from
  the submission fingerprint so PDS retries become idempotent (closes the
  lost-response duplicate asymmetry documented in §4.2); the write-path
  flip re-triggers the bypass security review. TASK 7 unchanged obligations.
  PRODUCT QUESTION for Bretton: comments bypass admission entirely — a
  banned author can still comment (PRD open question #2); decide whether
  bans should gate comments before Beta.
- (2026-08-08, task 4) RESOLVED in-task, no issue needed: comment edit's
  dead swap-conflict handling FIXED (ErrSwapConflict || ErrConflict) and
  the retried-delete-500 defect FIXED (RecordNotFound name mapping; the
  self-retiring pinned test fired exactly as designed and was retired).
- (task 4 → TASK 5 OBLIGATIONS, additive to task-2/3 lists): wire
  social.coves.community.acceptance + removal into consumerWantedCollections
  FIRST — the engine's catch-up stamp covers the stranded-pending hole but
  the firehose consumer is still the authority; drive the engine via a
  LEASELESS queue (safe only because every write is idempotent — documented
  in engine.go); serialize the queue per community DID (swapCommit is
  repo-global — sibling workers on one busy community starve removals);
  classifyRecordDiff ships PURE+UNCALLED — task 5 invokes it with old/new
  event snapshots AND applies the §5.5 bridge-trust gate; §8 edit-debounce
  belongs to the queue driver; no production AdmissionDecider/
  CommunityRepoFactory exists yet — task 5/6 wire them, factory MUST fail
  closed on unhosted communities (DID-mismatch guard is tested).
- (task 4, suite-health findings fixed at root): invalid lexicon fixtures
  must carry EXACTLY ONE violation (map-order coin-flip otherwise —
  tribunal-vote fixture repaired); T2 waits that can legitimately run long
  need poll cadences whose 100/min-bucket arithmetic outlasts
  contractBudget (comment contract's parent-post wait moved to 600ms after
  5 consecutive cliff failures at 23.97s-measured healthy latency). WATCH:
  posts-consumer drain latency under full-suite parallel load is ~20s+ —
  if another contract trips the cliff, revisit contractPollInterval
  systemically and profile the consumer (backlog candidate).
- (task 4 locked decisions): applyWrites writers are STATE-SHAPED (no
  upsert/tolerant-delete in the PDS — read both rkeys, shape create/update/
  delete per presence, swapCommit-guard the read-then-write); validate:false
  on applyWrites (unpublished lexicons fail validate:true); acceptance
  re-fire must not mint a new CID (skip-if-already-pinned / reuse
  createdAt); engine = ProcessAdmission one-row contract, no lease (safe
  ONLY because every write is idempotent — documented); optimistic
  ApplyAcceptance with commit rev = optimization, firehose is authority,
  own-echo skipped_stale = success; diff classification ships PURE +
  UNCALLED in task 4 — TASK 5 invokes it with old/new event snapshots and
  drives the engine via a leaseless queue.
- (task 3, backlog candidates — /file-issue if they survive the loop):
  registered-aggregator limiter is fail-open (RecordAggregatorPost failures
  logged-only, non-atomic count) — pre-existing; IsAggregator lookup
  failure downgrades to user class (documented, stricter-path fallback);
  post_submissions + aggregator_posts sweeper/retention (§8).
- (2026-08-08, task 5 → DEFERRED DECISION, owner input wanted): the §5.5
  bridgedStats REPIN path is wired nowhere — classifyRecordDiff and
  RepinAcceptedCID ship pure+uncalled. Consequence: once bridges write
  postv2, every stats refresh strobes the accepted post through
  pending_reacceptance + a fresh acceptance record (the exact churn §5.5
  exists to prevent). SAFE TODAY: no postv2 bridged traffic exists until
  tidepool adopts postv2 (post-cutover). Blocker for wiring it: the
  consumer keeps no old-record snapshot, and reconstructing from columns
  is lossy in exactly the way the classifier's fail-closed doc forbids —
  needs a design decision (stored record snapshot vs PRD open question #3's
  separate bridged-stats record, which would dissolve the problem
  entirely). DECIDE BEFORE tidepool's postv2 migration; candidate homes:
  task 6 (touches record shapes) or a named follow-up with open question
  #3. Also carried: credential force-renew gap (revoked tokens defer
  forever — needs a communities.Service method); removal-DELETE events are
  logged no-ops (pair-PUT-outranks argument, review-flagged).
- (2026-08-08, task 5 gate saga — ROOT CAUSE, evidence-backed): the e2e
  starvation flood = TWO composing PRE-EXISTING production defects, fixed
  in-task: CreateCommunity omitted `handle` from the profile record
  (egress-blocked PLC lookup → handle.invalid → UNIQUE collision) and the
  community consumer swallowed ErrHandleTaken as idempotent-replay →
  T1-created communities silently dropped → 72 of their records
  dead-lettered transiently at 4.2s inline head-of-line blocking each ≈
  5min posts-lane outage spanning T2. The defect was ALREADY DOCUMENTED
  in community_contract_test.go:81-95 as reported-not-worked-around.
  Task 5 tripled the poison (4 collections on one lane), making a
  marginal latent failure deterministic. Relay/cursors/load exonerated.
  FORENSIC GAP filed for suite health: the reliability suite recreates
  the appview container late in make ci, so .ci-out/appview.log loses
  the entire early-run window — capture continuously (backlog candidate).
- (2026-08-08, deps): sync.getRecord CAR verification pulled in
  github.com/ipld/go-car pinned to indigo's exact version WITHOUT `go mod
  tidy` — tidy upgrades transitives into a broken github.com/ipfs/go-log.
  Do not tidy this module until that upstream resolves; note for task 8's
  cleanup pass.
- (2026-08-08, task 6 → HARD DEPLOY GATE, security-mandated): the write
  flip REMOVES the credential barrier that gated who could put a post into a
  community (that IS the feature). Pre-task-7 reads are status-agnostic, so
  on this branch any authenticated user can write a postv2 naming ANY
  community (banned-from/private/unrelated) and it renders as that
  community's content immediately, regardless of the engine's acceptance
  decision. CLOSED completely by task 7's visibility predicate. Loop-safe
  (whole branch merges to main once, at task 8 — never task-6-alone). BUT:
  task 6 MUST NOT deploy to prod ahead of task 7. Bypass enumeration itself
  is CLEAN — the firehose path independently re-derives full admission +
  quota for any direct-PDS write; no gate is bypassable.
- (task 6, deferred to task 8 with pointers): fingerprint retype to
  PostV2Record (byte-stability makes re-materialization the free moment);
  the community.post delete branch + blobOwnerOf community-fallback
  scaffolding; RecordAggregatorPost double-meter on converged retry
  (backlog); §8 edit-debounce now reachable at volume via the new update
  path (backlog, task-4-originated).
- (2026-08-08, task 6 → TASK 7 OBLIGATIONS, now with security teeth): the
  centralized visibility predicate must gate EVERY read surface on
  community_post_admissions (security §6: post_repo.go + all feed queries
  currently reference it NOWHERE outside getStatus) — this is the
  compensating control for the deploy gate, not just a feature. Also:
  blob_transform's blobOwnerOf per-record owner is LIVE (postv2→author,
  legacy→community) — task 7's hydration must honor it, and AuthorView.PDSURL
  is now carried out of the scan (was dropped). getStatus↔post.get admission
  convergence per task-5 note. SELF-HOSTER config surface (GREEN flag): the
  SSRF fix silently drops thumbnails whose unfurl targets resolve to private
  addresses (internal wikis / same-network services); IS_DEV_ENV is the only
  escape hatch — a narrower per-host allowance is a deliberate config-design
  task if self-hosters need it (backlog, owner decision).
- (harness, multi-agent stacks): ONE coves-ci compose-project runner at a
  time — a conductor ci run and an agent test-e2e run collided (force-
  recreate mid-run → phantom dead-letter floods + starved lanes). The
  conductor owns stack runs; agents request them.
- (2026-08-08, task 5 → TASK 6/7/8 OBLIGATIONS): TASK 6 — write-path flip
  re-runs the bypass security review; deterministic client-chosen rkeys
  from the submission fingerprint (idempotent PDS retries); the old
  community.post author-not-found transient burn decision; sync fast path
  'pushes work at the engine' claims become true here. TASK 7 — getStatus
  ↔ post.get admission-context convergence (getStatus rationale notes the
  hydrating-join blindspot); §9's re-scoped T2 arcs land with the read
  paths. TASK 8 — drop deprecated community.post collection + its marker;
  orphan community-admissions sweep (re-deferred from task 2/5); go mod
  tidy hazard; forensic appview-log continuous capture (backlog).
  BACKLOG (owner-visible): per-source-DID token bucket on the ingestion
  lane (remaining ~4.2s transient stall primitives: unknown-community
  events); handle-squatting primitive now LOUD but not closed; -race
  suite needs batching if it ever joins the gate.
- (harness) pr-review-toolkit agents unregistered this session →
  /second-opinion runs general-purpose stand-ins with specialty briefs
  (worked well). Named TDD agents spawn in mailbox mode — gate on their
  idle notification, and commit RED's work at the RED gate so GREEN
  tampering is mechanically diffable (mtime check used in task 1).

- (2026-08-07) Loop scaffolded. Owner decisions locked in PRD rev 2.1:
  new NSID `social.coves.community.postv2` (not feed.post — hierarchy kept
  deliberately); `post.notify` + service auth deferred to Beta follow-up;
  lexicons are PUBLISHED — never edit `community.post`'s schema, deprecation
  note only.
- /tdd model check (2026-08-07): skill default is opus (= Opus 5 here);
  `sonnet`/`haiku` exist only as explicit downgrade modifiers — never pass
  them.
- Known adjacent defect (pre-existing, do not fix inline): community blocks
  indexed but never enforced (issue 2026-07-29) — task 7's inventory will
  touch the read paths where this surfaces; keep it filed, don't scope-creep.
- Kagi/aggregator posts are the bulk of prod data; task 8's script leans on
  the aggregator's stored OAuth session (migration 025) for re-authoring.
