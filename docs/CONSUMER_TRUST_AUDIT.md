# Consumer & write-path trust audit (follow-up to `COMMUNITY_HANDLE_BINDING.md`)

Status: remediation in progress. Audited 2026-08-28; updated 2026-08-30 after
the timestamp, legacy-ingestion, and consumer-availability fixes. §1.1 through
§1.3 are implemented and regression-tested. The remaining sections are
explicitly open product or follow-up work, not claims that this change resolves
them.

Question asked: *does the community-handle bug class — a record-asserted value
trusted as authoritative about identity/ownership without binding it to
`event.Did` — recur anywhere else, and what else sits on that surface?*

## 0. Headline

**The handle bug is now unique among active consumers.** Every active consumer
keys ownership on
`event.Did` + `commit.RKey`, and every delete path resolves rows through a URI
built from `event.Did`, so no repo can delete, re-attribute, or moderate
another repo's rows. The only recurrence found was the deprecated
`social.coves.community.post` `author` field; that ingestion path is now retired
(§1.2). Existing legacy rows remain readable. The handler side never lets a
body DID override the session DID.

What the audit *did* surface is a different, systemic problem: **the
"ordering failure ⇒ transient" convention gives any repo on the network a free
denial-of-service against every consumer lane** (§1.3). Plus one verified p1
that is not a trust bug at all (§1.1).

Severity key: p1 = fix before next deploy; p2 = schedule; p3 = backlog.

## 1. Findings and remediation status

### 1.1 p1 — future comment `createdAt` breaks hot ranking (RESOLVED 2026-08-30)

Before remediation, `internal/atproto/jetstream/comment_consumer.go` parsed
`createdAt` with no future clamp. The post consumer already had one, in
`parseRecordCreatedAt`; the comment consumer never got it.

Hot-rank SQL at `internal/db/postgres/comment_repo.go:683`, `:949`, `:1210`,
`:1242`:

```sql
log(greatest(2, c.score + 2)) / power(((EXTRACT(EPOCH FROM (NOW() - c.created_at)) / 3600) + 2), 1.8)
```

With `created_at` more than 2h in the future the base goes negative, and
Postgres refuses a negative base with a non-integer exponent. Run against the
test Postgres:

```
ERROR:  a negative number raised to a non-integer power yields a complex result
```

Effect: any repo writes one comment on any visible post with
`"createdAt": "2030-01-01T00:00:00Z"` → every `getComments` on that post under
the default (hot) sort fails, for every viewer, until the attacker deletes it.
There is no moderator removal path for comments (§2, D2), so nobody else can.
Below the 2h threshold the same field pins the comment to the top of new/hot,
which is exactly what the post-side clamp was written to prevent.

Implemented in three layers: the comment consumer now uses the same
future-clamping `parseRecordCreatedAt` path as posts; hot-rank SQL clamps a
negative age to zero; and migration 041 repairs previously indexed future
timestamps. `TestCommentConsumer_FutureCreatedAtClamped`, the repository
hot-rank tests, and `TestMigration041_ClampsFutureCommentCreatedAt` pin the
ingest, query, and repair defenses respectively.

### 1.2 p2 — legacy `social.coves.community.post` trusted the record's `author` (RESOLVED 2026-08-30)

`post_consumer.go:194` `AuthorDID: postRecord.Author`, checked only for
existence at `:834` (`GetUserByDID`). The consumer correctly binds
`event.Did == record.community` (`:805`), but author is a free field.

Impact before remediation: an indexed community repository could attribute a
legacy post to another indexed author. The legacy visibility path treated it as
accepted immediately, and the update path froze the attribution.

Resolved by retiring ingestion of the deprecated collection. It was removed
from `ConsumerPosts`, and `PostConsumer.HandleEvent` ignores create, update,
and delete commits for it without advancing the rev gate. Existing legacy rows
remain readable and rematerializable. `TestPostConsumer_LegacyCommunityPost_IsIgnored`
pins that contract. The active postv2 path derives the author from `event.Did`.

### 1.3 p2 — "ordering failure ⇒ transient" is a universal free stall (HARDENED 2026-08-30)

