# Moderation

Formerly `docs/PRD_ADMIN_MODERATION.md`. Code comments cite this file by section heading and by decision ID (Q-I6, Q-ORACLE, D-QUOTES), so keep both stable.

Server admins remove and restore posts and comments, apply or retract an NSFW label on posts, ban or unban accounts, and remove or restore communities. The service is `internal/core/moderation`, the state is in migrations 050 to 052 (community comment removals in 067, published labels in 071, account and community subjects in 072 and their recorded kind in 075, account-ban hide markers in 073, and PDS account-status restrictions and their rechecks in 074 and 076), and the served endpoints are `social.coves.moderation.removeContent`, `restoreContent`, `labelContent`, `retractContentLabel`, `banAccount`, `unbanAccount`, `removeCommunity`, `restoreCommunity`, `getSubjectState`, `listActions` and `listAdminActions`. The other schemas in that lexicon directory have no routes: `ban` is the community ban record Tidepool writes and the consumers index ([POSTS.md](POSTS.md) § 4.1), and the `banUser` family, `ruleProposal`, `tribunalVote` and `vote` are held back.

## Scope

A removal redacts content from this instance's served views and nothing else. It never writes to the author's PDS, never writes or deletes a community's acceptance or removal records, and never rewrites author self-labels. That holds for communities hosted on this instance too: community admission stays a separate constraint on visibility.

Instance removals reach Tidepool only as signed `!takedown` labels (§ Published labels). With `COVES_LABELER_DID` set, Tidepool follows `com.atproto.label.subscribeLabels`, sends a `Delete` for native content it delivered and an `Undo{Delete}` on negation, and withholds bridged content (Tidepool docs/PROTOCOL.md, "Coves instance takedowns"). Without that consumer, copies already delivered to Lemmy stay there.

Content state is several independent facts, never one `deleted` flag: the author's record lifecycle, community admission, active moderation decisions (each with its own authority and scope), and active classifications such as NSFW. An author edit, a firehose replay, a delete and recreate at the same URI, or community re-acceptance does not clear a standing removal. Decisions are keyed by subject URI; a different URI is a different subject.

## Authority

Admins are the DIDs listed in `MODERATION_ADMINS`. An empty list grants nobody, and config validation rejects an entry that is not a DID. `RequireInstanceAdmin` in `internal/api/middleware/admin_auth.go` checks the caller's DID (OAuth session or PDS service JWT) against the list before any handler or idempotency replay runs. Never authorize by handle, community creatorship, PDS ownership or a caller-supplied actor field. The authority DID on every action is `INSTANCE_DID`.

Any admin may reverse another admin's decision within the same authority and scope, and history keeps both actors. Removing a DID from the list does not erase or retract its past actions.

The list is read at boot. To change it in production, edit `MODERATION_ADMINS` in `/opt/coves/.env` and recreate the AppView (`docker compose -f docker-compose.prod.yml up -d --no-deps appview`, see `docs/PRODUCTION_DEPLOY.md`).

Keep `MODERATION_ADMINS` empty until supported web and mobile builds pass release gate 5 in [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md). There is no `#notFoundPost` fallback: `post.get` serves `#moderatedPost` for every instance removal, so a client that cannot decode it breaks as soon as an admin acts.

## What the viewer may do

Clients never probe admin endpoints to learn whether to show moderation controls. Post, comment and `#moderatedPost` views carry `viewer.moderation`, the action tokens the signed-in viewer may take, using the modlog's action names. For an instance admin, a live post gets `remove` and `label`, or `remove` and `retract-label` when this instance's NSFW label is active. A live comment gets `remove`. Content this instance removed gets `restore`, including a comment its author also deleted. Signed-out viewers, non-admins, content only another authority removed, and author-deleted comments this instance did not remove get no field. `PostViewerActions` and `CommentViewerActions` in `internal/core/moderation` compute the lists.

Screens with no item call `social.coves.actor.getViewerAuthority`, which needs authentication and returns `{instanceAdmin}` from `MODERATION_ADMINS`, `false` for a non-admin rather than `Forbidden`. Community moderators will later feed the same lists without a client change.

## Served shapes

