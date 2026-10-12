# Posts

Formerly `docs/PRD_AUTHOR_OWNED_POSTS.md`. Section numbers follow that design doc so the `§N` citations in code, migrations 034-037 and log output still resolve. Gaps in the numbering are sections that were dropped.

## 1. Problem

Posts used to be the only user-written record stored in the community's repo. The AppView signed the author's words into the community repo with the community's credentials. That made cross-server posting impossible, gave every community host impersonation power (the record's `author` field was an unsigned claim it could forge), and meant a community fork had to re-sign every post, orphaning every comment and vote that pointed at the old URIs.

## 2. Design summary

The author's repo holds the post. The community's repo holds the community's decision about it.

A post is a `social.coves.community.postv2` record in the author's repo. A community accepts it by writing a `social.coves.community.acceptance` record that pins the post's URI and CID. Community surfaces render only posts their community has accepted. A post that names a community without an acceptance is never shown in that community.

Admission state is per (community, post), so a fork can hold its own decisions about the same post URIs and the comment and vote graph stays attached. The post's `community` field is only its submission target.

Records are author-attributed as vouched for by a relay that verifies repo commits, or by a direct DID-resolved fetch (§5.4). Jetstream events carry no proofs, so never describe firehose events themselves as signed.

## 3. Lexicons

### 3.0 New NSID

The `social.coves.*` lexicons are published (see [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md)). Any consumer built against `social.coves.community.post` derives community = repo DID for that collection. Feeding it author-repo records under the same NSID would make it index authors as communities. A new NSID makes stale consumers ignore the new records, which is the correct failure. `community.post` is deprecated in place: its schema is unchanged apart from the description, and nothing writes new records to it. The XRPC methods (`social.coves.community.post.create`, `.get`, `.update`, `.delete`, `.getStatus`) keep their NSIDs; endpoints are verbs and records are nouns.

### 3.1 postv2

The record lives in the author's repo and has no `author` field. The repo DID is the author; consumers derive authorship from the event DID and nothing else. This is what makes authorship unforgeable, so never synthesize an author field from record data.

`community` is required and immutable. An update that changes it is ignored as a whole (`upsertAuthorPost` checks this before looking the community up). Retargeting a post means writing a new record. Applying the new content under the old community would publish content the first community never judged.

Media in a postv2 is owned by the author's DID and PDS. `blobOwnerOf` resolves blob URLs from the author for postv2 and from the community only for legacy posts. `crosspostOf`/`crosspostChain` now point at author-repo URIs, which survive community forks.

### 3.2 acceptance

The rkey is `SubjectRkey(postURI)`: unpadded lowercase base32 of the SHA-256 of the subject AT-URI, a fixed 52 characters. A readable transform (strip `at://`, swap `/`) is not total, because DIDs may be 2048 bytes and contain percent-escapes. A digest is total and deterministic. The input is the URI bytes as stored, with no normalization.

One post has one acceptance rkey per community, forever. That makes every acceptance writer idempotent: concurrent writers converge on the same record instead of minting duplicate TIDs, and re-acceptance after an edit updates the same record with a new pinned CID. Writes use `swapRecord`/`swapCommit`, so a lost race is a detected conflict.

The pinned CID is the contract. Edited content is never rendered under an acceptance of the old CID.

### 3.3 removal

Same rkey scheme as the acceptance; both collections share `SubjectRkey`. A removal deletes the acceptance and writes the removal record in one `com.atproto.repo.applyWrites` commit, so the firehose never carries half a moderation action. A restore deletes the removal and writes a fresh acceptance in one commit.

Removal is URI-scoped and terminal across author edits (§5.5). The pinned CID is audit metadata.

`code` is `knownValues` with `maxLength: 64`. It must never become an `enum`: peers may send codes this AppView has not seen, and their records must still validate.

Rejection at submission time writes no record, so spam cannot bloat the community repo. Rejection is AppView-local (§5.6) and readable through `post.getStatus`.

Today the only production writer of removal records is the acceptance engine, on a failed re-acceptance. `RestoreAcceptance` has no production caller, and there is no community-moderator remove or restore endpoint. Server-admin removals are a separate AppView-local system; see [MODERATION.md](MODERATION.md).

### 3.4 XRPC surface

`post.create` and `post.delete` take the caller's OAuth session explicitly; aggregators pass stored-token credentials. `post.update` is an author-session write with `swapRecord` conflict handling.

