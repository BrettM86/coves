# PRD: Author-Owned Posts + Community Acceptance Records

**Status: DRAFT rev 2 — design agreed 2026-08-01; revised 2026-08-05 after an
external design review (OpenAI Codex `gpt-5.6-sol`, high effort, full repo
access). The review confirmed the two-record trust split and forced five
structural changes: admissions became per-(community, post) relational state
instead of columns on `posts` (§6.1), cross-record ordering got a subject-scoped
watermark and atomic community-repo commits (§5.2), a `rejected` decision state
now actually exists (§6.1), removal is defined as terminal across author edits
(§5.5), and the spec stops claiming today's write path enforces bans — it does
not; ban enforcement is new scope (§4.1).
Rev 2.1 (2026-08-07): corrected for the fact that the lexicons ARE published —
the flipped post record moves to a NEW NSID (`social.coves.community.postv2`) instead
of breaking `social.coves.community.post` in place (§3.0/§3.1); lexicon
details audited against the atProto Lexicon spec, record-key spec, and the
draft Lexicon style guide (bluesky-social/atproto discussion #4245) — open
value sets confirmed, deterministic rkeys specified per the record-key spec's
"(transformed) AT URI" pattern.
Rev 2.2 (2026-08-07): rkey derivation switched from readable URI transform to
SHA-256/base32 digest (review catch: transform non-total over legal DID
space).
Rev 2.3 (2026-08-07): §5.2 watermark became a composite (rev, op-rank) tuple
CAS — task-2 plan review caught that a plain rev watermark with removal-wins
special-casing silently dropped moderator restores (the restore commit is
symmetric to the removal commit and indistinguishable from ordinary events on
the wire); restore is now defined as any community acceptance winning the
tuple CAS over a removal. Stale/terminal skips are outcome values, not
errors (033 precedent — sentinels would dead-letter healthy skips).
Rev 2.4 (2026-08-08): rejection narrowed to pending-only CAS with judged CID;
op-rank derived repo-side; NULL-evaluated acceptance treated as pin-trusting
(task-2 second-opinion catches).
Rev 2.5 (2026-08-08): §4.1 corrected by task-3 plan review — ban source is
community_memberships.is_banned behind a BanLookup interface (no
moderation.ban ingestion exists; no production ban writer yet); rate
limits/dedupe get a synchronous post_submissions ledger (migration 035) —
the posts table is unusable as a limiter substrate (ingestion lag,
author-supplied created_at, delete-to-evade); per-origin-PDS quota
explicitly deferred to Beta.
Rev 2.6 (2026-08-08): task-3 second-opinion — fingerprint normalized to
resolved-DID scope, release decoupled from request context, admission wiring
fail-loud, ActorClass fail-closed.
Rev 2.7 (2026-08-08): task-5 plan review — post.getStatus pulled forward into
task 5 as the T2 observation surface (unauthenticated; mild disclosure of
rejected-post status accepted, owner-flagged); hosted-community detection =
community credential presence, NEVER hosted_by_did (attacker-controlled for
firehose-indexed communities); deleted_accounts marker table (migration 036 —
account deletion previously left no marker, so swept admissions could be
recreated by replayed events); author-delete resurrection loop closed (driver
excludes tombstoned posts; decider refuses them); §5.1 keeps the deprecated
community.post collection subscribed until task 8's drain; §9's T2 list
re-scoped — accepted-state arcs prove at T1 (no T2 community holds
credentials), T2 contracts assert consumer semantics via getStatus.**

**Supersedes** the write-path architecture in `docs/federation-prd.md`: that
document solves cross-instance posting by service-auth-forwarding the write to
the community's host, which still signs the post into the community repo. This
PRD removes the need for any server to write posts on behalf of another
server's users. The service-auth *transport* from federation-prd is retained
for the notify endpoint, but its specification was hypothetical and is
re-specified concretely in §7.

---

## 1. Problem

Posts are the only user-authored record type that lives in the **community's**
repo. Comments (`internal/core/comments/comment.go:14`) and votes
(`internal/core/votes/vote.go:8`) already live in the **author's** repo and are
indexed from the firehose. Posts are write-forwarded: the AppView validates,
then uses the community's stored PDS credentials to sign the author's words
into the community repo (`internal/core/posts/service.go` `CreatePost`).

This causes three structural problems:

1. **Cross-server posting is impossible.** A user on Coves-canonical cannot
   post into a community hosted on Coves-selfhosted: their server doesn't hold
   that community's keys, and never should.
2. **Authorship is unverifiable.** The `author` field inside the post record is
   an unsigned claim by whoever holds the community keys. Any community host
   can fabricate posts by any user.
3. **Forks orphan the interaction graph.** A community fork is a new DID, so it
   must re-sign every post → new AT-URIs/CIDs → every comment and vote in every
   user repo still points at `at://old-community-did/...`. A fork today gets
   post husks with no comments and no scores.

## 2. Design summary

**The user's speech is signed by the user; the community's acceptance of that
speech is signed by the community. Two facts, two records, two repos.**

- The post record moves to the **author's repo** (same placement as comments
  and votes), as the new lexicon `social.coves.community.postv2` (§3.0 — the old
  published NSID is deprecated, not broken). The repo signature becomes the
  authorship anchor; there is no in-record `author` field.