Before this remediation, the connector retried transient errors **in-line, on
the serial read loop**:
`connector.go:165` `retryDelays = 200ms, 1s, 3s`; `connector.go:411-442`
`processMessage → handleWithRetry` synchronously; then a dead-letter row with
`MaxRedriveAttempts = 10` (`redrive.go:17`). So one event that fails
transiently costs its lane 4.2s of wall clock and eleven more attempts, and a
foreign repo can mint such events at zero cost. 1000 records ≈ 70 minutes of a
stalled lane. This is exactly the scenario `errors.go:17-19` says
`ErrPermanentEvent` exists to prevent, and it has been re-opened, one
consumer at a time, by the (individually reasonable) "the referenced thing may
arrive later" argument.

Every site, with what the attacker controls:

| lane | site | attacker-chosen value | per-event cost |
| --- | --- | --- | --- |
| votes | `vote_consumer.go:443` "vote subject not indexed" (the new subject gate) | `subject.uri` naming a nonexistent repo | 4.2s + 10 redrives |
| posts | `authorpost.go:716-722` postv2 unknown `community` | `community` string (not even `format: did` enforced, `:464`) | 4.2s + 10 |
| posts (retired legacy path) | `post_consumer.go` legacy "author not found" | `author` | 4.2s + 10 |
| posts | `authorpost.go:289-312, 1178-1183` acceptance fetch of an unindexed subject dials the PDS in the *subject's* DID doc | subject DID → slow PDS | up to 4×5s |
| communities | `community_consumer.go:1157-1166` subscription to unknown/malformed `subject` (FK) | `subject` — no DID-shape check | 4.2s + 10 |
| communities | `community_consumer.go:857-880` `verifyDIDDocument` fetch of an attacker `did:web:a<n>.evil.com` | `hostedBy` + wildcard DNS, host that accepts TCP and never answers | ~10s (`wellKnownTimeout`) per distinct DID before negative-cache; limiter bounds rate not latency, and legit events share `Wait` |
| communities | `community_consumer.go:1237-1266` block with non-DID `subject` → CHECK violation (migration 009) | `subject` | 4.2s + 10, **can never succeed** |
| users | `user_consumer.go:213-216` `ErrHandleAlreadyTaken` + every `InvalidHandleError` on identity events | relay-asserted handle | 4.2s + 10 |
| aggregators | `aggregator_consumer.go:277` `fk_community` (any user repo writing an authorization) | any | 4.2s + 10 |
| aggregators | `aggregator_repo.go:365` authorization `update` changing `aggregatorDid` → `record_uri` UNIQUE violation | rkey/aggregatorDid | 4.2s + 10, can never succeed; old aggregator stays authorized |

**HARDENED 2026-08-30.** The lane-stall mechanism is closed, and the remaining
remote work is explicitly bounded. `ErrUnresolvedReference` (`errors.go`) is
the third error class: the
connector's `handleWithRetry` short-circuits it exactly like `ErrPermanentEvent`
(one handler call, no in-line sleeps). It is dead-lettered with only three
redrives remaining; cache/in-flight hits that perform no new work do not consume
an attempt. A redrive pass snapshots its ID high-water mark and attempts each
row at most once, so a backlog cannot collapse a 15-minute retry window into a
single pass. Rows age out after seven days. Per-site outcome, same order as the
table:

| site | now |
| --- | --- |
| vote subject gate | `ErrUnresolvedReference` |
| postv2 unknown `community` | `requireDIDShaped` → permanent; unknown → unresolved |
| legacy author/community not found | ingestion retired; commits are ignored |
| acceptance direct fetch | fetcher's permanent RecordNotFound passes through; any other fetch failure → unresolved |
| subscription subject | `requireDIDShaped` → permanent; FK-not-found → unresolved |
| `verifyDIDDocument` | cache keyed by `(DID, handle domain)`; atomic in-flight marker re-stamped after limiter wait; 5s fetch ceiling; malformed host and definitive ID/alias mismatches → permanent; reachability/status/parse failures → unresolved |
| block non-DID subject | only indexable `did:plc`/`did:web` methods pass; migration 009's CHECK is no longer reachable with `did:key` |
| identity handle | `InvalidHandleError` → permanent; `ErrHandleAlreadyTaken` → unresolved |
| authorization `fk_community` | repo maps to `aggregators.ErrCommunityNotFound`; consumer → unresolved; `aggregatorDid` DID-shape → permanent |
| authorization `record_uri` UNIQUE | repo now retires the row the URI previously produced in the same transaction, so a retarget indexes and the old aggregator loses its authorization |