- A removed post's thread (`getComments`) returns `NotFound`. Its comments stay in their authors' repos and come back when the post is restored.
- `post.get` returns `#moderatedPost` for a removed post: no record, title or embed. It goes only to a viewer who could have seen the post had it not been removed (`AdmittedURIsForViewer`), so a removal never discloses a pending or rejected post. `#removedPost` keeps its meaning of a community removal and is never substituted.
- Feeds, search, Discover and profile activity exclude removed posts. Profile activity also drops removed comments and every comment whose root post is removed.
- A removed comment reuses the author-deleted placeholder: `record` absent, `isDeleted`, `deletionReason: moderator`, a `moderation` object naming the source authority and scope, author handle `handle.invalid`, zero vote stats, and thread references and `replyCount` kept. Its replies stay in the thread. If the author also deleted it, `deletionReason` stays `author` and the `moderation` object is still attached.
- `commentView.record` is optional under an approved output-loosening exception listed in LEXICON_PUBLISHING.md. The wire shape did not change; the AppView had always served deleted comments without a record. `handle.invalid` replaced an empty string that failed `format: handle`.
- `blockedComment.blockedBy: moderator` is published but never emitted.

D-QUOTES: Coves-quoted posts are not hydrated server-side. The served quote is a strongRef only, so a removed quoted post leaves no copied content to redact, and `post.get` on its URI returns `#moderatedPost`.

## Removal and NSFW

Removal and NSFW are independent decisions. Removal always wins: a reveal control never shows removed content, applying or retracting NSFW never restores a removal, and restoring never clears NSFW. NSFW is posts only and URI-scoped, so an edit does not drop it. It appears in `postView.moderation.contentLabels` next to the verbatim record and never changes feed eligibility, search or ranking. Clients combine it with the author's self-labels to drive their existing blur.

## Reasons

Reasons are the tokens in `moderation.defs`: spam, harassment, doxing, illegal-content, rule-violation and moderator-discretion. A seventh, subject-moved, is written only by Coves on the system actions described under [Community comment removals](#community-comment-removals). No request accepts it, and it appears only in action views (`actionReasonType`), not in `reasonType`. There is no public free text; private notes are admin-only. A `csam` report reason exists only in `adminreports`; an admin picks illegal-content when acting on one.

A removal's reason cannot be corrected out of the public log. Hiding is decided per entry, from its own reason and its reversed action's reason, and there is no amend API, so a restore and re-removal leaves the first entries public. Choose doxing or illegal-content on the first removal.

Doxing and illegal-content are remove-only. Q-RESTORE-REASON (2026-09-29): `restoreContent` rejects them with `UnsupportedReason`. Q-LABEL-REASON (2026-09-30): `labelContent` and `retractContentLabel` reject them the same way.

Public modlog entries whose reason, or whose reversed action's reason, is doxing or illegal-content omit the subject URI, CID and community, and are left out of any query filtered by subject, collection or community. The record is still fetchable from its author's PDS, and the log must not link to it. Q-ORACLE, accepted 2026-09-29: someone who already holds a URI can see that it is removed, find no public entry naming it, and infer the reason was doxing or illegal content.

## Errors

The vocabulary is the sentinel list in `internal/core/moderation/errors.go`: AuthRequired, Forbidden, InvalidRequest, InvalidSubject, SubjectNotFound, DecisionNotFound, InvalidDecision, ContentChanged, StateConflict, IdempotencyConflict, UnsupportedReason, UnsupportedLabel, InvalidCursor and ModerationUnavailable. `tests/lexicon_moderation_test.go` pins the seven content and modlog endpoints to the subset each can return; the account and community endpoints are not in that test. `ErrSubjectNotIndexed` and `ErrResolverUnavailable` never reach the wire alone.

Authorization runs before any subject or decision lookup. InvalidDecision (a reversal aimed at the wrong action kind, at another authority or scope, or at a decision no longer active) mutates nothing. A stale `expectedVersion` is StateConflict; a reviewed CID that no longer matches the indexed record is ContentChanged. An unindexed but well-formed URI has a state from `getSubjectState` (version `v0`, record unavailable), while `removeContent` and `labelContent` return SubjectNotFound for it.

## Account bans

`banAccount` and `unbanAccount` take an account DID (`subject`), `expectedVersion`, `idempotencyKey`, `reason` and an optional `privateNote`; `banAccount` also takes an optional `expiresAt`. Both are behind `RequireInstanceAdmin`, so a signed-out caller gets AuthRequired and a non-admin Forbidden. The moderation subject of an account is `at://<did>` with no collection (migration 072), and its version is read and checked through `getSubjectState` like any other subject. A subject that is not a DID is InvalidRequest. `banAccount` on a DID that is an indexed community is InvalidSubject unless the DID also has account activity (an account `users` row or an authored post, comment or vote) and its community row is not a trusted bridge's community (stored `pds_url` a trusted bridge host and stored `handle` under that host's registrable domain); such a DID (one that indexed as a community before its account footprint) is banned as an account, an account-kind decision that hides its authored content and refuses its writes like any ban. Other communities, including trusted-bridge ones, are moderated with `removeCommunity`. The check reads these facts, including whether the DID has a `communities` row, under the subject lock and comes before the version check; nothing read before the lock decides it. `unbanAccount` lifts an active ban even when the DID has since been indexed as a community; with no active ban, an indexed community's DID is InvalidSubject. Doxing and illegal-content are accepted on a ban and refused on an unban with UnsupportedReason, as for restore. `restoreContent` given the action ID of an account ban or a community removal is InvalidRequest; `unbanAccount` and `restoreCommunity` reverse those.