- A new community-authored record, `social.coves.community.acceptance`, is a
  strongRef (URI + CID) to an author's post. It is written **automatically by
  the community's host** — no human in the loop — after admission checks pass.
  For an open community and a non-banned author this happens in milliseconds.
  Acceptance is machine attestation, not human approval.
- **Community surfaces render exclusively from admission state derived from
  acceptance records.** A post *claiming* a community but lacking an acceptance
  is never shown in that community, by lexicon-documented convention. (Same
  trust shape as Bluesky threadgates.)
- Moderation removal = delete the acceptance record **and** write a
  `social.coves.community.removal` record carrying an error code, in **one
  atomic `com.atproto.repo.applyWrites` commit** (§5.5). Removal is a signed,
  portable, auditable act that survives migration and forks.

Policy on paper is unchanged — anyone can post in any public community unless
banned — but note honestly (§4.1): today's code does not yet enforce bans or
per-user rate limits, so the acceptance engine *introduces* those checks
rather than relocating them.

**Verification boundary (do not overclaim).** "Author-signed" holds at the repo
layer: the record lives in a commit signed by the author's signing key. Our
ingestion path does not itself verify commit/MST proofs — Jetstream events
carry no proof material. The trust model is therefore: records are
author-attributed **as vouched for by a relay that verifies repo commits** (our
self-hosted relay does) **or by a direct, DID-resolved PDS fetch** (§5.4).
Consumers that ingest from unverified sources get repo-origin attribution, not
cryptographic proof. Document this in the lexicon descriptions; never describe
firehose events themselves as signed.

### Why this preserves (and improves) the original goals

- **Portability**: the community CAR carries the *curated index* — every
  acceptance, removal, ban, rule, pin — instead of other people's prose.
  Content liveness depends on author PDSs, exactly as it already does for
  every comment thread. A snapshot dial (embedding content in acceptance
  records) exists as a v2 option; v1 is pointer-only.
- **Forkability**: post URIs are `at://author-did/...` — they belong to no
  community. A fork writes its own acceptance records pointing at the same
  posts and the entire comment/vote graph stays attached. This requires
  admission state to be **per-(community, post)**, not per-post — a post must
  be able to hold independent admission decisions from multiple communities
  (original + forks). §6.1 models exactly that; the post's `community` field is
  only its *initial submission target*, and cross-community acceptance is the
  privileged fork/import flow (§10.2).
- **Ownership across DIDs**: the community DID still owns everything
  collective — curation, membership, bans, rules, governance outcomes. It
  stops owning users' words, which was a liability (impersonation power,
  hosting liability, deletion obligations), not an asset.

---

## 3. Lexicon changes

### 3.0 Evolution strategy — the lexicons are published

The `social.coves.*` lexicons are published, so the atProto evolution rules
apply: non-optional fields cannot be removed, types cannot change, and
"larger structural changes require creating new Lexicons with different
NSIDs." Flipping a record's home repo is the largest structural change there
is — and keeping the old NSID would be actively dangerous, not just
rule-breaking: any consumer built against the published
`social.coves.community.post` derives **community = repo DID** for that
collection. Fed the same collection name from *author* repos, it would
silently index authors as communities. A new NSID makes stale consumers
ignore the new records entirely, which is the correct failure mode.

Therefore:

- The author-repo post record is a **new lexicon,
  `social.coves.community.postv2`** — same namespace hierarchy as today
  (deliberate owner choice over relocating to `social.coves.feed.*`; the
  safety property comes from the NSID being *new*, not from where it sits).
- `social.coves.community.post` (the record) is **deprecated in place**: its
  published schema is untouched except a description marking it deprecated
  and pointing here. No new records are ever written to it; §10's migration
  drains it.
- `social.coves.community.acceptance` and `social.coves.community.removal`
  are brand-new NSIDs — no evolution constraints.