Pinned by `TestConnector_UnresolvedReferenceSkipsRetriesButKeepsBoundedRedriveBudget`,
the redrive snapshot/permanence/cache-defer tests (T0), the DID-document
classification and in-flight tests (T0), the DID-method gates (T0), the
`ErrorIs` assertions in `error_taxonomy_transient_test.go` /
`vote_subject_gate_test.go` (T1), and
`TestAggregatorRepo_CreateAuthorizationRetargetsTheRecordURI` (T1).

Residual cost is bounded rather than nonexistent: each distinct fresh DID and
handle-domain pair can still consume one rate-limited fetch of at most five
seconds before caching applies. The LRU is capped at 1,000 entries, so a large
fresh-key flood can evict older results. This is mitigation, not a claim that
remote verification is free.

Implemented classification: split "malformed" from "unknown".
- Malformed (not DID-shaped, CHECK/UNIQUE violation, non-postv2 collection) →
  `ErrPermanentEvent`, free. Several rows above are already this.
- Well-formed-but-unknown reference → `ErrUnresolvedReference`, bounded and
  off-lane: no in-line retry sleep, three scheduled redrive attempts, and
  seven-day retention.
- `verifyDIDDocument`: cache the negative result *before* the fetch completes
  (in-flight marker) and lower `wellKnownTimeout` for unknown domains.

### 1.4 p2 — comment `root`/`parent` are never checked for consistency

`comment_consumer.go:912-928` `validateCommentEvent` checks both are well-formed
AT-URIs and nothing else: not that `root` is a post, not that `parent` exists,
not that `parent`'s root == `root`. Counters trust each independently
(`:817-857`), 0-rows-affected is a log line.

- A nonexistent parent can leave a durable count that no rendered thread owns.
- A parent from one thread paired with another root can split rendering,
  counting, and community attribution across posts. Exact record recipes are
  omitted while this validation gap remains open.

Fix shape: on create, load `parent`; require it exists and (for a comment
parent) `parent.root_uri == root`; for a post parent require `parent == root`.
Reject as permanent otherwise.

### 1.5 p2 — `users.handle` IS an addressing key; §8 of the handle-binding doc is wrong

`user_consumer.go:174-213` stores `event.Identity.Handle` verbatim
(`KNOWN LIMITATION` comment at `:163-167` covers ordering, not provenance).
`internal/core/users/service.go:353-366` `ResolveHandleToDID` does
`userRepo.GetByHandle` **before** any directory resolution, and that backs
`actor.getProfile?actor=<handle>`, `actor.getPosts`, `actor.getComments`, and
**`actor.blockUser`/`unblockUser`** (`internal/core/userblocks/service.go:248`),
which then writes the resolved DID as `subject` into the *caller's* PDS record.

If a feed supplies an unverified handle for a DID while the real handle owner
is absent locally, DB-first lookup can bind later reads and writes to the wrong
account. Trust source is the relay, not a hostile repo, so this needs a wrong or
compromised feed (the `self` feed is ours) — p2, not p1. Exact event recipes are
omitted while the resolution order remains open.
OAuth login uses indigo's directory, not the DB (`oauth/handlers.go:304, 612`),
so login is not hijackable.

Fix shape: same as communities — re-resolve on identity events, or at least
make `ResolveHandleToDID` directory-first with the DB as a cache.

### 1.6 p2 — authorized aggregators posting straight to their PDS are unmetered

`RecordAggregatorPost` has exactly one caller, the HTTP API
(`internal/core/posts/service.go:303`). The firehose acceptance engine
(`internal/core/posts/decider.go:198-215`) checks the hourly quota via
`ValidateAggregatorPost` but never records, and at `:255` exempts registered
aggregators from the engine's own admission quota on the stated premise that
`ValidateAggregatorPost` governs them. That premise only holds for API-submitted
posts. An aggregator a community authorized once, writing `postv2` directly
with its own credentials, has `CountRecentPosts == 0` forever and also skips
the private-community and ban checks (`admit.go:509-560`). Only brake is the
community disabling the authorization after the flood.

Fix shape: record in the engine path too, or apply the engine quota to
aggregators.

## 2. p3 backlog (confirmed unless marked)