A ban is one `remove` action on `at://<did>` and the instance removal decision on that subject, with a `!takedown` label on the bare DID. `expiresAt` makes it temporary. It must be strictly after the server's clock, or the call is InvalidRequest; it is stored at millisecond precision (truncated, UTC) and is part of the idempotency request, so the same key with a different expiry is IdempotencyConflict. The replay check runs before the expiry check, so replaying a ban after its expiry returns the original result. A ban is in force while its decision is active and `expiresAt` is absent or later than the server's clock (`accountbans.Ban.InForce`). At `expiresAt` it ends with nothing written: no job, action, label or modlog entry, since the label's `exp` already tells consumers it no longer applies.

`banAccount` with the current version on an account whose ban is in force replaces it with a new action, carrying the new expiry or none, and writes a new `!takedown` whose later `cts` supersedes the old one. On a lapsed ban it is a new ban. On a DID whose active decision is a community removal (a removed community whose row was deleted) it is StateConflict. `unbanAccount` on an active ban, in force or lapsed, records a `restore` that reverses it, deactivates the decision and writes the `neg`. With no active ban it is StateConflict.

While a ban is in force, the account's posts (every author class, aggregators included), comments and votes are refused with `Banned` ("Your account is banned on this instance") before any PDS write or ledger reservation; after an unban or after `expiresAt` they are accepted. A post that reaches the acceptance engine while its author's ban is in force (a firehose post, or an accepted post edited and awaiting re-acceptance) is neither rejected nor removed: the engine defers it, and decides it normally on a pass after the ban is lifted or lapses. `getSubjectState` on `at://<did>` shows `moderation.state` `removed` while in force and `clear` after lapse, and `localRemoval` (with `expiresAt` for a temporary ban) while the decision is active. Account actions appear in `listActions` and `listAdminActions` with the `at://<did>` subject, the acting admin and `expiresAt`, under the same reason-withholding rules, and never with a content snapshot.

### What a ban hides

While a ban is in force, every viewer other than the account itself, signed in or not, stops seeing the account's content. Its posts drop out of the timeline, community feeds, search, Discover and `actor.getPosts`, `community.post.get` returns them as not found, and `actor.getComments` lists nothing. Its comments in threads, subtrees and permalinks are removed placeholders (`deletionReason` `moderator`, an instance-scope source naming the instance DID) with replies kept. Notifications whose record, subject or root post is the account's read `removedByServerAdmin` for every recipient but the account ([NOTIFICATIONS.md](NOTIFICATIONS.md) § Moderated accounts and communities). `actor.getProfile` returns the suspended shape: `did`, `handle`, `createdAt`, `accountStatus` `suspended` and zero post and comment counts, with no display name, description, avatar or banner. The account itself still sees all of its own content and its full profile, plus a `banNotice` (`reason`, and `expiresAt` for a temporary ban), which `actor.getViewerAuthority` also carries. These are read-time checks on the database clock (`accountRestrictedSQL`), so an unban or a temporary ban reaching `expiresAt` restores everything they hid, with nothing written. Per-item removals are independent and stay in place after an unban.

