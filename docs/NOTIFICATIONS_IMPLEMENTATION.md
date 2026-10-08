# In-App Notifications: implementation notes

How in-app notifications work in the AppView. The product spec is [PRD_NOTIFICATIONS.md](PRD_NOTIFICATIONS.md). This file records the product decisions made during the build that refine it, how the code fits together, and what to know when running it.

## 1. Summary

The AppView now writes and serves in-app notifications. Nothing is pushed: clients poll.

- **Endpoints** (all `social.coves.notification.*`, all behind `RequireAuth`, the caller is always the recipient):
  - `getUnreadCount` (GET): `{count}`, capped at 101 ("100+").
  - `listNotifications` (GET): keyset-paged list, `limit` 1..100 (default 50), opaque `cursor`, page-level `seenAt`, per-row `isRead`.
  - `updateSeen` (POST): advances the monotonic, server-clamped `seen_at` watermark.
  - `getPreferences` (GET) and `putPreferences` (POST): per-reason on/off; `putPreferences` returns the full object.
- **Reasons**: `postReply`, `commentReply`, `mention` (in comments and postv2 posts, on create and when an edit adds a mention), and `upvote` groups (one row per item, bumped to the top on a new qualifying upvote; `upvoteCount` includes trusted-bridge (Lemmy via Tidepool) totals; `recentUpvoters` names up to 3 native voters).
- **Placeholders**: deleted or moderator-removed content keeps its notifications. The view carries `status` (`deleted`, `removedByModerator`, `removedByServerAdmin`) and no text; it gets no new notifications.
- **Previews**: post title or 140-grapheme body excerpt, comment excerpt, the author's self-labels (`labels`), and post `thumbnail` / `thumbnailAlt` through the image proxy.
- **Maintenance**: account erasure deletes a user's rows; an hourly retention job prunes read, overflow and empty-group rows.

## 2. Product decisions made during the build

These refine the PRD and are binding on the code. Tests cite them by their Q labels. The PRD's own decisions cover the rest: Q10 is PRD decision 7 (deleted and removed content), Q11 and Q15 are decision 8 (bridged upvote totals and the high-water mark), and Q12 to Q14 are decision 9 (labels, thumbnails, blank titles, upvoter order).