Update/delete asymmetries:
- **Community `delete` ignores rkey** — `community_consumer.go:363-365` →
  `deleteCommunity(ctx, did)`; create/update enforce `RKey == "self"`
  (`:476`, `:614`). `DELETE FROM communities WHERE did=$1` cascades to every
  author's posts, subscriptions, memberships, aggregator rows; admission rows
  are orphaned (no FK, migration 034). Self-scoped, but a stray non-self
  record's deletion nukes the real community.
- **Vote `update` silently dropped** — `vote_consumer.go:104-111` has no
  `update` case (posts/comments/communities do). A `putRecord` direction flip
  leaves AppView drifted from the PDS; visible through the two-source viewer
  state (next item).
- **Viewer vote state has two sources of truth** — posts read the PDS-backed
  cache (`internal/api/handlers/common/viewer_state.go:47-61`), thread
  comments read Postgres (`comment_repo.go:1337-1348`), actor-comments reads
  the cache again. Any gate-dropped vote shows as inconsistent, and the re-tap
  toggle-off path (`service_impl.go:145-172`) then silently deletes it. This is
  the mechanism behind the known federation-lag deletion.
- **Subscription/block `update` dropped** — `community_consumer.go:1112-1116`,
  `:1222-1226`; `ON CONFLICT` refreshes only `record_uri/record_cid`
  (`community_repo_subscriptions.go:67-70`), so a changed `contentVisibility`
  never lands.
- **Removal rkey not validated** — `authorpost.go:1075` create never checks
  rkey; delete recovers the subject by `SubjectRkey(uri) == RKey`
  (`:1398-1416`), so a non-canonical removal can never be withdrawn.
- **Pre-emptive removals are unbounded orphan rows** — `applyRemoval`
  (`authorpost.go:1286-1303`) upserts `(event.Did, any_uri)` with no post
  lookup. Inert on read (join key includes `p.community_did`,
  `post_visibility.go:107-116`, pinned by `TestDiscoverVisibility_ForkJoinKey`)
  but unlimited storage; `decision_code` is unbounded TEXT (lexicon says 64)
  and is rendered to authors.

Other:
- **Legacy resolver branch indexes `handle.invalid`** — `post_consumer.go:850`
  lacks the check its postv2 twin has (`authorpost.go:869`); `users.handle`
  UNIQUE → second unverifiable bridge author collides forever.
- **Profile validation is DB-CHECK only** — `user_consumer.go:394-401`; CHECK
  counts code points, lexicon counts graphemes/bytes; a valid record can fail
  and churn. Self-row only.
- **Aggregator `createdAt`/`disabledAt` parse failures fall back silently** —
  `aggregator_consumer.go:143-147`, `:245-250`; structural failures are
  permanent, these are not.
- **Vote `subject.cid` stored, never validated or read** — `vote_consumer.go:153`.
- **Rev gate accepts any `rev` bytes** — `rev_gate.go:112`; keyed on
  `event.Did` so a hostile rev freezes only the emitter's own URI.
- **`account` events are a no-op** — `user_consumer.go:255-269`; PDS-side
  deactivation/takedown never hides content. Safe direction (nothing on the
  wire can trigger erasure), but stale.
- **Dead `IsConflict` branches** — `community_consumer.go:1262`,
  `user_consumer.go:518`; both writes are upserts.

Handlers (all p3; the write surface is in good shape):
- `aggregator.register` (`internal/api/handlers/aggregator/register.go`) is
  unauthenticated and binds a *domain* to a body DID via
  `/.well-known/atproto-did` — proves the caller controls a domain claiming the
  DID, not the DID. Net effect: pre-provisioning `users` rows. Rate-limited.
- Web `POST /delete-account` (`internal/web/handlers.go:122-148`) unseals the
  cookie but never calls `store.GetSession`, so a revoked session's token
  deletes the AppView account until sealed TTL; CSRF relies on SameSite=Lax
  only. XRPC `actor.deleteAccount` is behind `RequireAuth` and fine.
- "Admin" for suggestion status is `COMMUNITY_CREATORS`
  (`routes/communitysuggestion.go:14`); no admin role exists. Empty var =
  community create open to all, `updateStatus` closed to all.
- Service JWT accepted with `lxm=nil` (`middleware/auth.go` `handleServiceAuth`);
  effective surface is `post.create` only.