Coves cannot refuse comments and votes the account writes straight to its PDS while banned, so the consumers record them hidden with `hide_reason` `account_ban` (migration 073) when the ban is in force at Coves index time on the consumer clock, in every community and before any pending community check. Like `community_ban` the marker is permanent: those comments stay placeholders for everyone and those votes stay out of tallies after an unban or lapse, and replays keep it. Only content Coves indexed before the ban or after it ended is served and counted again. Index-time classification holds the ban-subject advisory lock on the author's DID, which `banAccount` and `unbanAccount` also take, so a ban's commit and a classification serialize.

## Account status restrictions

An account whose PDS reports it `takendown` or `suspended` is restricted on this instance like an account under an in-force ban, for reads only. The users consumer handles `#account` events with `active` false and one of those two statuses, and activations (`active` true) of an account that has a stored restriction; other statuses are ignored, and an activation with nothing stored asks no PDS. Jetstream delivers `#account` for the whole network, and a takedown or suspension for a DID with no Coves presence (a `users` or `communities` row, or an authored post or comment) and no stored restriction is confirmed and stored like any other, at the cost of one PDS call per network takedown: content the account wrote before its takedown can still be indexed afterwards (the users, posts and comments connectors run independently, and the redrive lane can index an earlier commit later), and the stored restriction hides it. A failed confirmation for such a DID stores nothing and keeps no recheck row, so it is not retried. The event only triggers a check: the consumer asks the account's current PDS (`com.atproto.sync.getRepoStatus`, uncached identity resolution, SSRF-safe client, 5-second timeout) and applies only the confirmed answer, storing the status in `account_status_restrictions` (migration 074) or deleting the row. A 400 answer naming `RepoTakendown`, `RepoSuspended` or `RepoDeactivated` counts as that inactive status: `getRepoStatus` declares only `RepoNotFound`, but PDSes use these names from the sibling `com.atproto.sync` lexicons (`getRepo`, `getLatestCommit`). Any other error name, or these names under another status, is an error status. A forged or stale event cannot set or lift a restriction. Each DID's read, confirmation and write run under one per-DID lock.

While the row exists, every read that hides a banned account's content hides this account's too (`accountRestrictedSQL`): posts, comments, notifications and the suspended profile shape, with the account itself still seeing everything. It gets no `banNotice`, and no Coves write is refused. Comments, edits and active re-creates the account's PDS still delivers while restricted send no reply or mention notifications, but carry no `hide_reason`; its late votes still count. Deleting the row is the whole restore: nothing was rewritten, so nothing needs undoing.

A confirmation that fails (identity unresolved, PDS unreachable, an error status, or an invalid answer) keeps the stored state and writes a row in `account_status_rechecks` (migration 076) with the failure code; the event is consumed. A repo-not-found answer over a stored restriction is treated the same way: the restriction stays and the row is rescheduled with code `repo_not_found`; with nothing stored it changes nothing and clears the row. PDSes emit `#account` only on change, so the recheck worker in `cmd/server` is the retry: every minute it retries up to 50 due rows, fewest attempts first and then oldest due, so a long-failing DID cannot starve newer ones, waiting 1 minute before the first attempt and doubling up to a 1-hour cap. It never gives up; the row goes once the PDS answers, a due row for a DID with no Coves presence and no stored restriction is dropped without asking any PDS, and a newer `#account` event that asks the PDS again supersedes it: its answer clears the row, or its failure restarts the backoff. The exception is an activation of an account with no stored restriction: it asks no PDS and keeps the pending row, so a forged active event cannot cancel a pending takedown check. Each pass with due rows logs its due, confirmed, failed and dropped counts at Info; a pass cut at the 1-minute cycle deadline logs them at Info too, a pass ended by shutdown is not logged as a failure, and any other failed pass logs at Error.

## Community removal

`removeCommunity` and `restoreCommunity` take a community DID (`subject`), `expectedVersion`, `idempotencyKey`, `reason` and an optional `privateNote`, behind `RequireInstanceAdmin` like `banAccount`. A subject that is not a DID is InvalidRequest. `removeCommunity` on a DID that is not an indexed community is InvalidSubject; `restoreCommunity` lifts an active community removal even when the community row was deleted, and with none, a DID that is not an indexed community is InvalidSubject. The moderation subject is `at://<did>`, read and version-checked through `getSubjectState`. Removing an already-removed community at the current version is unchanged, with nothing written. Removing a community whose DID has an account ban in force is StateConflict (unban it first); a lapsed ban is superseded by the removal. Restoring a community with no active removal is StateConflict, and it never reverses an account ban, and doxing and illegal-content are refused on a restore with UnsupportedReason. Reusing an idempotency key across the two procedures is IdempotencyConflict. Both actions appear in `listActions` and `listAdminActions`.