`post.get`'s `postView` carries optional `status` and `acceptanceUri` for the author's own view. A post removed by its own community renders as `#removedPost` with the removal code instead of `#notFoundPost`. A lookup failure while building that marker is an error, never a silent `#notFoundPost`.

`post.getStatus` takes a post URI and community DID and returns `pending | accepted | pending_reacceptance | rejected | removed` with decision code and time, read from the admissions table. It reports `pending_reacceptance` verbatim so an author who edited can tell that state from never accepted. It is unauthenticated (§7).

## 4. Write path

### 4.1 Admission checks

`admitPost` (internal/core/posts/admit.go) is the one decision for "may this submission become a post in this community". The same policy runs from the synchronous fast path and, through `AdmissionEngineDecider`, from the firehose engine. Order: community resolves, private-visibility wall (regular users), ban, aggregator authorization, dedupe, per-author quota (§8). The privacy wall comes before the ban lookup so moderation state is never read for a community the submitter cannot see.

Actor class is an input. Users get every check. Registered aggregators are held to their authorization record and their own hourly quota. Trusted aggregators (`TRUSTED_AGGREGATOR_DIDS`, falling back to `KAGI_AGGREGATOR_DID`) skip visibility, ban and authorization. Both paths read the list through `TrustedAggregatorDIDs`, so they parse it the same way: `CreatePost` calls it per create, and the engine's decider gets it once at wiring, so a change reaches the engine only after a restart. On the write path a failed actor lookup downgrades to user; in the engine it is undecided, because the engine persists refusals as non-redrivable.

Bans go through `BanLookup` over `community_memberships.is_banned`, keyed by the resolved community DID. No membership row means not banned. A lookup failure fails closed; failing open would turn a database blip into a global unban. Live gap: no production code writes `is_banned`, so per-community post bans are wired but never fire. Comments do not go through admission at all, so a banned author can still comment.

"Hosted community" means this AppView stores the community's PDS refresh token. It never means `hosted_by_did`, which is a claim in a record anyone can write.

### 4.2 Flow

1. Validate and normalize input and check the authenticated DID, before any repo write.
2. Blobs (embed images, unfurl thumbnails) are uploaded to the author's PDS under the author's session. The community's credentials are not on this path at all.
3. The postv2 record is written to the author's repo. Aggregators write to their own repos with their stored OAuth sessions. The rkey is `SubmissionRkey(community, fingerprint, bucket)`, a real TID inside the submission's dedupe bucket, and the write is create-only, so a client retry after a lost response converges on the same record instead of creating a duplicate.
4. If this AppView hosts the community, `settleSubmission` seeds the admission row and runs the engine synchronously. The client gets the URI, CID and `accepted` back.
5. If the community is hosted elsewhere (`ErrCommunityNotHosted`), the post stays `pending`. This AppView has no authoritative view of another host's bans, visibility or quotas and must not decide for it.

Nothing after the record commits may fail the request. A failed seed or acceptance leaves the post `pending` for the engine to retry with the same rkey. Never roll back the author's record. Every failure between admission and the commit releases the ledger reservation, or it costs the author a quota slot and refuses their retry as a duplicate.

`post.delete` deletes from the author's repo and then `compensateAuthorDelete` withdraws this AppView's acceptance and tombstones the row directly, without waiting for the firehose copy. The firehose copy still arrives and is a no-op.

## 5. Ingestion

The posts lane subscribes to `postv2`, `acceptance` and `removal` together so both halves of a decision share one cursor and one dead-letter queue.

### 5.2 Watermark

Acceptance and removal are different record URIs about the same subject, so the per-record rev gate cannot order them. Every community-repo event about a subject applies through one compare-and-swap on the admissions row: the event's `(last_community_rev, last_community_op_rank)` must be strictly greater than the stored tuple. Op rank is delete = 0, put = 1, derived in the repository from the operation and never caller-supplied. Revs compare as `TEXT COLLATE "C"`, which for TIDs is commit order.

That one rule makes removal win inside the removal commit (acceptance delete at (R,0), removal create at (R,1)) and restore win inside the restore commit, in either arrival order. There is no distinct restore event: a community acceptance at a strictly newer watermark than the removal is the restore. An equal tuple is a replay and a no-op.

Author-repo events never advance the community watermark; neither does an AppView-local rejection. Letting an edit advance it would let an author outrank a moderator.