- The XRPC procedures/queries (`social.coves.community.post.create`, `.get`,
  `.getStatus`, `.notify`) keep their NSIDs: the client contract evolves
  additively (new optional output fields, new union members), which the
  rules permit. A `community.post.*` endpoint family writing
  `community.postv2` records is fine — endpoints are verbs, records are
  nouns.

### 3.1 `social.coves.community.postv2` (new — author repo)

New file `internal/atproto/lexicon/social/coves/community/postv2.json` — the §3.0
successor to `social.coves.community.post`, with these deltas from the
deprecated schema:

- **Placement**: author's repo (joins `social.coves.feed.vote` and
  `social.coves.community.comment`, which already live there).
- **No `author` field** — the repo DID *is* the author. Consumers MUST derive
  authorship from the event DID.
- **Keep** `community` (required, `format: did`) — the initial submission
  target. **Immutable across updates**: an update event that changes
  `community` is invalid and MUST be ignored by consumers (retargeting a post
  is a new post record). This prevents dangling admissions and matches the
  existing update-immutability conventions in the consumer.
- **Keep** everything else: title, content, facets, embed union, langs, labels,
  tags, crosspostOf/crosspostChain, createdAt, bridgedStats.
- `crosspostOf`/`crosspostChain` strongRefs now point at author-repo URIs —
  stable across community forks (previously broken).

### 3.2 `social.coves.community.acceptance` (new — community repo)

New file `internal/atproto/lexicon/social/coves/community/acceptance.json`.
As specced in rev 1 (strongRef `subject` + `createdAt`, community implicit in
the repo), with two hardening changes from review:

- **Record key: deterministic, not TID.** `key` is `any`, and the rkey is the
  unpadded lowercase base32 encoding of the SHA-256 digest of the canonical
  subject AT-URI — a fixed 52 characters, always within the 512-byte rkey
  limit and always drawn from the rkey-safe charset. Why a digest instead of
  a readable URI transform: external review caught that DIDs may legally run
  up to 2048 bytes and may contain percent-escapes, so the readable transform
  (strip `at://`, swap `/` for `:`) is non-total over the legal DID space —
  a fixed-size digest is total and still deterministic. The record-key spec
  explicitly blesses `any` for exactly this — "de-duplication and known-URI
  lookups" via "a (transformed) AT URI" — and Bluesky's `threadgate` (rkey
  must equal the subject post's rkey) is precedent for subject-derived keys.
  One post →
  one acceptance rkey per community, forever. This makes the three
  independent acceptance writers (sync fast path §4.3, firehose engine §5.6,
  notify §7) **idempotent by construction** — concurrent attempts converge on
  `putRecord` of the same rkey instead of allocating duplicate TIDs, and
  re-acceptance after an edit is an update of the same record with a new
  subject CID (references to the acceptance URI stay valid).
- Writes use `swapRecord`/`swapCommit` so a lost race is a detected conflict,
  not a silent overwrite.

The `subject` strongRef pins the accepted CID: if the author edits the post,
the CID no longer matches and the post is pending re-acceptance — clients and
AppViews MUST NOT auto-render the new CID under the old acceptance (§5.5).

### 3.3 `social.coves.community.removal` (new — community repo)

As specced in rev 1 (strongRef `subject`, required `code`, optional `reason`,
`createdAt`), plus:

- `code` uses **`knownValues`, which is an open set by definition** — "values
  are not limited to this set" — so new codes can ship without a lexicon
  break. This is deliberate, per the draft Lexicon style guide's "enum sets
  are closed … should almost always be avoided." Never convert it to `enum`.
  Values are kebab-case (style-guide convention for fixed strings):
  `rule-violation | spam | off-topic | illegal-content | author-banned |
  moderator-discretion`. Add `maxLength: 64` so unknown codes are still
  bounded.

- Same deterministic-rkey scheme as acceptance (one removal record per
  (community, post); re-removal updates it).
- **Removal is URI-scoped and terminal** (§5.5): it applies to the post URI,
  not to the CID it happened to pin. The pinned CID is audit metadata.
- Written in the **same `applyWrites` commit** that deletes the acceptance, so
  the firehose never carries a half-completed moderation action.

Rejection at submission time (never accepted) writes **no record** — spam must
not bloat the community repo. Rejection state is AppView-local (§6.1) and
queryable via §3.4.

### 3.4 XRPC surface