A removal is one `remove` action and the instance removal decision on `at://<did>`, with a `!takedown` label on the bare community DID. A restore records a `restore` action, deactivates the decision and writes the `neg`. Coves does not take the community's account down on its PDS.

While the removal is active:

- `community.get` still answers 200 with `did`, `handle`, `name`, `displayHandle` and `status` `removed`, and withholds the display name, description, description facets, avatar and banner.
- The community drops out of `community.list`, community search and Discover.
- Post create (every author class, aggregators included), comment create on its threads, vote create on its posts and comments, and subscribe are refused with 403 `CommunityRemoved` before any PDS write or ledger reservation. A comment is checked against the community of its indexed parent, the same resolution as the community ban check. Existing subscriptions stay in place.

These are read-time checks, so a restore brings everything back with nothing else written. The firehose acceptance engine does not decide a post in a removed community: its decider returns undecided (`ErrCommunityRemovedDeferred`, counted as deferred), and `ListPendingSubjects` leaves the community's `pending` and `pending_reacceptance` rows out of the batch. Posts written straight to a PDS, and edits of accepted posts, wait in those states during the removal and are decided as usual after the restore, so the removal never becomes a rejection or a signed removal commit. The community's existing content is hidden at read time too: its posts drop out of every read path for every viewer but their author, whose profile posts, thread header and `post.get` keep them with admission status `removed` and no community avatar, and comments whose parent chain reaches one of its posts are listed in profile activity only to their commenter. Notifications about either read `removedByServerAdmin` for every recipient but the author. Community suggestions have no link to an indexed community, so a removal changes nothing there.

A community removal and an account ban are stored the same way (an instance removal decision on `at://<did>` naming no collection), and each action records which it is in `subject_kind`, `community` or `account` (migration 075). Readers and mutations key on the recorded kind, never on whether the DID has a `communities` row or whether the decision has an expiry. Migration 075 backfilled the kind with the rules readers used before: a removal with an expiry is an account ban, a permanent one is a community removal when the DID has a `communities` row and an account ban otherwise, and a restore has the kind of the action it reverses. Its down migration refuses while any recorded kind differs from that rule, while any account action is on a DID that has a `communities` row, or while a restore's kind differs from the action it reverses. A community-kind action never has an expiry (a CHECK constraint).

The community consumer refuses (`ErrPermanentEvent`) a community profile from a DID with account activity: a `users` row whose `index_source` is `account`, or an authored post, comment or vote. A `users` row written only by post-author hydration (`index_source` `author_hydration`, migration 075, which also backfilled it for existing rows whose DID has a `communities` row) does not count: the authored post counts on its own, and the row outlives it when the post cascades away with a deleted community. Signup, sign-in, aggregator registration and a profile record indexed from the firehose write or upgrade the row to `account`; profile and handle updates leave the source alone. The exemption is a trusted bridge's community: the resolved PDS is a trusted bridge host and the verified handle is under that host's registrable domain (a bridged group posts as itself before its profile arrives). The PDS alone never exempts, since the DID controller writes it in its own DID document, and the record's own origin or `hostedBy` never exempts it. `banAccount` applies the same rule to the stored community `handle` and `pds_url`. User creation and the firehose post, comment and vote consumers hold the ban-subject advisory lock on the DID, which the community create takes before its check, so a concurrent write from them is either seen or waits; `cmd/reindex-votes` and the repository-level post, comment and vote `Create` methods do not take it. A footprint-free DID can still index as a community. An account ban made on it before it is indexed keeps reading as an account ban; after indexing, `banAccount` refuses the DID (`InvalidSubject`) until it gains account activity, and otherwise it is moderated with `removeCommunity`.

If a removed community's row is deleted (its profile record is deleted), the leftover decision still reads as a community removal, and `restoreCommunity` lifts it.

### Runbook: a community refused because of a pre-075 hydrated author row

Migration 075 marked `users.index_source = 'author_hydration'` only for DIDs that already had a `communities` row. A users row that post-author hydration wrote before 075 for a community DID with no `communities` row at deploy (its posts arrived before its profile, or the community was deleted before the deploy) is `account`, so the community's profile is refused. The backfill is deliberately not widened; fix such DIDs one at a time.

