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
| 2 | Migration 034: community_post_admissions + posts FK drop; admissions repo + T1 transition/watermark matrix | A | pending | | |
| 3 | admitPost extraction + NEW policy (bans, rate limits, dedupe) wired into existing write path | A | pending | | |
| 4 | Acceptance engine: deterministic rkey, swap-safe acceptance writer, atomic applyWrites removal, repin/terminality rules | B | pending | | |
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