- `social.coves.community.post.create` — kept as the client-facing procedure;
  semantics per §4. **The service signature changes**: like comments'
  `CreateComment`, it takes the caller's OAuth session
  (`*oauth.ClientSessionData`) explicitly — no hidden context coupling — and
  the aggregator path passes stored-token credentials instead
  (`internal/core/posts/interfaces.go` and the handler change accordingly;
  review confirmed the current interface takes no session at all).
- `social.coves.community.post.update` — **new scope, not a flip.** The route
  is currently commented out (`internal/api/routes/post.go:54`) and the
  service method doesn't exist. It ships in this change as an author-session
  write with `swapRecord` conflict handling, because the edit → re-acceptance
  lifecycle (§5.5) is core to the design and must be testable.
- `social.coves.community.post.get` — `postView` gains admission context
  (status + acceptance URI for the viewed community); union gains
  `#removedPost` (mirrors `#notFoundPost`/`#blockedPost`, carries the removal
  `code`). URI normalization flips to author-DID-based.
- `social.coves.community.post.getStatus` — new query on the community host:
  given post URI (+ community), returns
  `accepted | pending | pending_reacceptance | rejected | removed` plus code
  and decision time, read from the admissions table (§6.1).
- `social.coves.community.post.notify` — new procedure on the community host
  (Beta, §7).

---

## 4. Write path (`internal/core/posts/service.go`)

### 4.1 What today's checks actually are (correcting rev 1)

Review against the code: the current user flow enforces **community existence,
private-visibility block, embed/thumb validation, and aggregator
authorization + rate limits — nothing else**. The docstring's
"membership/ban validation" is aspirational; there is no ban lookup
(`ErrBanned` is marked Beta in `errors.go`) and no per-user rate limiting.

Therefore `admitPost` (§5.6) is **extraction plus new policy**, not a
behavior-preserving refactor. New checks arriving with it, each with an
explicit error code and tests: ban enforcement, per-author/per-community
submission rate limits, and duplicate-submission dedupe (§8).

**Ban source, honestly (task-3 plan-review correction):** the only ban state
in the system is `community_memberships.is_banned` — and no production code
path writes it today (the memberships repo has no non-test callers; no
`social.coves.moderation.ban` consumer exists and the collection is not in
`consumerWantedCollections`). `admitPost` therefore enforces bans through a
`BanLookup` interface backed by that column, making enforcement live the
moment a ban writer ships (moderation write path and/or ban-record
ingestion — future scope, not this loop). Non-membership reads as
not-banned; any lookup FAILURE fails the request closed — failing open on a
ban would turn a database blip into a global unban.

**Rate-limit substrate:** limits and dedupe are backed by a synchronous
`post_submissions` ledger (migration 035, mirroring `aggregator_posts`) with
a canonical-record fingerprint and a UNIQUE-insert dedupe gate,
reserve-then-confirm around the PDS write. The `posts` table cannot back
them: it is firehose-fed (ingestion lag hides the very burst being limited),
its `created_at` is author-supplied (attacker-controlled windows once writes
flip to author repos), and its indexes exclude soft-deleted rows
(delete-to-evade). Refused submissions consume no quota. Dedupe precedes the
rate limit (a client retry storm must not burn quota) and applies to every
actor class; trusted aggregators keep their historical no-limit status and
registered aggregators are governed by their existing limiter only.
§8's per-origin-PDS quota is **deferred** to the Beta remote path (it
requires PDS resolution, §7) — recorded here so §8 does not silently become
fiction.

### 4.2 Flow

1. Validate input, URI normalization, DID auth check — unchanged, order
   preserved, fail-fast before any repo write.