- **Q1, community blocks**: blocking a community does not hide notifications. Only user blocks hide them, in either direction.
- **Q2, excerpts**: every excerpt is 140 grapheme clusters (comment preview, untitled-post body fallback, `record` excerpt). A post mention's `record` excerpt is the post title, or the body when there is no title.
- **Q3**: `putPreferences` returns the full preferences object.
- **Q4, who counts as an upvoter**: any voter who is not erased and not an aggregator. Voters on a trusted bridge PDS count. This applies to bumps, `upvoteCount`, `recentUpvoters` and deciding when a group is empty.
- **Q5, activation**: an edit to a record created before notifications launched never notifies. Only the 7-day freshness check uses the edit's time.
- **Q6, edit freshness**: a mention added by an edit is fresh when the edit's Jetstream event time is within 7 days. With no event time, index time is used.
- **Q7, re-upvote spam**: a voter bumps an item's upvote group once, ever. Removing and re-adding an upvote does not bump again, and a group deleted when the voter un-voted is not recreated by their re-vote. An earlier downvote does not count as an earlier upvote.
- **Q8, mention cap**: at most 10 mention notifications per record, in total, across create, every edit, re-create and resurrection. A slot freed by retention or erasure can be reused.
- **Reply/mention dedup**: someone notified as a record's reply recipient is never also notified of a mention by that record, after edits or re-creates too. The rule keys on who the reply recipient is, so a reply row lost to retention or a lifted block does not re-open the mention.
- **Q10 addition**: a live reply under a deleted root keeps its own excerpt; only the withdrawn reference loses its text.
- **Q15 accepted trade-offs**: after a group is deleted at zero, a re-rise only up to the old peak recreates nothing. A spurious spike raises the peak permanently.
- **Admin moderation removals (2026-10-06)**:
  - An active admin removal (`moderation_decisions`, kind `removal`) follows the Q10 placeholder rule. A notification whose root post, subject or triggering record is removed keeps its row, is listed and counted, and shows no text, excerpt, labels, thumbnail, title or community for that reference. Label decisions change nothing.
  - Status: any active instance-scope removal gives `removedByServerAdmin`, even alongside a community removal; community-scope removals alone give `removedByModerator`.
  - A post gets the placeholder only if it was publicly visible; a pending or rejected post stays hidden. An author's delete wins: a deleted post or comment reads `deleted` (or stays hidden without the marker).
  - Removed content gets no new notifications, including a comment or post whose own URI carries a removal. Restoring (decision inactive) does not backfill; the reference reads live again.
  - A comment reference whose root post carries an active removal reads the root's removal status, so a reply's excerpt is not shown inside a removed thread (main's comment lists hide such replies the same way). Its own `deleted_at` still wins. Q10's "live reply under a deleted root keeps its excerpt" still holds for author deletes and community withdrawals.
  - An admin removal does not stop the withdrawal markers: an admitted public post that is later author-deleted or community-removed still gets its marker, so its rows keep their placeholder.
- **Unindexed thread references (2026-10-06)**: a comment whose root post or parent (post or comment) has no indexed row fans out nothing, replies and mentions alike, so a comment pointing at a nonexistent post cannot plant rows that stay hidden forever. Accepted trade-off: a comment that arrives before its root or parent loses its notifications; nothing backfills them.
- **Hidden-reference sweep (2026-10-06)**: rows whose required reference reads hidden (unindexed, never public, removed or deleted without a marker, unsupported collection) are deleted once `sort_at` is older than 7 days. Rows hidden only by a block, a disabled preference or the upvote alive rule are kept. A reply to a post still pending admission after 7 days is lost.
- **Malformed comment labels (2026-10-06)**: a live comment reference whose stored self-labels do not parse omits its row (counted as `malformed_labels` in the omission warning) rather than showing the excerpt unlabeled.

## 3. How it works

### Schema and tables

- `internal/db/migrations/053_notifications.sql`:
  - `notifications`: `recipient_did` (FK `users` ON DELETE CASCADE), `reason`, `record_uri`, `record_cid`, `actor_did`, `subject_uri`, `root_post_uri` (NOT NULL, the navigation target), `record_created_at` (display only), `sort_at` (stamped with `clock_timestamp()` at the write, not the transaction-start `now()`; raised by bumps). A CHECK fixes the shape per reason: upvote groups have NULL record and actor; mentions have NULL subject.
  - Unique indexes: `uq_notifications_record (recipient_did, reason, record_uri) WHERE reason <> 'upvote'` and `uq_notifications_upvote_group (recipient_did, subject_uri) WHERE reason = 'upvote'`. Paging index `idx_notifications_recipient_sort (recipient_did, sort_at DESC, id DESC)`.
  - `notification_state (did PK FK users, seen_at NULL = never seen, disabled_reasons TEXT[])`.
  - `notification_activation`: one row, `activated_at = NOW()` at migration time. A missing row is an error (`postgres.ErrNotificationActivationMissing`), never "no cutoff".
  - `posts.bridged_upvote_peak` and `comments.bridged_upvote_peak` (INT NOT NULL DEFAULT 0, CHECK >= 0), backfilled from `bridged_upvote_count` (Q15).
- `054_votes_upvote_history_index.sql`: `idx_votes_voter_subject_upvotes ON votes (voter_did, subject_uri) WHERE direction='up'`, built `CONCURRENTLY` with goose `NO TRANSACTION`, for `EarlierUpvoteExists`.
- `055_notification_public_post_withdrawals.sql`: `(post_uri, kind IN ('authorDelete','communityWithdrawal'), community_rev, recorded_at)`, PK `(post_uri, kind)`, `community_rev` NULL exactly for `authorDelete`, no FK. Postv2 posts only.
- Any later migration must add a `MigrateDownOne` step to each rollback site in `internal/db/postgres` (11 call sites today, grep `MigrateDownOne(t, db, 55`) and update the "035 through 055" comment in `admission_repo_schema_test.go`.
- No new env vars. Retention thresholds are constants in `internal/core/notifications/notification.go`.

### Package layout

- `internal/core/notifications`: `notification.go` (types, constants), `interfaces.go` (`Lookups`, `Repository`, `ReadRepository`, `RetentionSweeper`, `PreferencesRepository`), `fanout.go` (pure fan-out functions), `service.go` (`Service`, `PreferencesService`), `list_service.go` + `view.go` (`ListService`, view types), `cursor.go`.
- `internal/db/postgres`: `notification_repo.go` (writes, lookups, SQL fragments, retention), `notification_visibility.go` (read predicate, unread count), `notification_list.go`, `notification_seen.go`, `notification_preferences.go`; `bridged_votes_repo.go` (bridged poller write path); `admission_repo.go` (community-withdrawal markers).
- Consumers depend on `notifications.Repository` and never import the service. Fan-out functions return intents; `Repository.ApplyTx` / `ApplyUpvoteGroupTx` write them inside the consumer's own index transaction.

### Write path: transactions and the erasure lock

- Every notification-writing consumer transaction runs at READ COMMITTED (`BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})`). `ErasureGateTx` checks isolation first and returns `postgres.ErrErasureGateRequiresReadCommitted` otherwise.
- `ErasureGateTx(tx, actorDID)` takes the shared `pg_advisory_xact_lock` on `postgres.ErasureLockKeySQL` (`hashtext('erasure:' || $1)`), then checks `deleted_accounts` in a separate statement. It runs right after `tryAdvanceRecordRev` wins and before any content row is read for update. Moving it later deadlocks with erasure.
- Erasure (`user_repo.go` `Delete`): exclusive advisory lock first, then the `deleted_accounts` marker, content deletes, `DELETE FROM notifications WHERE recipient_did=$1 OR actor_did=$1`, the user's withdrawal markers, `notification_state`, then `users`. Upvote groups on other users' items are left for the sweep. Moving the notification delete before the content deletes deadlocks (pinned by a test).
- Lock order everywhere: content (subject) row → notification/group row → `users` FK. There is no explicit `FOR KEY SHARE`; the `recipient_did` FK provides it.
- A recipient erased mid-fan-out raises 23503 on `notifications_recipient_did_fkey`. `ApplyTx` and the group upsert skip that case inside a savepoint and keep the caller's earlier writes; every other error aborts the transaction and the event is retried or dead-lettered.
- `ApplyTx` writes all record-keyed intents in one savepoint with one `unnest` INSERT `ON CONFLICT (recipient_did, reason, record_uri) WHERE reason <> 'upvote' DO NOTHING`. Only on that 23503 does it fall back to per-intent savepoints (Postgres caches 64 subtransaction XIDs per backend).
- Unparsable thread URIs give "no recipient" (`nil, nil`), never an error, so a malformed record cannot stall a Jetstream lane.

### Write path: hook points

- Comments (`internal/atproto/jetstream/comment_consumer.go`):
  - New insert, unsupported-parent early commit and different-parent resurrection go through `writeCreateNotificationsAfterCounts` (repair → fan-out unless erased → `DeleteReplyRecipientMentionsTx`), after the parent/root count updates.
  - Same-parent resurrection calls `writeCommentCreateNotifications` then `DeleteReplyRecipientMentionsTx` before its early commit.
  - `updateComment` and the active re-create (newer rev, new CID) share `writeCommentEditNotifications`. Both read stored facets, `created_at`, `parent_uri`, `root_uri` in one `FOR UPDATE` read inside the transaction. The re-create uses the pre-update `created_at` for activation.
  - `deleteComment` takes the gate (lock only) and keeps rows.
  - A different-root resurrection runs `ReplaceUpvoteGroupRootTx` for the commenter's group.
- Votes (`vote_consumer.go`): `indexVoteAndUpdateCounts` final commit applies `FanoutVoteCreate`; the stale-vote replacement early commit applies `FanoutVoteRemoval`; `deleteVote` applies `FanoutVoteRemoval` after the count decrement. The gate is used for its lock only; `FanoutVoteCreate` owns voter eligibility. A comment subject's root is read after the count UPDATE, under the row lock (`voteSubjectRootAfterCountUpdate`).
- Posts (`post_consumer.go`, `authorpost.go`): `indexPostIfRevWins` (Jetstream and direct fetch) → `writePostCreateNotifications`; the direct fetch passes no rev and serializes on `INSERT ... ON CONFLICT (uri) DO NOTHING`, so the mention-budget read must stay after the INSERT. `applyPostContentUpdate` reads stored facets and `created_at` `FOR UPDATE`, then `FanoutPostEdit`. `tombstoneRecordIfRevWins` takes the gate, soft-deletes, then `RecordPostAuthorDeleteWithdrawalTx`.
- Pending admission (`UpsertPending`) is written after the post insert transaction, not inside it.
- Wiring options: `jetstream.WithCommentNotifications`, `WithVoteNotifications` (also installs the voter's erasure lock), `WithVoteBridgeTrust`, `WithPostNotifications`; each consumer exposes `NotificationsWired()` / `BridgeTrustWired()` for the `cmd/server` wiring tests. Builders: `buildCommentConsumer`, `buildVoteConsumer`, `buildPostConsumer` in `cmd/server/consumers.go`.

### Fan-out and write-time gates (`fanout.go`)

- Order of checks, shared by every path: the withdrawn gate (`anyWithdrawn`) and record-level time gates (`notificationRecordAllowed`: activation on the record's `createdAt`, then 7-day freshness against index time) run once per record; then one batched `RecipientFacts` call in `notificationAllowedRecipients` (absent from `users` = not indexed, erased, aggregator, recipient's `users.pds_url` on a trusted bridge, block in either direction with the actor). The actor's own PDS never suppresses. The self rule is checked outside `notificationAllowedRecipients`.
- `BridgeHostChecker` implementations must be safe on a nil receiver; nil trusts no host.
- Replies (`FanoutCommentCreate`, `resolveCommentReply`): `postReply` to the root post's author for a top-level comment, `commentReply` to the parent comment's author otherwise. The root must parse and be a post collection. Postv2 author = repo DID; legacy author = `posts.author_did`; a direct reply to a legacy post with no row gives no intents at all (`errMissingLegacyPost`).
- Mentions: `richtext.MentionedDIDs` returns at most 200 distinct DIDs (parse bound, logged as "past the facet parse bound"). Mentions are dropped for the author, community DIDs and the resolved reply recipient (whether or not the reply survived its gates), and require a post root. Edits diff new against stored facets (`addedMentionDIDs`); removed mentions are never retracted.
- Mention cap (`notificationCappedMentions`, `MaxMentionsPerRecord = 10`): budget = 10 minus existing `mention` rows for the `record_uri` (`ExistingMentionRecipients`), read inside the writing transaction; recipients already holding a row are dropped; the rest are cut in facet order. Kept rows on deleted or resurrected records count.
- Edit freshness: `CommentRecord.EditEventTime` / `PostRecord.EditEventTime` from `eventTime(time_us)`; zero skips freshness (index-time fallback). Suppressed when `EditEventTime < IndexTime − 7d`; exactly 7 days is allowed. Activation still uses the stored `createdAt`.
- A non-RFC3339 `createdAt` parses as `time.Now()` and passes both replay gates (filed: `2026-09-29-malformed-createdat-defeats-notification-replay-bounds`). Future `createdAt` is clamped to now by the consumers.
- Withdrawn gate (`Lookups.ReferenceStates`, one statement): comments with `deleted_at`, posts with `deleted_at` or an own-community admission `status='removed'`, any URI with an active admin removal (indexed or not), and `ReferenceUnindexed` for a post or comment URI with no indexed row; no marker read (covers legacy posts). A URI in any other collection can never be indexed and reads live. Comment create/edit check the comment's own URI, root and parent; post create/edit check the post; vote create checks subject and root (returns DeleteIfEmpty, never Bump); bridged new highs (above `GREATEST(peak, stored count)`) check subject and root. `FanoutVoteRemoval` and the resurrection repair are not gated. Indexed pending or rejected posts read live here. This write-time notion differs from the read-time placeholder classification below.
- A block created later keeps existing rows; read-time filtering hides them.

### Upvote groups

- `FanoutVoteCreate` order: subject author (postv2 URI authority; legacy `LegacyPostAuthor`; comment = the commenter, whatever its root) → direction (`down` → DeleteIfEmpty with no lookups) → post root (a comment whose stored root is not a post never bumps) → self → withdrawn subject or root → activation/freshness on the vote's `createdAt` → voter erased → voter aggregator → recipient facts → `EarlierUpvoteExists` last (true → DeleteIfEmpty). Unresolvable subjects give NoChange; every other non-bump gives DeleteIfEmpty.
- `EarlierUpvoteExists` = any other `votes` row with the same voter and subject, `direction='up'`, different URI, live or soft-deleted. It requires `VoteRecord.URI`.
- `ApplyUpvoteGroupTx`: Bump = `INSERT ... ON CONFLICT (recipient_did, subject_uri) WHERE reason='upvote' DO UPDATE SET sort_at = GREATEST(...)` in `SAVEPOINT notification_upvote_group`; `root_post_uri` is written on insert only. DeleteIfEmpty = `DELETE ... WHERE NOT (alive)`, no savepoint. The caller must hold the subject's posts/comments row lock (or re-check in a separate statement); at READ COMMITTED the DELETE's subquery reads its original snapshot.
- `qualifyingUpvoteSQL(voteAlias, subjectExpr, recipientExpr)` in `notification_repo.go` is the one per-vote fragment: subject match, live, `up`, voter ≠ recipient, voter not erased, not an aggregator, no block either direction. No time, users-row or bridge conditions. It panics on unsafe aliases or expressions.
- `upvoteGroupAliveSQL(subjectExpr, recipientExpr, bridgedTotals)` is the one alive rule: a qualifying native upvote exists OR (gate on) the post or comment has `bridged_upvote_count > 0`. The bridged term deliberately ignores `deleted_at` (Q10 keeps a deleted item's count; the poller never writes deleted rows, so the total freezes). Used by DeleteIfEmpty, both sweep statements, the read predicate and the list aggregate. `bridgedUpvoteTotalSQL` gives the total (post wins over comment; `0` gate off). Both guarded by `requireUpvoteGroupOuterExpression`.
- Two concurrent first upvotes by one voter under different rkeys can both bump; `GREATEST` makes it one visible bump.
- Vote hard deletes reopen a voter's first-upvote bump: account erasure (expected) and `cmd/reindex-votes` (`DELETE FROM votes` then refetch; every voter could bump again after a run).

### Bridged upvote totals

- Gate: `postgres.NewNotificationRepository(db, postgres.WithBridgedUpvoteTotals())`, passed in `cmd/server/wiring.go` iff `cfg.Instance.TrustedBridgePDSHosts` is non-empty. The constructor builds `countUnreadSQL`, `listSQL`, `listUpvotesSQL` once; `CountsBridgedUpvoteTotals()` reports the gate. The retention sweeper is the same instance. No read statement joins `communities.pds_url` and there is no provenance column.
- Only the poller writes groups from bridged totals: `BridgedVotesRepository.ApplyAggregate` (one READ COMMITTED tx) → `lockBridgedAggregateSubject` (`SELECT bridged_upvote_count, bridged_upvote_peak [, root_uri] ... deleted_at IS NULL FOR UPDATE`, posts then comments) → the guarded UPDATE (millisecond `>=` on `asOf`, score recompute, `bridged_upvote_peak = GREATEST(bridged_upvote_peak, bridged_upvote_count, new upvotes)`) → `notifications.FanoutBridgedUpvoteChange` with the locked previous total and peak → `ApplyUpvoteGroupTx`. A stale `asOf` changes nothing. A notification error rolls back the aggregate and the sweep aborts before `MarkPolled`. A deleted or missing subject counts as stale.
- `FanoutBridgedUpvoteChange`: decrease → DeleteIfEmpty (alive rule decides); new total ≤ `GREATEST(peak, stored count)` → NoChange before checking the post root, withdrawn state or recipient; otherwise post root via `bumpRootPostURI`, `anyWithdrawn`, `notificationAllowedRecipients` with no actor, then Bump. No activation, freshness, `asOf` age, self or first-upvote check. The effective launch baseline is `GREATEST(peak, stored count)`, including rows whose stored count was written without a peak.
- The poller takes no erasure advisory lock (no actor). Lock order subject row → group row → `users` FK matches erasure.
- Wiring: `NewBridgedVotesRepository(db, WithBridgedVoteNotifications(notificationRepo, bridgeTrust))` in `buildBridgedVotePoller`, which runs only with trusted hosts configured. Test seams: `bridgedvotes.Poller.Store()`, `BridgedVotesRepository.NotificationWiring()`.
- The poller only sweeps candidates within its lookback (`BRIDGED_VOTE_POLL_LOOKBACK`, default 90 days by `created_at`), every `BRIDGED_VOTE_POLL_INTERVAL` (default 5 min).
- The Jetstream `bridgedStats` record path is not hooked: it accepts totals only for bridge-PDS authors, whom fan-out always rejects.
- Behaviour to expect (Q15): an oscillating total (5 → 4 → 5) does not bump on the return to 5; only a new high (6) bumps. A group deleted at zero is not recreated when the total climbs back only to its old peak. A peak raised by a spurious spike is never lowered. A removed bridge's totals stay counted and frozen. A compromised trusted bridge can still bump once per poll by reporting a steadily rising total, capped by `bridgedvotes.MaxBridgedCount`. A bridged-only group renders `upvoteCount` with no `recentUpvoters`. Turning the gate off makes the sweep permanently delete bridged-only groups; turning it back on does not restore them.
- Coves treats bridged and native votes as disjoint (score adds both); Tidepool serves a fediverse-only total net of votes it wrote back for native users. There is no dedupe here.

### Deleted content, resurrection and public-withdrawal markers

- Author deletes keep rows (`deleteComment`, `tombstoneRecordIfRevWins`, including a zero-row soft delete).
- `RecordPostAuthorDeleteWithdrawalTx` writes an `authorDelete` marker in the tombstone transaction when the post (postv2) passes `admittedPostsPredicate(anonymousViewerSQL)` (admitted publicly; an active admin removal does not prevent the marker); it ignores `deleted_at` because it runs after the soft delete.
- `communityWithdrawal` markers are written by `internal/db/postgres/admission_repo.go` (`compareAndSwapWithWithdrawal`, used by `ApplyAcceptanceDelete` and `ApplyRemoval`) in the admission transaction: lock the admission row `FOR UPDATE`, compute "admitted before" (postv2, `deleted_at IS NULL`, own-community post, `admittedPostsPredicate` for the anonymous viewer; no row = not admitted), run the existing guarded upsert, then upsert the marker with the event rev if it was admitted; a non-admitted `ApplyRemoval` deletes a marker whose rev differs (same-rev carry-forward). `ApplyRemovalDelete` does not touch markers. A marker-write failure rolls back the admission write. This is the one cross-domain write.
- Different-parent resurrection: `RepairResurrectedCommentNotificationsTx` deletes reply rows whose subject is not the new reply subject (`notifications.CommentReplySubject`) and repoints `root_post_uri` when the new root is a post; after fan-out, `DeleteReplyRecipientMentionsTx` removes a mention held by someone who now holds a reply row. Both resurrection branches call it.
- Mentions added by a re-create store the pre-update `createdAt` as `record_created_at`; `listNotifications` shows `record_created_at`.

### Erasure and withdrawal summary

- Account erasure deletes the user's received and sent notifications, their `notification_state`, and both marker kinds for their posts (`split_part(post_uri,'/',3) = did`, served by an expression index from migration 055). Groups on other users' items lose the voter through `qualifyingUpvoteSQL` (erased voters do not qualify) and are swept when empty.
- After erasure, `root_post_uri` in other users' rows still names the erased thread author's post (matches `comments.root_uri` behaviour).

### Retention (`notification_repo.go`, `cmd/server/notification_retention_job.go`)

- Constants: `RetentionReadWindow = 720h`, `RetentionUnreadCap = 500`, `RetentionUnreadWindow = 4320h`, `RetentionHiddenReferenceWindow = 168h`, `RetentionBatchSize = 10_000`.
- `RetentionSweeper`: `SweepReadNotifications` (rows whose `sort_at` is more than 30 days before the reference `COALESCE(seen_at, recipient's newest sort_at)`), `SweepUnreadOverflow` (only for users with a non-NULL `seen_at` and strictly more than 500 unread rows: deletes unread rows more than 180 days older than their newest row), `SweepEmptyUpvoteGroups` (candidate SELECT then a DELETE that re-checks the alive rule in a separate statement; no subject lock needed), `SweepHiddenReferenceNotifications` (rows with `sort_at` older than 7 days (`now() - 168h`) that fail `referencesVisible`, the reference-only half of the read predicate; no lower age bound, so rows missed during downtime or hidden later are still removed, and the lateral lookups per statement are bounded by the batch limit). Each is one READ COMMITTED tx deleting at most 10,000 rows via `FOR UPDATE OF n SKIP LOCKED`; windows are `$hours::bigint * INTERVAL '1 hour'`.
- Job: `runTicker(..., "notification-retention", 1h, ...)`, sweeps read → unread_cap → empty_groups → hidden_references, each looped while a batch returns 10,000. Errors are logged (`notification retention sweep failed`) and the next sweep still runs. When the cycle context ends, the cycle stops before the next batch or sweep and logs one line. Registered in `main.go` with `app.notificationRetentionSweeper`.
- For never-seen users the read reference is the newest row of any kind, so a hidden newest row (pending post, block-hidden) can delete their only visible unread row. PRD-literal. Placeholder rows are visible, so they do not cause it.
- After retention prunes a mention row, removing and re-adding the mention in an edit re-notifies.

### Read path: visibility predicate (`notification_visibility.go`)

- `notificationVisibility(bridgedTotals)` is the one predicate builder, used by `CountUnread` and `List`. It returns `joins`, the five state expressions (`subjectPostState`, `recordPostState`, `rootPostState`, `recordCommentState`, `subjectCommentState`) and `visible`. Extend it there, nowhere else.
- Post positions are `LEFT JOIN LATERAL` over `posts` with `admittedPostsPredicate(anonymousViewerSQL)` (reused from `post_visibility.go`, never copied). State precedence: community withdrawal (own-community admission `status='removed'`, joined on `a.community_did = p.community_did AND a.post_uri = p.uri`, AND a `communityWithdrawal` marker; `removedByServerAdmin` under an active instance removal, else `removedByModerator`) > admitted and undeleted (live, or the admin-removal status) > `deleted` (`authorDelete` marker) > hidden. Comments: deleted = placeholder, undeleted = live or the admin-removal status of the comment or its root post, unindexed = hidden.
- `visible` = no applicable state is hidden, plus collection guards, plus: blocks (`n.actor_did IS NULL OR NOT EXISTS user_blocks` either direction; groups use the per-voter rule), the alive rule for upvote groups, and disabled reasons (`NOT EXISTS ... n.reason = ANY(ps.disabled_reasons)`, NULL-safe).
- Admin removals (`moderation_decisions`, kind `removal`, active) are one scalar aggregate per reference (`moderatedState`): any instance scope gives `removedByServerAdmin`, community scope only gives `removedByModerator`, so several decisions never multiply rows. `ReferenceStates` (the write gate) applies the same precedence and also classifies a removed URI with no indexed row.
- Attacker-chosen URIs can store phantom subjects and roots, so every reference check is a positive EXISTS/JOIN, never "NOT EXISTS a deleted row".

### Read path: unread count, updateSeen, preferences

- `CountUnread`: unread bound written as `n.sort_at > COALESCE((SELECT seen_at ...), '-infinity')` with no `notification_state` join; `LIMIT 1` when `seen_at` is NULL (only the newest visible row is unread), else 101. Keep the bound in this form: an OR'd or joined `seen_at` scans the whole read history. `TestNotificationUnreadCount_SeenAtBoundsRecipientIndexScan` pins it.
- `UpdateSeen`: `INSERT ... LEAST($2, NOW()) ON CONFLICT DO UPDATE SET seen_at = GREATEST(old, LEAST(new, NOW()))`, `$2` sent in UTC (Postgres rejects offsets beyond ±15:59). Never writes `disabled_reasons`. The read sweep trusts `seen_at`, so the future clamp is load-bearing. Handler parses with Indigo `syntax.ParseDatetimeTime`.
- Preferences: `GetPreferences` returns all true without a state row; `PutPreferences` is one upsert that merges named reasons, drops NULLs, never writes `seen_at`, and returns the full object. The `[]string{}` literals are load-bearing (nil would bind NULL). Handler uses `xrpc.DecodeJSON(..., reqbody.LimitTiny, ...)`.
- A caller with no `users` row: 23503 on `notification_state_did_fkey` → `notifications.ErrAccountNotIndexed` → 400 `AccountNotIndexed` (both `putPreferences` and `updateSeen`).

### Read path: listNotifications

- SQL (`notification_list.go`): one REPEATABLE READ read-only tx: `seen_at`, then the page, then the upvote aggregate (only when the page has upvote rows). Page WHERE is `CASE WHEN (visible) THEN true ELSE false END` plus keyset `(n.sort_at, n.id) < ($2, $3)`, `ORDER BY n.sort_at DESC, n.id DESC LIMIT limit+1`. The CASE is a planner estimate barrier; keep it (`TestNotificationList_RecipientSortIndexWithMostlyBlocked` catches a regression). Every reason is listed.
- `isRead`: `sort_at <= seen_at`, or with NULL `seen_at`, every row except the newest visible row of any reason.
- Per-reference state and current CID: `ListedNotification.RootPost/Subject/Record` (`ListedReference{State, CID}`). Subject CID = `COALESCE(subject_post.cid, subject_comment.cid)`; record = post or comment. `listedReference()` errors on NULL or unknown state. Not-applicable positions are skipped by reason: mentions have no subject, upvotes have no record and no actor.
- Upvote aggregate (`buildListNotificationUpvotesSQL`): per page subject, `COUNT(*)` of qualifying votes + `bridgedUpvoteTotalSQL`, and up to 3 voters ordered `indexed_at DESC, id DESC`.
- Cursor (`cursor.go`): RawURL base64 of `<UTC RFC3339Nano>|<id>`; encoded length > 96 rejected. A malformed cursor wraps `ErrInvalidCursor`; the handler returns a fixed `InvalidCursor` 400 message and never echoes or logs the input.
- Hydration (`list_service.go`): one `repo.List`, then at most one each of `GetByDIDs` (authors and voters; unindexed → DID-only `profileView`), `GetViewsByURIs(..., "")` (live post roots, post subjects, post-mention records; anonymous viewer), `GetByURIsBatch` (live comments). `GetByURIsBatch` returns deleted comments, so a live-classified comment with `DeletedAt` set is treated as missing.
- A live reference that is missing at hydration omits the row (never half-hydrated). One `WarnContext` per request, "notifications: omitted unhydratable rows", with counts by reason and cause, never identifiers.
- Rendering: placeholders carry `status` and no title, preview, excerpt, community, labels or thumbnail; strong-ref CIDs come from `List`. `rootPost` is always emitted. Times are `UTC().Format(time.RFC3339Nano)`; clients echo the largest `sortAt` into `updateSeen`, so never truncate. Empty pages render `[]`. A page can hold fewer than `limit` rows (even zero) while `cursor` is present.
- Text (`notificationPostText`): title unless blank (whitespace-only), else body excerpt (140 graphemes, `notificationExcerpt`), except image posts (stored `images` or `images#view` embed), which get no body text. `rootPost.title` is omitted when blank and never falls back to the body. Post-mention `record.excerpt` = title, else body excerpt.
- Labels: post views from `Record["labels"]`, comments from parsed `ContentLabels`; values with `val` > 128 bytes dropped, first 10 kept; key omitted when empty. A live comment reference whose `ContentLabels` does not parse omits the whole row (counted as `malformed_labels`).
- Thumbnails: `posts.PreviewThumbnail(view)` (`internal/core/posts/notification_thumbnail.go`) is the only source. Empty when the image proxy is disabled or its base (`CDNURL` over `ProxyBaseURL`) is not an absolute http(s) URL; only `social.coves.embed.external` with a blob `external.thumb` or `social.coves.embed.images` with a blob `images[0].image`; CID checked (≤256 chars, `cid.Decode`, `imageproxy.ValidateCID`); the projected URL must carry the `#view` stamp and pass `syntax.ParseURI`. It mutates `view.Embed`, so call it once per view before anything else projects it. Alt from `images[0].alt`, bounded to 1,000 graphemes / 10,000 bytes, omitted when blank. Comments and video get none.

### Lexicons (`internal/atproto/lexicon/social/coves/notification/`)

- `defs.json`: `reason` (open, knownValues `postReply`, `commentReply`, `mention`, `upvote`); `notificationView` with `reason`, `sortAt`, `isRead`, required `rootPost`, optional `subject`, `record`, `author`, `upvoteCount`, `recentUpvoters` (max 3); `rootPost`/`subject`/`record` each carry `uri`, `cid`, `status` (knownValues `deleted`, `removedByModerator`, `removedByServerAdmin`), `labels` (`com.atproto.label.defs#selfLabels`), `thumbnail`, `thumbnailAlt`; `preferences`.
- `getUnreadCount.json` (count 0–101), `listNotifications.json` (error `InvalidCursor`), `updateSeen.json` and `putPreferences.json` (error `AccountNotIndexed`), `getPreferences.json`.
- `TestNotificationListLexicon_Contract` (`tests/lexicon_notification_list_test.go`) checks view field names against the lexicon; a new view field needs a lexicon property.

### Wiring (`cmd/server`)

- `wiring.go` `buildRepositories`: `a.notificationRepo` (with `WithBridgedUpvoteTotals()` when trusted hosts are set) and `a.notificationRetentionSweeper` (type assertion, checked at compile time by `var _` assertions in the postgres package). Later: `notificationService`, `notificationListService` (from `userRepo`, `postRepo`, `commentRepo`), `preferencesService`, all by unchecked assertions on the same repo.
- `routes.go`: `RegisterNotificationRoutes` (getUnreadCount, updateSeen), `RegisterNotificationListRoutes`, `RegisterNotificationPreferenceRoutes` from `internal/api/routes/notification.go`.
- T2 contracts (`tests/e2e/notification_contract_test.go`) cover replies, mentions (comment and postv2), upvotes (including self-upvote never notifying) and mark-seen. Bridged totals have no T2: the hermetic stack has no bridge.

## 4. Operations

- **Migrations**: `053_notifications.sql`, `054_votes_upvote_history_index.sql`, `055_notification_public_post_withdrawals.sql`. 054 builds an index `CONCURRENTLY` outside a transaction on the large `votes` table; if interrupted it leaves an INVALID index that must be dropped by hand (`DROP INDEX CONCURRENTLY idx_votes_voter_subject_upvotes`) before retrying, since there is no `IF NOT EXISTS`.
- **Activation**: `notification_activation.activated_at` is set when 053 runs. Records created before it never notify; there is no feature flag.
- **Lexicons**: the `social.coves.notification.*` schemas are published in their own release step, `docs/LEXICON_PUBLISHING.md` section 4.
- **Routing**: no Caddy change. The `@appview` matcher already sends every `/xrpc/*` path to the AppView.
- **Bridged totals**: notify and count only when `TRUSTED_BRIDGE_PDS_HOSTS` is non-empty. The setting is read at boot; changing it needs a restart. Turning it off makes the hourly sweep delete bridged-only groups for good.
- **Thumbnails**: require the image proxy enabled (`IMAGE_PROXY_ENABLED`) with an absolute `IMAGE_PROXY_BASE_URL` or `IMAGE_PROXY_CDN_URL`. With the proxy off (dev, or production with `AllowUnproxiedMedia`) notifications carry no thumbnails; feeds are unchanged.
- **Retention job** starts with the server, hourly. Watch for `notification retention sweep failed`. On shutdown, a mid-statement cancel can surface as SQLSTATE 57014 and log one spurious error.
- **Clients**: render `status` placeholders, blur by `labels`, page while `cursor` is present (pages may be short), echo the newest `sortAt` verbatim into `updateSeen`.

## 5. Known limitations

Accepted for now; changing any of them is a product call.

- Community `contentWarnings` are not applied to previews (postponed until community NSFW lands in the NSFW system).
- No video thumbnails.
- Whitespace-only titles count as no title only in notification previews. The posts service accepts whitespace-only titles, and feed, thread and `post.get` views return them verbatim.
- A removal that lands before the root post is indexed leaves a `removed` admission row with no posts row, so comments on that post are not gated at write time ("unindexed is live").
- A zero or epoch `seenAt` turns a never-seen user's state into "everything unread".
- Feeds, `post.get` and comment views serve forged `#view` embeds and string thumbnails from firehose records. Notification thumbnails reject these; other read paths do not.