1. **Recognize it.** The AppView logs `security: refusing community profile from a DID with account activity` with `did=<did>`, and the event is dead-lettered already exhausted (`attempts` = 10) under consumer `communities` (or `communities@<feed>`):
   ```sql
   SELECT id, consumer_name, attempts, last_error, updated_at FROM jetstream_dead_letters
   WHERE consumer_name LIKE 'communities%' AND last_error LIKE '%community DID has account activity%'
     AND last_error LIKE '%<did>%';
   ```
2. **Check the evidence outside the database first.** Before starting the repair, confirm the DID is not an account: it has no `social.coves.actor.profile` record (`com.atproto.repo.listRecords` on its PDS) and was never a Coves user (signup history). The database keeps no record of a past sign-in, since sessions are deleted on logout. Any of these means it is an account; stop.
3. **Check and reclassify in one transaction.** Take the ban-subject lock first, so sign-in, user creation and the post, comment and vote consumers wait, then check, then update:
   ```sql
   BEGIN;
   SELECT pg_advisory_xact_lock(hashtext('community-ban-subject:' || $1));
   -- Every count must be 0 except users_account (1).
   SELECT
     (SELECT COUNT(*) FROM users WHERE did = $1 AND index_source = 'account') AS users_account,
     (SELECT COUNT(*) FROM posts WHERE author_did = $1) AS posts,
     (SELECT COUNT(*) FROM comments WHERE commenter_did = $1) AS comments,
     (SELECT COUNT(*) FROM votes WHERE voter_did = $1) AS votes,
     (SELECT COUNT(*) FROM oauth_sessions WHERE did = $1) AS oauth_sessions,
     (SELECT COUNT(*) FROM aggregators WHERE did = $1) AS aggregator,
     (SELECT COUNT(*) FROM communities WHERE did = $1) AS community;
   UPDATE users SET index_source = 'author_hydration'
   WHERE did = $1 AND index_source = 'account'
     AND NOT EXISTS (SELECT 1 FROM posts WHERE author_did = $1)
     AND NOT EXISTS (SELECT 1 FROM comments WHERE commenter_did = $1)
     AND NOT EXISTS (SELECT 1 FROM votes WHERE voter_did = $1)
     AND NOT EXISTS (SELECT 1 FROM oauth_sessions WHERE did = $1)
     AND NOT EXISTS (SELECT 1 FROM aggregators WHERE did = $1)
     AND NOT EXISTS (SELECT 1 FROM communities WHERE did = $1);
   COMMIT;
   ```
   If any count is not as expected, run `ROLLBACK` instead of the UPDATE and stop. An authored post means the community's posts are still indexed, and the refusal is the account-activity rule working as intended unless the DID is a trusted bridge's community. A current `oauth_sessions` row means it is an account. The UPDATE repeats every check in its `WHERE`, so it must report `UPDATE 1`; on `UPDATE 0` a signal is present, so run `ROLLBACK` and stop. The lock expression is `communities.BanSubjectLockKeySQL` (`internal/core/communities/community_ban.go`); use that one if it has changed.

   A sign-in or signup after the commit upgrades the row back to `account` (the `author_hydration` upgrade in user creation, under the same lock), so the repair cannot erase a genuine account; the community profile is then refused again, as it should be.
4. **Re-index the profile.** Reset the dead letter so the redriver retries it on its next pass (`REDRIVE_INTERVAL`): `UPDATE jetstream_dead_letters SET attempts = 0 WHERE id = <id>;`. If the dead letter is gone (pruned seven days after its last attempt), have the community republish its profile record (any `putRecord` of `social.coves.community.profile` with a new rev). Confirm a `communities` row now exists for the DID.

## Public modlog

`listActions` is public and names the acting admin's DID. `listAdminActions` adds private notes and the subjects the public view suppresses. Public fields are built by allowlist in `NewActionView`, never by stripping a private projection.

Q-LABEL-LOG (2026-09-30): `listActions` never returns `label` or `retract-label` actions, and its `action` filter rejects those values with InvalidRequest. The admin log keeps them; the blur on the post is the public signal.

Restricted subject (decided 2026-10-03): removals are public by default. The public log names a subject (URI, CID and the community it was posted in) and matches it under subject, collection and community filters while its post or comment row is indexed, whether soft-deleted or not and whatever its admission status. Only a subject whose row is gone, as after account erasure, is restricted: listed without subject or community, and excluded from those filters.