Every mutation is a single-statement CAS. A stale or terminal skip is an outcome value, never an error, because an error would route healthy skips into the dead-letter queue. A refused event leaves the row byte-identical, `updated_at` included.

### 5.3 Post events

The author is `event.Did`. An unknown community returns `ErrUnresolvedReference` (redriven off-lane, no in-line retry), because a post can arrive before the community's profile event but a nonexistent community must not stall the lane.

`posts.author_did` has no foreign key to `users` and no cascade (migration 034). Unknown federated authors must index, and author profiles are hydrated opportunistically. Because a missing `users` row no longer means "deleted", `deleted_accounts` (migration 036) marks erased DIDs so replayed or redriven events cannot recreate their rows. A failed lookup of that marker fails closed.

A new postv2 seeds a `pending` admission row. A delete tombstones the post and the hosting AppView withdraws its acceptance; author deletion is not moderation, so no removal record is written.

Legacy `social.coves.community.post` ingestion was retired on 2026-08-30 because its consumer trusted the record's free-text `author`. Firehose updates and deletes for the roughly 12,000 legacy rows no longer apply, so their bridged scores are frozen. API deletes tombstone legacy rows directly. Legacy rows stay readable and still collect vote and comment counts until the drain (§11).

### 5.4 Acceptance and removal events

Redrive alone cannot converge acceptance-before-post, because no retry manufactures a post event a relay gap never delivers. For an acceptance whose subject is not indexed, the consumer resolves the author's DID to a PDS and fetches the record with `com.atproto.sync.getRecord`, not `repo.getRecord`. The CAR lets the AppView recompute the CID from the bytes; `repo.getRecord`'s `cid` is a claim by a server a stranger chose. The CID must equal the pinned CID. The fetch is SSRF-guarded, capped at 1 MiB and 5 seconds. Fetch failures are `ErrUnresolvedReference`. A CID mismatch or an XRPC `RecordNotFound` is permanent (`ErrPermanentEvent`, dead-lettered already exhausted); a bare 404 is not, because it usually means a stale PDS URL rather than a missing record. Only transient failures are redriven.

An acceptance whose subject names a different community is refused permanently, on both the indexed and the fetched path (§10.2).

Applying an acceptance: pinned CID equals the indexed CID gives `accepted`; a mismatch gives `pending_reacceptance` with the acceptance fields kept, so an acceptance that arrives before its subject's edit converges when the edit lands. Deleting an acceptance returns an `accepted` or `pending_reacceptance` row to `pending`; if the same-commit removal already applied, the delete loses the watermark and the row stays `removed`. A delete for a subject with no row still inserts one, as a tombstone against replay of the acceptance it superseded. A removal with no prior acceptance is valid.

### 5.5 Edits and re-acceptance

An edit whose CID differs from the accepted CID moves the row to `pending_reacceptance`. The engine re-runs admission on the new content and either updates the acceptance (same rkey, new CID) or removes the post with a code.

`removed` is terminal against author events and against community events at or below the removal's watermark. An author edit while removed updates only `evaluated_cid` and `updated_at`; otherwise editing would launder a removed post back through auto-acceptance. A `rejected` row reopens as `pending` on new content and resets `redrivable`.

The design exempts `bridgedStats`-only edits from a trusted bridge author, so origin-platform score refreshes repin the acceptance instead of strobing posts out of feeds. `classifyRecordDiff` and `RepinAcceptedCID` exist, but no path calls them: the consumer keeps no old-record snapshot, so every edit, bridge refreshes included, goes through full re-admission. Bridge trust for `bridgedStats` is keyed on the author's PDS, not the community's.

### 5.6 Acceptance engine

`AcceptanceEngine` (engine.go) takes one admission row for a hosted community and makes the community repo agree with the policy answer: write or update the acceptance, record an AppView-local rejection, or commit acceptance-delete plus removal. `CommunityRecordWriter` is the only writer of community-repo records in the post system, and the engine is its decision point.

Rejection is a pending-only CAS carrying the judged CID: a rejection of content the row no longer holds refuses, and accepted or removed rows are never rejected. A failed re-acceptance is a removal, not a rejection. Policy rejections set `redrivable = false`; undecided outcomes (credential failure, lookup failure) defer and are retried.