2. **Blob handling flips to the author's PDS** under the author's session
   (comments' `PDSClientFactory` + OAuth/DPoP pattern). The blob service
   currently uploads with a community `BlobOwner`; it gains an author-repo
   owner path. Embedded media is owned by the author's DID and PDS, as comment
   embeds already are.
3. **Post record is written to the author's repo** via the author's session.
   Aggregators (Kagi) write to their own repos via their stored OAuth tokens
   (migration 025) — unchanged in shape.
4. **Local-community fast path**: this AppView hosts the community, so it *is*
   the authoritative admission engine — it runs `admitPost` synchronously and
   writes the acceptance (deterministic rkey, §3.2) via the existing
   credential machinery (`EnsureFreshToken`). Client gets URI + CID back with
   the post already accepted; UX identical to today.
5. **Remote-community path (Beta)**: the author's server does **not** run
   admission checks — it has no authoritative view of a remote community's
   bans, visibility, or quotas, and a stale or hostile home server must not be
   able to fake either an admission or a rejection. Author-side
   responsibilities are only: syntactic validation, authentication, and the
   author-repo write. The post is `pending` until the community host decides
   (§7); the client learns the outcome via optimistic self-view + `getStatus`.

Failure mode: author-repo write succeeds, acceptance write fails → post stays
`pending`; the firehose engine (§5.6) retries idempotently (same rkey).
Degraded latency, not data loss. Never roll back the author's record.
There is a lost-response asymmetry here: when the PDS write's outcome is
ambiguous (the record may or may not exist) and the submission reservation is
released, a client retry can produce a duplicate post — the remedy, noted for
task 6, is to derive the record rkey deterministically from the submission
fingerprint so retries become idempotent at the PDS layer.

`post.delete` likewise flips to an author-session delete.

---

## 5. Ingestion (`internal/atproto/jetstream/`)

### 5.1 `consumerWantedCollections` (`feeds.go:56`)

```go
ConsumerPosts: {
    "social.coves.community.postv2",
    "social.coves.community.acceptance",
    "social.coves.community.removal",
},
```

The deprecated `social.coves.community.post` collection is dropped from the
subscription — after §10's drain, no live events exist for it, and its delete
events during migration are irrelevant because `posts` is truncated and
re-indexed anyway.

`cmd/contract-manifest` then fails CI until each new collection has a
`//coves:ingestion-contract` marker in `tests/e2e/` — the mechanism that
forces §9's pipeline tests to exist.

### 5.2 Ordering: the per-record rev gate is not enough

Acceptance and removal are **different record URIs about the same subject**,
so the existing per-record rev gate cannot order their combined effect: a
redriven stale acceptance could resurrect a removed post; a delayed acceptance
delete could flip `removed` back to `pending`.

Fix: admission state transitions are gated by a **subject-scoped composite
watermark** — `(last_community_rev, last_community_op_rank)` on the admissions
row (§6.1), where `rank(delete) = 0 < rank(put) = 1`. Every community-repo
event about subject S applies through ONE rule: a strictly-greater tuple
comparison (revs are base32-sortable TIDs, so lexicographic comparison is
commit order — same semantics as migration 033's per-record gate). This
single rule yields, order-independently within each atomic commit:

- **removal wins** inside the removal commit `{acceptance-delete@(R,0),
  removal-create@(R,1)}` — whichever applies second either lands (put ranks
  above delete) or is skipped as not-greater;
- **restore wins** inside the restore commit `{removal-delete@(R2,0),
  acceptance-create@(R2,1)}` — which also means there is NO distinct
  "restore" operation at the event level: a community-authored acceptance at
  a strictly newer watermark than the removal IS the moderator restore, by
  construction (only the community's key holder can write acceptances);
- **exact-duplicate idempotency** — an equal tuple is a replay (multi-feed
  overlap, DLQ redrive) and is a no-op, never re-stamping decision
  timestamps.

A skipped-stale or skipped-terminal event is the system WORKING (033
precedent): repositories report it as an outcome value, never as an error —
an error return would route healthy skips into the dead-letter queue.
Author-repo events (post create/update/delete) keep the existing per-record
rev gate and NEVER advance the community watermark (nor does the AppView-local
`rejected` decision — a local decision must not suppress a genuine community
event). ApplyAcceptance compares the pinned CID against the indexed post CID
(§5.4): match → accepted; mismatch → `pending_reacceptance` with the
acceptance fields still persisted, so an acceptance arriving before its
subject's edit event converges when the post event lands (no livelock). All
handlers are single-statement CAS upserts; replay is a no-op.

### 5.3 Post events (author repos)

- Author DID = `event.Did`. Community DID = the record's `community` field;
  unknown community → dead-letter (redrive resolves the profile race).
- **Unknown authors — the FK must go.** `posts.author_did` currently has a
  hard FK to `users` with `ON DELETE CASCADE` (migration 011), and the
  consumer only bootstraps missing authors from trusted bridge PDSs — the test
  architecture states federated authors cannot currently be indexed. That
  contradicts open federated posting. Changes: drop the FK to a soft
  reference (migration 034), hydrate unknown author profiles opportunistically
  via SSRF-safe identity resolution (extending the existing
  `identityResolver` path beyond bridge trust), and **stop cascading** — a
  deleted profile row must not silently erase indexed posts; author-requested
  deletion is an explicit tombstone (existing comments pattern, migration 021).
- Index/refresh the post row; admission state lives in §6.1, initial status
  `pending` for the target community.
- Update events: §5.5. Delete events: tombstone the post; the community host
  observes the tombstone and deletes its acceptance (removal record optional —
  author deletion is not moderation).

### 5.4 Acceptance/removal events (community repos) — and convergence

- Repo DID must be an indexed community; else dead-letter.
- **Acceptance-before-post does not converge by redrive alone** — bounded
  dead-letter retries cannot manufacture a post event that a relay-coverage
  gap will never deliver. On acceptance whose subject post is unindexed:
  resolve the subject author's DID → PDS, fetch the record directly
  (`com.atproto.repo.getRecord`), **verify the returned CID equals the pinned
  CID**, apply strict SSRF protections and size/time caps, and index it. The
  DLQ remains the backstop for transient failures only. Notify (§7) is a
  latency optimization; direct fetch is what makes firehose-only ingestion
  actually converge. State plainly: without either, convergence requires full
  relay coverage.
- Acceptance apply (subject to §5.2 watermark): pinned CID == indexed post
  CID → `accepted`; mismatch → `pending_reacceptance` (stale acceptance for a
  since-edited post; the engine re-emits).
- Acceptance delete: leaves `accepted`; goes to `removed` if the same-rev
  removal is present (it will be, §3.3), else `pending`.
- Removal apply: `removed` + code. A removal without prior acceptance is
  valid (pre-emptive) and indexes normally.

### 5.5 Edits, re-acceptance, and removal terminality

- Post update where the new CID ≠ accepted CID → `pending_reacceptance`; the
  engine re-runs `admitPost` on the new content and either updates the
  acceptance (same rkey, new pinned CID) or removes with code. Edited content
  is never auto-rendered under the old acceptance.
- **Exception — `bridgedStats`-only updates.** Bridges refresh records
  frequently *specifically* to update origin-platform vote counts; treating
  each refresh as an edit would strobe accepted bridge posts out of feeds and
  spam the community repo with re-acceptance commits. Rule: if the record diff
  touches **only** `bridgedStats` and the author passes the bridge-trust gate,
  repin synchronously (acceptance update, no status change, no feed removal).
  Any diff touching title/content/facets/embed/labels/tags/community requires
  full re-admission. (v2 alternative if this proves noisy: move bridged
  aggregates to a separate author-owned stats record so content CIDs stop
  churning at all.)
- **Removal is terminal across author edits.** `removed` is exited only by an
  explicit moderator restore (atomic commit: delete removal + write fresh
  acceptance — which reaches consumers as ordinary events winning the §5.2
  tuple CAS; there is no distinct restore operation on the wire). Terminality
  is scoped precisely: `removed` is terminal against AUTHOR-repo events and
  against community events at or below the removal's watermark; a
  community-repo acceptance at a strictly greater watermark is the restore.
  An author edit while removed updates audit metadata only (`evaluated_cid`,
  `updated_at` — never status or decision fields) — otherwise editing would
  launder a removed post back through auto-acceptance.
- Bridge-trust re-keying: the `BridgeTrust` gate currently trusts *community*
  repos' PDS provenance for `bridgedStats`; it re-keys to the **author**
  (bridge account) repo's PDS.

### 5.6 Acceptance engine (new component)

The single decision point: inputs are pending/pending-reacceptance admissions
for communities this AppView hosts (from the fast path, the firehose consumer,
or notify). It runs `admitPost` — the extracted §4.1 checks plus the new
ban/rate-limit policy — then writes/updates the acceptance, or (rejection)
records the decision in the admissions table, or (re-acceptance failure /
moderation) performs the atomic acceptance-delete + removal. Rejection is a
pending-only CAS carrying the judged CID: a rejection evaluated against
content the row no longer holds refuses rather than applies, and accepted or
removed rows are never rejected — a failed re-acceptance is a removal per
§5.5, not a rejection. It is the only
writer of community-repo records in the post system, and every write is
idempotent via deterministic rkeys + swap semantics.

---

## 6. Storage & read paths

### 6.1 Migration `034_author_owned_posts.sql`

**Posts stay pure content; admission state is relational.** A post must be
able to carry independent decisions from multiple communities (forks, §2), a
`rejected` state must actually exist somewhere queryable (rev 1 promised it in
`getStatus` but stored it nowhere), and decisions need audit metadata.

New table `community_post_admissions`:

- PK `(community_did, post_uri)`
- `status TEXT NOT NULL` —
  `pending | accepted | pending_reacceptance | rejected | removed`
- `acceptance_uri`, `acceptance_rkey`, `accepted_cid` (NULL unless accepted)
- `decision_code TEXT`, `decision_at TIMESTAMPTZ` — rejection/removal codes;
  rejections are AppView-local (never a community-repo record)
- `evaluated_cid TEXT` — the exact CID the last decision judged
- `redrivable BOOLEAN NOT NULL DEFAULT true` — policy rejections are
  `false` (terminal; not retried by DLQ redrive); transient evaluation
  failures stay `true`
- `last_community_rev TEXT COLLATE "C"` + `last_community_op_rank SMALLINT`
  with a `CHECK` restricting the op-rank to `0 | 1` — the §5.2 subject-scoped
  composite watermark (033 pins `COLLATE "C"` for rev comparison; same rule
  here). The op-rank is derived repo-side from the event's operation
  (delete = 0, put = 1) and is never caller-supplied: an out-of-range rank
  would corrupt the tuple comparison silently
- Partial index on `status = 'accepted'` for feed queries

`posts` changes: drop the `users` FK + CASCADE (§5.3), drop the in-record
author column's trust role (author = repo DID), keep content columns.
Pre-production data: truncate and re-materialize (§9).

### 6.2 Read paths — centralized visibility, full inventory

Review found rev 1's list incomplete (e.g. `getComments` hydrates its post via
raw `GetByURI` and already serves deleted-post content today). Piecemeal
predicates will miss a surface; therefore:

- **One admission-aware visibility predicate** (SQL view or shared query
  helper joining `community_post_admissions`), used by *every* posts read
  path. No handler queries `posts` directly for display.
- Inventory to convert and test: community feeds (`feed.getCommunity`,
  `getAll`, `getDiscover`, `getTimeline`, `communityFeeds`), `post.get`,
  `getComments`' post hydration, actor surfaces (`actor.getPosts`, comment
  community filters), search, embed hydration of quoted posts, community post
  counts and user statistics (counts must not include non-accepted rows), and
  moderation views (which deliberately *do* see pending/removed).
- Author self-view: authors see their own posts with per-community status;
  other viewers see accepted only; removed renders as `#removedPost` + code;
  pending renders to non-authors as `#notFoundPost`.
- T1/T2 tests must prove pending/removed content is unreachable through the
  *alternate* endpoints (comments, search, counts), not just absent from feeds.

---

## 7. Remote communities (Beta — sequenced after local flip)

**Plain-language scope note.** This section is ONLY about the cross-server
case — a user whose home server is instance A posting into a community hosted
by instance B. It has nothing to do with private communities. The problem it
solves: after the author's post lands in their own repo on A, *B has to find
out it exists* before B's acceptance engine can evaluate it. Firehose
delivery gets it there eventually — if B's relay crawls A's PDS — but that is
a coverage-and-latency bet. `post.notify` is A tapping B on the shoulder:
"post at `<uri>` targets your community, go evaluate it now," turning
post-to-accepted from "whenever the relay delivers" into one round trip.
`getStatus` is the reverse direction: the author's client asking B "did you
accept it, and if not, why" (B's rejections are AppView-local, §3.3, so
there's no record to read — you have to ask). Service auth is how B knows the
notify/getStatus caller genuinely is that author, without the author having
any account on B. For same-server posts none of this machinery runs — §4.4's
synchronous fast path already did everything.

federation-prd's service-auth material is explicitly hypothetical, and the
current middleware verifies service JWTs only for registered aggregators,
without `lxm` enforcement. This section is therefore a specification to build,
not machinery to reuse:

1. **Discovery**: the community DID document gains a concrete service entry —
   id `#coves_host`, type `CovesCommunityHost`, endpoint = the hosting
   AppView's public URL — written at community creation and rotated on
   migration. Resolvers validate scheme/host (https, no private ranges).
2. **Auth**: `post.notify` and `post.getStatus` accept atProto service-auth
   JWTs: `iss` = the *author's* DID, `aud` = the community host's service DID,
   `lxm` = the exact method NSID, short expiry, `jti` replay cache. Endpoint-
   specific middleware — not the aggregator-global path — enforces all four.
3. **Flow**: author's server writes the post to the author's repo, then
   service-proxies `post.notify` to the community host. The host fetches the
   record from the author's PDS (CID-verified, SSRF-safe — same fetch path as
   §5.4), runs the acceptance engine, writes the acceptance; its firehose
   carries the acceptance to every other AppView.