Content snapshots (decided 2026-10-13): a removal stores the text of the version the admin removed, as `contentSnapshot`: a post's title, body and external link URL, a comment's body, and `capturedCid`, the CID it was read from. Restores and labels store none, and illegal-content removals store none at all. A restore entry shows the snapshot of the removal it reverses, under the same public rules. A label or retract-label entry (`listAdminActions` only) shows the post's current title, body and external link URL, with `capturedCid` its current CID; it has none when the post is deleted by its author, gone, or has a doxing or illegal-content action. A doxing or illegal-content removal also deletes every snapshot earlier removals of the same subject stored, for admins too. Removing a comment its author already deleted stores none, because the delete blanked its text. The public log shows the snapshot only with the subject, so it is withheld for doxing removals; `listAdminActions` shows it there. A restricted subject has no snapshot left: its row is gone only after account erasure, which also deletes the author's snapshots. Once the author deletes the subject, the public entry still names it but withholds the snapshot; `listAdminActions` keeps it. For a comment this is permanent: a new record that re-creates the comment's URI does not bring the snapshot back. The snapshot is kept while its action row exists, and a restore or a later edit does not change it; a label entry's text follows edits. Erasing the author's account deletes the author's snapshots and keeps the actions. The author is the account that wrote the post or comment, which for a legacy post is not the community repository that holds it.

Comment subjects carry `rootUri`, the post at the root of the comment's thread, recorded when the action was taken. A restore copies it from the removal it reverses. Actions recorded before 2026-10-13, and comments whose indexed root is not a valid AT-URI, have none. It is withheld from the public log with the rest of the subject, and on its own when the root post has a doxing or illegal-content action or is no longer indexed; `listAdminActions` then carries it in `privateSubject` (decided 2026-10-13).

## Media blocks

Every removal blocks the (owner DID, blob CID) pair of each image on the subject, on cache hits and misses, and purges the image proxy's disk cache. An illegal-content removal also blocks each CID for every owner, so a re-upload from another account is not served. Restoring deactivates the decision's blocks.

Q-I6 (2026-09-28): every-owner blocks cover only the CIDs indexed when the admin acted. An image the author adds later, by an edit, a recreate or a create the consumer does not index, gets an owner-scoped block only, through `MediaReconciler.ReconcileTx` when the change is indexed and `ReconcileIncomingTx` when it is not. Otherwise an author could get someone else's image blocked for every owner.

The owner is the repo that holds the blob, which is not always the human author. Legacy bridged posts live in a Tidepool community repo, so several legacy posts can share one (community DID, CID) pair, and removing one blocks the image on all of them. This was accepted.

Bluesky media served from cdn.bsky.app is outside Coves' control; removal only stops the AppView from handing out those URLs. Edge purge, cache headers and exposure bounds are in [MEDIA.md](MEDIA.md).

## Writes on removed subjects

Removing a post or comment adds no write-path refusal (decided 2026-09-20). `feed.vote.create` and `comment.create` forward to the author's PDS without looking up the subject's removal, and the consumer indexes votes and comments that target removed subjects. They stay unserved through the parent's state and appear on restore. Clients hide vote and reply controls on a removed-comment placeholder as they do on an author-deleted one. The refusals that do exist are for the writer or the community, not the subject: an account ban in force, a community ban (POSTS.md § 4.1) and a community removal.

## Idempotency and versions

Every mutation carries an idempotency key and the `expectedVersion` from `getSubjectState`. Keys are scoped to actor DID plus authority DID and bound to a fingerprint of the whole request; reuse with a different body is IdempotencyConflict, and a replay returns the original result, not current state. `MODERATION_IDEMPOTENCY_RETENTION` (default 24h) sets how long keys live, and `MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS` (default 1000) caps live keys per actor; a request over the cap is InvalidRequest. After a key expires, the monotonic version still stops an old retry from reversing newer state.

## Durability

`moderation_*` tables hold decisions, not index state. They have no foreign key to posts or comments, so decisions survive deletes, recreates at the same URI, and reindexing. Backups and any truncate or reindex procedure must keep them.

## Community comment removals