The engine takes no lease. The queue driver, firehose redelivery and the fast path can race on one subject; deterministic rkeys, swap guards and the watermark CAS make the races converge. `QueueDriver` (queue.go) is the pull side: one goroutine walks the undecided backlog grouped by community, with per-subject backoff on deferral, every `ACCEPTANCE_QUEUE_INTERVAL` (default 1m, 0 disables it) in batches of `ACCEPTANCE_QUEUE_BATCH_SIZE` (default 50). It is what reaches posts stranded by an expired credential or a failed lookup.

## 6. Storage and reads

### 6.1 Admissions table

`community_post_admissions` (migration 034) is keyed `(community_did, post_uri)`. `posts` stays pure content.

| Status | Meaning |
|---|---|
| `pending` | Indexed, no decision yet |
| `accepted` | Acceptance pins `accepted_cid` |
| `pending_reacceptance` | Content changed since the acceptance |
| `rejected` | AppView-local refusal; no record exists |
| `removed` | Removal record stands |

`evaluated_cid` is the CID the last decision judged. `redrivable` is false only while a policy refusal stands; every transition that reopens evaluation resets it to true. `last_community_rev`/`last_community_op_rank` are the §5.2 watermark, with a CHECK holding the op rank to 0 or 1.

### 6.2 Visibility

Every posts display query joins through one predicate, `visiblePostsPredicate` in internal/db/postgres/post_visibility.go, bound via `visiblePostsJoin` (feeds, Discover, timeline, search, `post.get`, profiles, comment thread headers) or `visiblePostCountSubquery`. It is a shared JOIN, not a SQL view, because feed queries carry computed rank columns and the author self-view is parameterized by viewer.

Piecemeal predicates miss surfaces, so no posts read path queries `posts` for display without the predicate. That covers feeds, Discover, timeline, `post.get`, `getComments` post hydration, actor surfaces (`actor.getPosts`, comment community filters), search, community post counts and user statistics. Counts must not include non-accepted rows. Moderation views deliberately see pending and removed posts. Authors see their own posts with per-community status.

The join key is `a.community_did = p.community_did AND a.post_uri = p.uri`. Both halves matter: joining on URI alone would let one community's acceptance publish another community's pending post.

The rule: `accepted` with `accepted_cid = p.cid` is visible to everyone. Any other status is visible only to the post's author, so clients can show "pending" or "removed" on the author's own profile. No admission row means visible for legacy (non-postv2) posts and author-only for postv2: the consumer always seeds a row, so a missing one means the seed failed. An active server-admin removal hides the post from everyone, author included.

An `accepted` row whose CID has drifted (the window between an edit landing and the status transition) is hidden from everyone, author included; `post.getStatus` still reports it. `getComments` resolves its thread header and community filter through the same predicate. Tests must prove pending and removed content is unreachable through alternate endpoints (comments, counts, search), not only absent from feeds.

A community's served `postCount` is the live subquery, also used by `sort=active`. The stored `communities.post_count` and `IncrementPostCount` are vestigial (§12).

## 7. Remote communities

Not built. A post to a community hosted on another server reaches it only through the firehose and §5.4. The planned `post.notify` procedure, `#coves_host` DID service entry and endpoint-specific service auth (`lxm`, `jti`) do not exist.

`post.getStatus` is unauthenticated because its intended caller is an author on another server with no account here; requiring auth would make that case unanswerable. Clients poll it for the accepted transition, so it has its own named limiter (`postGetStatus`, 120/min) on top of the global one, which also bounds enumeration of rejected posts. Disclosing rejected status to anyone who can name the URI was accepted by the owner.

## 8. Abuse limits

Anyone can write unlimited records naming any community; limits apply at admission. Dedupe and the per-author, per-community quota use the synchronous `post_submissions` ledger (migration 035), not `posts`. `posts` is firehose-lagged (it hides the burst being limited), its `created_at` is author-supplied, and its indexes exclude soft-deleted rows (delete-to-evade).

The fingerprint is the canonical record minus `createdAt` and the client-typed community identifier. Dedupe is a unique insert, runs before the quota for every actor class, and refuses a retry as `duplicate-submission` rather than burning quota. The quota applies to users only. Refused submissions use no quota. Dedupe buckets are epoch-aligned, so effective protection ranges from near zero to the full window; that lets keys expire with no cleanup job.

Posts from other servers have no ledger row, so the engine's decider counts recent admission rows instead (`CountRecentAdmissions`) against the same limit. Rate refusals are `rejected` with `rate-limit-exceeded`, `redrivable = false`.