4. Without notify, §5.4's direct fetch on acceptance-events plus relay
   coverage still converges; notify removes the latency.

Client UX: optimistic self-view immediately; `getStatus` polling for the
accepted transition.

---

## 8. Abuse & resource limits

Anyone can write unlimited posts naming any community; nothing stops the
*records* — the design absorbs them at the admission layer:

- Per-author, per-community, and per-origin-PDS submission quotas in the
  acceptance engine (new policy, §4.1), with `rejected` +
  `rate-limit-exceeded` decision codes, `redrivable = false`.
- Dedupe identical submissions by (author, community, canonical-record
  fingerprint) — the hash of the canonical record with `createdAt` removed
  (§4.1, rev 2.5), bucketed by the dedupe window.
- Debounce edit re-evaluation per post (a rapid edit storm collapses to the
  latest CID).
- Retention caps on `pending`/`rejected` admission rows for never-accepted
  posts, and on the `post_submissions` ledger (migration 035), whose
  confirmed rows are otherwise never deleted and grow one per admitted post.
- Notify endpoint: per-caller and per-PDS quotas on top of service-auth.
- All outbound fetches (identity bootstrap §5.3, record fetch §5.4/§7) behind
  SSRF guards, response-size caps, and timeouts.

---

## 9. Testing (per `docs/TEST_ARCHITECTURE.md`)