A `social.coves.community.removal` record whose subject is a comment removes that comment when the record is in the repo of the community that hosts the comment's thread root post (the root's indexed `community_did`). A record from any other repo is ignored. Writing the record removes the comment; deleting it restores it. There is no acceptance record. The consumer writes an `apply-removal` action and an active `moderation_decisions` row with the community as authority and `scope_kind = community`, `origin = inherited`; the delete writes a `retract-removal` that reverses it. These decisions are independent of instance removals: neither restores the other.

A removal that arrives before its comment or its thread root is held in `community_comment_removals` (migration 067), keyed by (repo DID, rkey). Deleting a held record discards it.

The thread root is the post the comment's parent chain reaches, not the comment's own `root_uri`. Removals are reconciled when the removal record is indexed (including a redelivery, which reconciles that record), when the comment consumer indexes a comment (every removal of it and of the comments beneath it), and when the post consumer indexes a post (every removal of the comments beneath it). Reconciling one removal locks its row, then resolves the thread under that lock, then acts on the current state:

- Held, and the thread resolves to the record's community: the removal is applied.
- Applied, and the thread now resolves to another community (the commenter deleted the comment and re-created it at the same URI under another community's post): the removal is demoted to held. Coves writes a system `retract-removal` with no actor, the community as authority, `origin = inherited` and reason `reasonSubjectMoved`, reversing the active `apply-removal`, and ends the decision. The public log shows the entry without an actor.
- Held after such a demotion, and the thread resolves to the record's community again (the comment came back): the removal is re-applied by a system `apply-removal`, again with no actor and reason `reasonSubjectMoved`, reversing the demotion. Only the first application and the community's own delete of the record are attributed to the community.
- The chain does not resolve (a parent comment or the post is not indexed, or the chain loops): nothing changes. A held removal stays held, and an applied removal stays applied, so a comment re-created under a dangling parent does not reappear.

Because the row lock is taken before the thread is read, an application that is still writing when the comment is re-created elsewhere is waited on by the re-creation's reconcile, which then sees the applied removal and the new thread and demotes it.

A removal from a repo that is not the thread's community is held indefinitely. There is no prune: a held row may be a legitimate removal waiting for its comment or its root post, and a comment shown under another community's post may come back under the removing community's post. The rows are bounded by the community's own record writes, one per (community, comment), since the record key is derived from the subject.

Authority follows the thread. When a comment's parent is re-created under another community's post, the removing community's removals of the replies beneath it are demoted as well, and the new thread's community can remove them.

`getComments` serves a community-removed comment as a moderator-removed placeholder with replies kept, attributed to the community (`scope.kind = community`, `scope.communityDid`). In the author's own profile activity the comment stays listed with its content and the same attribution; other viewers do not see it there. An instance removal hides the comment from its author too.

This is the Coves half of comment removal across the Lemmy bridge: Tidepool writes these records in the bridged community's repo when a Lemmy moderator removes a comment. Until Tidepool ships that writer, a Lemmy moderator's comment removal stays visible here.

## Published labels

With `LABEL_SIGNING_KEY` set, each instance removal, restore, NSFW label and NSFW retraction writes one signed atProto label (`!takedown`, `!takedown` neg, `nsfw`, `nsfw` neg) in its decision transaction, and `com.atproto.label.queryLabels` serves them. The AppView serves an instance DID document at `/.well-known/did.json` listing the `#atproto_label` key and `#atproto_labeler` endpoint, but production serves the static `static/.well-known/did.json` through Caddy, which lists the same `#atproto_label` key and `#atproto_labeler` endpoint; every key rotation updates `#atproto_label` there in the same deploy. The first boot with a key exports the decisions already active; a key change needs `LABEL_SIGNING_KEY_RETIRING` and re-signs every label at boot. Once a label is stored or that export has completed, a boot without `LABEL_SIGNING_KEY` fails (`ErrSigningKeyRequired`), so decisions are never made unpublished. `com.atproto.label.subscribeLabels` streams the same labels. Details are in [plans/MODERATION_LABELS.md](plans/MODERATION_LABELS.md) § Built today.

## Not built

- Consuming other sources' labels: see [plans/MODERATION_LABELS.md](plans/MODERATION_LABELS.md). Coves publishes its own (§ Published labels); whether Lemmy sees an instance removal of native content depends on Tidepool consuming them, which is outside this repo.
- Community-admin moderation written by Coves. The only community-scope decisions are inherited comment removals (above).
- Evidence retention for illegal-content removals, which keep no snapshot, and modlog editing.