Knobs: `POST_SUBMISSIONS_MAX_PER_COMMUNITY` (10), `POST_SUBMISSIONS_WINDOW` (1h, rolling), `POST_SUBMISSIONS_DEDUPE_WINDOW` (1h). The server refuses to start if any is zero or negative, so unset can never mean unlimited.

Live gaps: no retention sweep exists for `post_submissions` or for never-accepted `pending`/`rejected` admission rows, admission rows are not removed when a community is deleted, the posts lane has no per-source-DID rate limit, and aggregators posting directly to their PDS bypass the ledger. A crash between reservation and PDS write leaves an orphan reservation that clears when the bucket rolls.

All outbound fetches (identity hydration, §5.4 direct fetch) go through SSRF guards, size caps and timeouts; see [SSRF_SECURITY.md](SSRF_SECURITY.md).

## 9. Test scope

No T2 community holds PDS credentials, so accepted-state arcs are proved at T1, and T2 contracts assert consumer semantics through `post.getStatus`. Read-path contracts must cover the alternate endpoints named in §6.2. See [TEST_ARCHITECTURE.md](TEST_ARCHITECTURE.md).

## 10. Rollout

### 10.1 Order

The flip was a hard cutover with no schema migration of existing rows. `posts` holds legacy and postv2 rows side by side, differing only in the URI's collection, and `IsPostCollection` routes both to the same counters. Step 5 of the original rollout order, re-materializing the legacy records, is now the §11 runbook; posts are not truncated and re-indexed (§11 step 5).

### 10.2 Clients and forks

Post URIs are author-DID based; clients must not parse a community out of a post URI. The fork/import flow, a community accepting a post whose `community` field names a different DID, is supported by the data model but not built. Until it is, the consumer refuses such acceptances permanently (`ErrPermanentEvent`), because otherwise a community could pull another community's content into its feed on its own say-so.

## 11. Cutover runbook

Live until the legacy drain finishes. The tool is `cmd/rematerialize-posts`; the state machine is `posts.Rematerializer`. It deletes production records irreversibly. Order matters: getting it wrong makes live posts vanish or relaunders a removed post.

1. Deploy the tool and migration 037 (`post_rematerialization_ledger`). Legacy read code stays; production still has `community.post` rows.
2. Maintenance window: stop writers. Only inside this window, `TRUNCATE post_submissions` and deploy the fingerprint retype (§12). A retype under live writers strands in-flight dedupe reservations.
3. Credential census first. The tool resolves every author before writing anything. Only authors with a stored non-interactive session (aggregators) can be re-materialized; a human author has no session the tool can use. A post whose author cannot be resolved stays as `community.post` with ledger state `fallback_left_legacy`, logged and never deleted. Never forge an admin-signed postv2: that reintroduces the impersonation §2 removes. By default the run stops before mutating if the census finds fallbacks; `-accept-fallbacks` proceeds. After re-authorizing authors, `-reopen-fallbacks` moves fallback rows back to `discovered` and exits.
4. Run the tool. Rehearse with `-dry-run` (same code path, mutations replaced); a real run needs `-yes` after the banner. `-community` scopes to one community; `-delay` paces PDS calls; `-timeout` (default 6h) bounds the run. It needs the AppView's `ENCRYPTION_KEY`, holds a Postgres advisory lock so a second run refuses to start, cancels between records on SIGINT/SIGTERM, and always prints the census, including on failure. Re-running resumes from the ledger.

   Per record: `discovered` → `postv2_written` (author repo, rkey `RematerializeRkey(oldURI)`) → `verified` (acceptance written with `WriteAcceptance` directly and read back from the community repo) → `migrated` → `done` (old record deleted with a swap guard on its source CID, then the AppView row tombstoned).

   `RematerializeRkey` is a deterministic TID from the SHA-256 of the old URI. It is not `SubmissionRkey` (submission-time inputs a re-run cannot reproduce) and not `SubjectRkey` (not a TID). Its golden test values must never change: every migrated post sits at the old key, so a change writes a second postv2 for each. Acceptance goes through `WriteAcceptance`, not the engine, because the engine re-decides and could reject a since-banned author's live post. `migrated` and `done` are separate so a crash after the checkpoint retries only the delete, after re-verifying.

   A legacy post with an active instance-scope removal by the instance DID is skipped (`skipped_removed`, an outcome, not a ledger state) and stays legacy. The removal is checked before discovery and again before the acceptance write; if the second check fires, the new postv2 is left unaccepted in the author's repo. A removal made after the acceptance does not carry over to the new URI.