- **T0**: `admitPost` matrix (visibility, ban — new, rate limits — new,
  aggregator authz); §5.5 state machine including bridgedStats-only repin,
  removal terminality, edit-while-removed; deterministic rkey derivation;
  lexicon fixtures (post loses `author`; acceptance/removal fixtures valid +
  invalid; community-immutability rejection).
- **T1**: admissions-table transitions under the §5.2 watermark (stale-rev
  acceptance after removal is a no-op; same-rev removal wins);
  acceptance-before-post → direct-fetch path (CID mismatch rejected);
  rejection rows non-redrivable; idempotent replay of fast-path + firehose
  double-delivery.
- **T2**: ingestion contracts for both new collections. Full loops: author
  write → pending → accepted → in feed; banned author never accepted; edit →
  pending_reacceptance → re-accept; moderator removal (atomic commit) → feed
  drop + `#removedPost`; author delete → tombstone + acceptance cleanup;
  pending/removed unreachable via comments/search/counts.
- Anti-flake: subscribe-before-write cursor pattern; no sleeps (test-audit).

## 10. Rollout (hard cutover — pre-production)

### 10.1 Order

One branch, one `make ci`, one merge:

1. Lexicons (§3) + fixtures + migration 034.
2. `admitPost` extraction **plus** the new ban/rate-limit checks with tests
   (called out as new policy, not refactor).
