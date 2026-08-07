# Author-owned posts — build-loop plan

**Spec: `docs/PRD_AUTHOR_OWNED_POSTS.md` (rev 2.1). This file is the
implementation partition + loop protocol; the PRD is the design authority.
Where they disagree, the PRD wins — fix the PRD first, then the code.**

Tracker: `loop_state.md` (living state, one row per task).

## Branch strategy

One **integration branch**, `feat/author-owned-posts`, cut from `main` at
loop start. Each task runs on its own short-lived branch cut from the
*current integration head* (the /tdd worktree provides this isolation),
and folds back in at iteration end:

```
main ──────────────────────────────────────────────▶ (single /merge-to-main
  └─ feat/author-owned-posts ──M1───M2───M3─── … ──▶  after task 8's panel)
       └ tdd/task-1 ──╯   │    │
            └ tdd/task-2 ─╯    │
                 └ tdd/task-3 ─╯
```

- Merges into the integration branch are **`--no-ff`** — one merge commit
  per task keeps the task boundary in history (the audit trail the
  stacked-branch alternative would have provided, without its rebase
  ceremony — the tasks are strict sequential dependencies, so true
  stacking buys nothing here).
- Task branches + worktrees are deleted after their merge (the "reset").
- `main` is touched exactly once, at the very end, via `/merge-to-main`
  after task 8's final review panel. `make ci` is the merge gate at BOTH
  levels: before every integration-branch merge, and again at the final
  merge to main.

## Loop protocol (one iteration per task)

1. **Analyze** (parent, persistent context): read `loop_state.md`, pick the
   first non-done task, write a **self-contained brief** — scope, files,
   PRD sections, relevant cross-iteration notes, exit criteria, and the
   task's acceptance behavior (the outer BDD test /tdd will frame).
2. **`/tdd <brief>`** — the whole implementation happens inside the tdd
   skill: parent as CONDUCTOR, outer acceptance test, inner red/green/
   refactor cycles, RED author and GREEN implementer as **separate
   persistent Opus 5 agents** (the skill's default model is opus — NEVER
   pass the `sonnet`/`haiku` modifiers). Worktree on a task branch cut
   from the integration head.
3. **`/second-opinion`** on the completed task, scoped to the task diff —
   tell it the base explicitly: `git diff feat/author-owned-posts...HEAD`.
   pragma:security fires automatically on trust-boundary diffs; task 6
   (auth/credential surface) must include it — verify it fired, don't
   assume.
4. **`/fix-pr`** with the synthesized review report pasted in. Re-run the
   tdd suite after fixes land (fix-pr's own go vet/build check is not the
   gate).
5. **Verify**: full `make ci` in the task worktree. Green or it doesn't
   merge — red means fix now, or mark the task `blocked: <reason>` in
   loop_state and stop the loop for user input.
6. **Merge** the task branch into `feat/author-owned-posts` (`--no-ff`,
   merge commit titled `task N: <name>`), delete the task branch and
   worktree.
7. **Update `loop_state.md`** (status, merge commit, notes — surprises and
   decisions go to Cross-iteration notes) and schedule the next iteration.

Statuses: `pending → in-tdd → review → done` (or `blocked: <reason>`).
Stop when every task is done or on any block.

Standing rules (carried from the test-refactor loop):
- A skip is a failure; missing infra is a `t.Fatal` naming the target.
- Subscribe-before-write cursor pattern for every firehose wait; no sleeps.
- Test handles: suite canonical helpers only (`uniqueTestID`/`uniqueAccount`).
- Defects found in *existing* code that are out of task scope: /file-issue,
  don't fix inline, note in loop_state.
- Briefs to any agent are self-contained (file:line, failure scenario,
  expected behavior) — no agent reads this file's history to reconstruct
  context.

## Task partition (each row ≈ one PR-sized /tdd run)

Sizing rationale: every task leaves `make ci` green. The contract-manifest
gate forces consumers and their e2e contracts to land together (task 5); the
read-path filter (task 7) lands *after* the write flip (task 6) so
status-agnostic reads keep serving both old and new posts during the middle
of the loop.

### Phase A — foundations (no behavior change)