5. Index convergence. The tool tombstones old rows itself, because the firehose no longer ingests legacy deletes; the new postv2 and acceptance index from the firehose. Do not truncate `posts`: fallback and skipped-removed rows can no longer be re-indexed.

   Blocking and unresolved: every comment and vote on a migrated post is orphaned. Comments pin `reply.root`/`reply.parent` and votes pin `subject` as strongRefs, re-materialization changes both URI and CID, those refs live in user repos, and `comment_consumer` refuses root or parent changes. Existing comment and vote rows stay attached to the tombstoned legacy URI, and the new postv2 row reconciles to 0 comments and 0 score. Nothing fails loudly.

   Options: an AppView-side remap of `comments.root_*`, `comments.parent_*` and `votes.subject_*` from the ledger (`old_uri`, `new_uri`, `new_cid`); re-materializing comments and votes too; or accepting the loss in release notes. Decide before the production run, and capture the ledger mapping before anyone truncates it.
6. Legacy read removal, on a separate branch, only when an unscoped run (no `-community`) reports `MIGRATION COMPLETE`, there are zero fallbacks and zero skipped-removed posts, and `SELECT count(*) FROM posts WHERE split_part(uri,'/',4)='social.coves.community.post' AND deleted_at IS NULL` returns 0. The `deleted_at` filter is required: the tool tombstones legacy rows through `PostRepository.SoftDelete` and never hard-deletes them. The tool's log lines that mention "§11 step 6" refer to this gate. A scoped run never certifies the whole migration. The surfaces to remove are listed in §12. Removing them before the drain is the vanish/relaunder bug.

The tool exits 1 when its own scope is incomplete. A scoped run that finishes its scope exits 0 but says the migration as a whole is not certified.

## 12. After the drain

These go together, after §11 step 6's gate holds:

- the legacy branch of `admittedPostsPredicate` (no admission row visible for non-postv2);
- `blobOwnerOf`'s community fallback;
- `removedMarkers` legacy handling and its author-field synthesis;
- `legacyPostCollection` in post_repo.go, the legacy half of `IsPostCollection`, and other `LegacyPostCollection` users (moderation included);
- `applyRemoval`'s absent-collection guard in authorpost.go;
- the `communities.post_count` column, `IncrementPostCount` and its `communities.Repository` entry, dropped with a migration (nothing reads them).

The `submissionFingerprint` retype from `PostRecord` to `PostV2Record` is deploy-sequenced to the §11 step 2 window with `TRUNCATE post_submissions`, never merged on its own. It ripples through `enhanceExternalEmbed` and `postV2From`.

## 13. Comments

Thread counts include deleted comments, which render as `[deleted]` placeholders. A comment delete soft-deletes and blanks content and never decrements `comment_count` or `reply_count`.

A top-level comment increments the post's `comment_count`. A nested reply increments the parent comment's `reply_count` and the root post's `comment_count`, so `comment_count` is the whole-thread total. Increments have no `deleted_at` filter, to match reconciliation.

Comments can arrive before their parents; there is no foreign key on the parent. When a post is indexed, the post consumer sets `comment_count` to `COUNT(*)` of comments with that root. When a comment is indexed, the comment consumer sets its `reply_count` to `COUNT(*)` of its children. Reconciliation and increment semantics must change together.

A comment re-created on the same parent skips the increment; a changed parent increments. The parent table comes from the parent URI's collection, and both post collections route to `posts` through `posts.IsPostCollection`. Recognizing only one fails silently with `comment_count` stuck at 0.

Comment hot rank is `LOG(GREATEST(2, score + 2)) / POWER(GREATEST(age_hours, 0) + 2, 1.8)`; the posts formula differs (signed log, exponent 1.5). Age is clamped so future-dated comments get no boost. The rank clock is pinned in the hot cursor (`rankedAt`) so later pages do not skip or repeat replies.

Comment author hydration uses `LEFT JOIN users` with a DID fallback, so comments from users not yet indexed are not dropped. `getComments` is mounted in cmd/server/routes.go behind its own `commentQuery` limiter (20/min) plus the global 100/min.