3. Ingestion (§5): admissions state machine, watermark, direct-fetch
   convergence, consumers + contracts.
4. Write path (§4: session-explicit signature, author-repo writes, author-repo
   blobs, `post.update` as new scope) + read path (§6.2 centralized
   predicate).
5. Prod cutover: **stop writers** (brief maintenance window — mixed old/new
   writers must not overlap), then a **resumable, idempotent migration script
   with a per-post ledger**: for each community-repo post, write the record
   into the author's repo as `social.coves.community.postv2` (dropping the `author`
   field; authors are overwhelmingly the Kagi aggregator,
   whose session we hold; human authors are on our PDS), write the acceptance,
   **verify both records and the pinned CID, checkpoint the ledger — and only
   then** delete the community-repo record. Authors whose credentials cannot
   be restored are logged and their posts re-materialized under an explicit
   fallback decision, not silently dropped. Truncate + re-index `posts`.
6. Delete: community-credential post writes, `author`-field handling, old
   fixtures. `federation-prd.md` gets a superseded-by header.

### 10.2 Client coordination

`coves-mobile` / `coves-frontend`: post URI authority flips to author DIDs;
anything parsing `at://` URIs or deep-linking by community DID changes in the
same release window. Pending/removed status UI ships after cutover. The
fork/import flow (a community accepting a post whose `community` field names a
different DID) is deliberately **not** built now — the data model supports it
(§6.1), the privileged flow that exercises it is future scope.

## 11. Open questions (decided-by-default, flag to revisit)

1. **Snapshot dial** (v2): embed content in acceptance records for
   self-contained CARs — at the cost of re-hosting user content. Default:
   pointer-only.
2. **Comment admission**: comments render un-gated on accepted posts.
   Default: out of scope.
3. **Separate bridged-stats record** (v2): if bridgedStats-only repins (§5.5)
   still generate too much community-repo churn, move mutable aggregates out
   of the post record entirely.
4. **Private communities** (Beta): acceptance records leak subject URIs on the
   public firehose; private-community design must address opaque/hashed
   subjects. Out of scope for the flip.