**1. Lexicons + fixtures**
PRD §3. New `internal/atproto/lexicon/social/coves/community/postv2.json`
(no `author`, `community` immutable-on-update documented),
`community/acceptance.json` (key `any`, deterministic-rkey doc, strongRef
subject), `community/removal.json` (open `knownValues` code + maxLength 64).
Deprecation note in `community/post.json` description ONLY (schema
untouched — it's published). Fixture tree: `tests/lexicon-test-data/postv2/`
valid+invalid, `acceptance/`, `removal/`; extend the record-lexicon
validation T0 tests. Acceptance behavior for the outer test: the three new
lexicons validate their valid fixtures and reject each invalid fixture with
the expected error. Exit: T0 green; no consumer references yet.

**2. Migration 034 + admissions repository**
PRD §6.1, §5.3. `034_author_owned_posts.sql`: `community_post_admissions`
(PK (community_did, post_uri), status, acceptance_uri/rkey, accepted_cid,
decision_code, decision_at, evaluated_cid, redrivable, last_community_rev,
partial index on accepted); drop `posts` FK to users + CASCADE (soft ref).
`internal/db/postgres` admissions repo: upserts, status transitions,
watermark compare-and-set. Acceptance behavior: the T1 transition matrix —
stale-rev events are no-ops, same-rev removal wins, every legal/illegal
status transition. Exit: T1 green.

**3. `admitPost` extraction + real admission policy**
PRD §4.1, §8. Extract CreatePost's checks into a shared `admitPost`
(community exists, visibility, aggregator authz) and ADD the new policy:
ban lookup against indexed ban state, per-author/per-community submission
rate limits, dedupe by (author, community, content CID) — each with typed
error → decision code. Wire into the EXISTING write-forward path (bans
start being enforced now; that's intended policy, call it out in the merge
commit). Acceptance behavior: a banned author's create is refused with the
ban code end-to-end on the current path. Exit: T0 matrix covers every
branch; T1 for ban/rate lookups; e2e still green.

### Phase B — the new machinery

**4. Acceptance engine + community-repo writers**
PRD §3.2, §3.3, §5.5, §5.6. Deterministic rkey helper (subject AT-URI →
rkey-safe transform) with T0 round-trip tests; acceptance writer
(putRecord + swapRecord conflict path); atomic acceptance-delete + removal
via `com.atproto.repo.applyWrites`; engine core: consume a
pending/pending_reacceptance admission → admitPost → write/update
acceptance | record rejection (AppView-local, redrivable=false) | atomic
remove. Removal terminality + bridgedStats-only repin decision function
(pure, T0). Acceptance behavior: engine double-fired on the same subject
converges to exactly one acceptance record (idempotence), proven against
the real PDS container at T1. Exit: T0/T1 green; nothing triggers the
engine in prod paths yet.

**5. Ingestion: postv2 + acceptance + removal consumers, contracts**
PRD §5 entire. postv2 handler in `PostEventConsumer` (author = event.Did,
community from record + immutability enforcement, unknown-author soft
handling + opportunistic SSRF-safe identity hydration, pending admission
row, edit → CID compare → pending_reacceptance, delete → tombstone);
acceptance/removal handlers (community-repo authority check, §5.2 watermark
gating, acceptance-before-post → CID-verified direct PDS fetch with
SSRF/size/time caps, DLQ backstop); engine triggered for hosted
communities; BridgeTrust re-keyed to author repo.
`consumerWantedCollections` gains the three collections; THREE
`//coves:ingestion-contract` e2e contracts. Acceptance behavior (the outer
test IS the primary contract): postv2 written to an author repo on the
hermetic PDS → firehose → pending → auto-accept → accepted; plus banned
author never accepted; edit → re-accept; removal → atomic commit observed.
Old `community.post` consumer path stays live in parallel. This is the
hardest task — the parent may split it 5a (consumers) / 5b (contracts) at
brief-writing time, but WantedCollections + contracts must land in the
same merge. Exit: full `make ci` green.

### Phase C — the flip

**6. Write path flip** *(review must include pragma:security)*
PRD §4. CreatePost: session-explicit signature
(`*oauth.ClientSessionData`, comments' `PDSClientFactory` pattern), write
postv2 to author repo, blobs/thumbnails to author PDS (blob service gains
author BlobOwner path), aggregator stored-token path, sync fast-path
acceptance via engine, no rollback of author records on acceptance
failure. `post.delete` → author-session delete. `post.update` NEW endpoint
(route, handler, service, swapRecord conflict handling). Update e2e flows
that create posts. Acceptance behavior: a user's create lands in THEIR
repo, is accepted synchronously, and round-trips through the feed; an
acceptance-write failure leaves the author record intact and pending.
Exit: full `make ci`.

**7. Read path: centralized visibility**
PRD §6.2, §3.4. One admission-aware predicate (view or shared query
helper); convert the inventory: community feeds ×4 + communityFeeds,
post.get (+ `#removedPost` union + admission context in postView),
getComments post hydration, actor surfaces, search, embed hydration,
counts/stats. `getStatus` query endpoint. Author self-view semantics.
Acceptance behavior: pending/removed posts are unreachable through EVERY
alternate endpoint (comments, search, counts) for non-authors, while the
author still sees status. Exit: full `make ci`.

### Phase D — cutover

**8. Drain, cleanup, cutover tooling** *(final panel reviews the whole branch)*
PRD §10. Re-materialization script (`cmd/` tool): per-post ledger,
resumable/idempotent, write postv2 to author repo → acceptance → verify
both + pinned CID → checkpoint → delete old community-repo record; tested
hermetically against the e2e stack with seeded old-style data. Remove: old
`community.post` from WantedCollections + its consumer branch +
community-credential post writes + `author`-field handling; old fixtures.
`federation-prd.md` superseded-by header. Docs pass over the PRD (mark
implemented, log divergences). Delete `plan.md` + `loop_state.md` before
the final merge (build-loop trackers, not suite artifacts — precedent:
`512e00e`). Acceptance behavior: the script, run twice against a seeded
hermetic stack, converges with a complete ledger and zero old-style
records. Exit: full `make ci`; task's /second-opinion runs on the WHOLE
branch diff vs main; then `/merge-to-main`. Prod execution of the script
is MANUAL, outside the loop, coordinated with coves-mobile/coves-frontend
URI-parsing updates (PRD §10.2).

### Deferred (NOT loop tasks)

- **Beta remote path** (PRD §7): `post.notify`, service-auth middleware
  (`lxm`/`aud`/replay), DID service entry, local-vs-remote detection.
  Explicitly deferred per owner decision 2026-08-07.
- Private-community admission leakage (PRD §11.4), snapshot dial (§11.1),
  separate bridged-stats record (§11.3).
