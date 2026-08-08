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
| 5 | Ingestion: postv2/acceptance/removal consumers, watermark gating, direct-fetch convergence, WantedCollections + 3 e2e contracts | B | pending | | parent may split 5a/5b at brief time; WantedCollections + contracts same merge |
| 6 | Write path flip: author-repo postv2 via session, author-PDS blobs, sync fast-path accept, post.delete flip, post.update NEW | C | pending | | review MUST include pragma:security — verify it fired |
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