- Mobile HTTPS fallback puts the sealed token in the redirect URL
  (`oauth/handlers.go` `handleMobileCallback`). Known trade-off.
- Handle→DID on the handler side is DB-first for both communities
  (`communities/service.go:1157, 1266, 1285, 1296`) and users (§1.5). The
  community branch's severity is entirely the handle-binding fix.

## 3. Product gaps surfaced (not defects, but nothing in the tree says they are intended)

- No ban / admission gate on firehose **comments** (`validateCommentEvent`
  checks only `did:` prefix; `grep -i ban comment_repo.go` is empty) or
  **votes** (`votes.ErrBanned` defined, never referenced; no self-vote check).
  `ErrBanned` is enforced only on the XRPC write path, which a PDS-direct
  client bypasses.
- No moderator removal path for comments: `DeletionReasonModerator` has no
  writer anywhere. Makes §1.1 and §1.4 un-remediable by mods.
- `community_blocks` has no read-path consumer; indexed community blocks do
  nothing to feeds.
- Deleting an aggregator service/authorization does not touch already-admitted
  posts.

## 4. Verified correct — do not re-audit

- **Identity binding** in every active consumer: voter = `event.Did`
  (`vote_consumer.go:151`); commenter = `event.Did` (`comment_consumer.go:163`);
  subscriber/blocker = `event.Did` (`community_consumer.go:1150`, `user_consumer.go:458,507`);
  postv2 author = `event.Did` (`authorpost.go:482`); aggregator service DID must
  equal `event.Did` (`aggregator_consumer.go:133`), authorization `communityDid`
  must equal `event.Did` (`:222`, stored from `event.Did` at `:256`).
- **Acceptance/removal moderation binding**: records carry no community
  field; `event.Did` must be an indexed community (`authorpost.go:1026`);
  acceptance requires `stored.communityDID == event.Did` (`:1119-1133`) or,
  on the fetch path, `record.Community == event.Did` after CID recomputation
  from CAR bytes (`:400-407`, `:1209`); non-postv2 subjects refused without a
  fetch (`:1164-1169`). Deletes resolve subjects `WHERE community_did = event.Did`.
  Cross-community removal rows are inert on every read path. **Untested
  branches** worth pinning: the indexed-path cross-community acceptance at
  `:1119-1133`; end-to-end stranger-community removal leaving the post visible.
- **Delete paths** all key on `at://event.Did/<collection>/<rkey>`; none
  decrement counters for rows that were never indexed (votes load direction
  from the stored row; comments deliberately never decrement).
- **Vote uniqueness** on `(voter_did, subject_uri)` in code and by partial
  unique index; rkey freedom cannot inflate counts.
- **Subscription counts** gated on fresh insert (`xmax = 0`), FK blocks
  phantoms, decrement floored.
- **Bridge trust** provenance is always the PDS endpoint from resolving
  `event.Did`, never a record field.
- **SSRF transport** (`oauth/transport.go`): resolve-once + dial vetted IPs
  (rebinding), IPv6 embedded forms, redirects capped and re-guarded, 32 MiB
  body cap, `NormalizeDomain` before URL build. Untested at the community call
  site: redirect-to-private, oversize `did.json`.
- **Undeclared-field passthrough**: every consumer decodes a fixed set of
  lexicon-declared keys; only the community `handle` was undeclared.
- **Handlers**: `RequireAuth` = sealed AES-GCM token + DB session lookup +
  `AccountDID` match; body `createdByDid`/`hostedByDid`/`authorDid` rejected
  when present; update/delete authorize on the URI authority vs session DID
  before any fetch; `createApiKey` refuses OAuth-session/aggregator DID
  mismatch; the `b05e98b` open-redirect fix is complete on write and read
  sides; no SQL built from input (all `Sprintf` hits are `$n` placeholders or
  fixed-literal `switch` outputs).

## 5. Suggested order

1. Ship the completed §1.1 timestamp clamp, §1.2 legacy-ingestion retirement,
   and §1.3 bounded/off-lane consumer availability changes with their T0/T1
   regression coverage.
2. §1.4 comment ref consistency; §1.6 aggregator engine metering.
3. §1.5 users handle — same design as the community binder; do it in the same
   PR series so `ResolveHandleToDID` and `resolveScopedIdentifier` get the same
   directory-first treatment.
4. p3 backlog via `/file-issue`.
