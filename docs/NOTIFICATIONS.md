# Notifications

Formerly `docs/PRD_NOTIFICATIONS.md` and `docs/NOTIFICATIONS_IMPLEMENTATION.md`. Code comments cite this file by section heading and tests cite decision Q4, so keep both stable.

The AppView tells a user when someone replies to their post or comment, mentions them, or upvotes their post or comment. Delivery is in-app only: no push, no email, and clients poll. Fan-out is `internal/core/notifications`, the tables are migrations 053 to 055, and the API is `social.coves.notification.*`: `listNotifications`, `getUnreadCount`, `updateSeen`, `getPreferences` and `putPreferences`. Every endpoint is behind `RequireAuth`, and the recipient is always the caller. Web and mobile rules are in [plans/NOTIFICATIONS_CLIENTS.md](plans/NOTIFICATIONS_CLIENTS.md).

Notifications are AppView-local derived state, as in Bluesky. A user reading through another AppView sees none, and nothing before activation is backfilled.

## Reasons

- `postReply`: a top-level comment, to the root post's author.
- `commentReply`: a reply to a comment, to the parent comment's author. When that author also wrote the post, they get `commentReply` only.
- `mention`: a user `#mention` facet in a comment or postv2 post, to each mentioned user, on create and when an edit adds the mention.
- `upvote`: one group per post or comment, to its author. Downvotes never notify.

`reason` is an open set on the wire.

## Recipients

A postv2 URI's repo DID is its author, and so is a comment's. Legacy `social.coves.community.post` events are no longer consumed, but legacy rows remain, so a reply or upvote on a legacy post takes the author from `posts.author_did`. If that row is missing, the event notifies nobody, mentions included. Mentions are fanned out only from comments and postv2 posts, and only when the thread root is a post.

One record notifies each recipient at most once. The reply recipient never also gets a mention from that record, after edits and re-creates too. The rule keys on who the reply recipient is, not on whether their reply row exists, so a reply row lost to retention or a lifted block does not reopen the mention. Mentions of the author or of a community DID are dropped.

Nobody is notified of their own action, which covers the author's automatic upvote on every create. A recipient gets nothing when they have no `users` row, are erased, are an aggregator, or are hosted on a trusted bridge PDS, or when either party blocks the other at write time. A block created later hides existing rows at read time but does not undo a bump. An erased actor notifies nobody. Community blocks and community bans suppress nothing: a ban gates only post admission, and a banned user's comments stay visible in threads.

## Activity time

`sort_at` is the AppView's index time and drives both ordering and unread. Bluesky uses `least(createdAt, indexedAt)`; Coves does not, because it indexes late as a matter of course (dead-letter redrives, lagging external PDSes, Jetstream cursor rewinds), and under Bluesky's rule a late reply would arrive already read. A record's `createdAt` never affects ordering, so a future-dated record cannot pin itself to the top. `record_created_at` is for display only.

Writes stamp `sort_at` with `clock_timestamp()`, not the transaction-start `now()`, so time spent waiting on locks does not make a row look older than a `seen_at` set meanwhile. An `updateSeen` landing between the insert and the commit still marks the row read without it being shown. That window is milliseconds and is accepted.

## Time gates

The migration writes one `notification_activation` row at deploy time. A record whose `createdAt` is before it never notifies, which keeps redrives and first-time indexing of older activity quiet. A missing row is an error (`ErrNotificationActivationMissing`), never "no cutoff". There is no feature flag.

The freshness gate drops a record whose `createdAt` is more than 7 days before index time, bounding cursor rewinds and first-time indexing of an old repo. Exactly 7 days still notifies.

A mention added by an edit is fresh when the edit's Jetstream event time is within 7 days of index time; with no event time, freshness passes. Activation still uses the stored `createdAt`, so editing a record created before launch never notifies. An active re-create uses the pre-update `created_at` and stores it as `record_created_at`.

A `createdAt` that is not RFC 3339 parses as now and passes both gates. This is a known backlog issue. The consumers clamp a future `createdAt` to now.

## Mentions

Edits diff the new facets against the stored ones, and only newly added DIDs notify. Removing a mention retracts nothing. `richtext.MentionedDIDs` stops at the facet parse bound and returns the dropped count, which the `Fanout*` callers log.

A record holds at most `MaxMentionsPerRecord` (10) mention rows, in total, across its create, every edit, re-create and resurrection (decided 2026-09-29). The budget is 10 minus the record's existing mention rows, read inside the writing transaction; recipients who already hold a row are skipped, and the rest are cut in facet order. Rows kept on a deleted record still count when it comes back. A slot freed by retention or erasure can be used again, so re-adding a mention after retention pruned its row notifies again.

## Upvote groups

There is one row per (recipient, subject), with no actor or record. Counts and names are derived at read time from live `votes` rows, so they cannot drift.

Q4: a qualifying upvote is a live `up` vote whose voter is not the recipient, not erased, not an aggregator, and not blocked either way. A voter on a trusted bridge PDS counts. `qualifyingUpvoteSQL` in `notification_repo.go` defines it for `upvoteCount`, `recentUpvoters`, the alive rule and emptiness. `FanoutVoteCreate` applies the same Q4 rules in Go before a bump, so a Q4 change must update both.

A voter bumps a group only with their first upvote on that subject, ever (decided 2026-09-29). Removing and re-adding an upvote does not bump again, and a group deleted when the voter un-voted is not recreated by the re-vote. An earlier downvote does not count. `EarlierUpvoteExists` checks for any other `up` vote by the voter on the subject, live or soft-deleted. Hard deletes of votes reopen the first bump: erasure, as expected, and `cmd/reindex-votes`, after which every voter could bump again.

A bump raises `sort_at` with `GREATEST`, never lowers it, and marks the group unread. `root_post_uri` is written on insert only, so a comment resurrected under a different root repoints its group through `ReplaceUpvoteGroupRootTx`. A vote on a comment whose stored root is not a post never bumps.

Every other committed change to a subject's vote set asks to delete the group if it is empty. `upvoteGroupAliveSQL` is the one alive rule: a qualifying native upvote exists, or the item's bridged total is above 0. DeleteIfEmpty, both statements of the empty-group sweep and the read predicate use it. The list aggregate uses `qualifyingUpvoteSQL` plus `bridgedUpvoteTotalSQL`, and `SweepHiddenReferenceNotifications` skips the alive rule. The caller of DeleteIfEmpty must hold the subject's posts or comments row lock. At READ COMMITTED a DELETE that waits on a concurrent bump evaluates its subquery against its original snapshot and would delete a group whose new upvote is live.

`recentUpvoters` lists up to 3 native voters by `votes.indexed_at` descending, ties by higher vote ID, not by the vote's `createdAt`. A voter who re-votes is named first, but the group does not move.

## Bridged upvote totals

Decided 2026-10-02 and 2026-10-03. The bridged-vote poller stores each native item's Lemmy upvote total (via Tidepool) in `bridged_upvote_count`. Totals have no voters, so they add to `upvoteCount` and never appear in `recentUpvoters`. A group with only bridged upvotes has a count and no names. Coves treats bridged and native votes as disjoint and does not deduplicate.

- A group bumps only when a poll finds the total above the item's high-water mark, `GREATEST(bridged_upvote_peak, bridged_upvote_count)`, once per new high. 5, 3, 5, 4, 5 notifies nothing; 6 then notifies once.
- Migration 053 backfilled the peak from the stored totals, so totals present at launch notify nobody.
- Activation, freshness and `asOf` age do not apply. An increase observed after launch bumps even on old content.
- Recipient gates and the deleted or removed gate apply. Self, erased-actor and block rules have no actor to check.
- A decrease asks to delete the group if empty.
- A spurious spike raises the peak for good. After a group is deleted at zero, a climb back only to the old peak recreates nothing.

Only the poller writes groups from totals, in `BridgedVotesRepository.ApplyAggregate`: one READ COMMITTED transaction that locks the item row, applies the guarded aggregate update and raises the peak, then applies the group change. A stale `asOf` changes nothing, and a failed notification write rolls back the aggregate. The poller only visits items inside its lookback (90 days by default). The Jetstream `bridgedStats` path is not hooked, because it carries totals only for bridge-PDS authors, who are never notified.

The gate is the whole `TRUSTED_BRIDGE_PDS_HOSTS` setting, not a host. `cmd/server` builds the repository with `WithBridgedUpvoteTotals()` when the list is non-empty, and the setting is read at boot. While any host is listed, every stored total counts, including one from a bridge since removed, which stays counted and frozen because the poller no longer dials it. There is no provenance column. With the list empty, totals are not counted and do not keep groups alive, so the hourly sweep deletes bridged-only groups permanently; turning the gate back on does not restore them. A compromised trusted bridge can bump once per poll by reporting a rising total, capped by `bridgedvotes.MaxBridgedCount`.

## Deleted and removed content

Decided 2026-10-01, extended to admin removals 2026-10-06. Notifications about deleted or removed content stay as placeholders, and that content gets no new notifications.

- An author delete keeps every row, including on a zero-row soft delete.
- The API returns a `status` on the reference (`rootPost`, `subject` or `record`), never placeholder text: `deleted`, `removedByModerator` or `removedByServerAdmin`. A reference with a status carries no title, community, preview, excerpt, labels, thumbnail or, on `rootPost`, author, createdAt or stats; its CID is the record's current one.
- Any active instance-scope admin removal gives `removedByServerAdmin`, even alongside a community removal. Community removals alone give `removedByModerator`. An author's delete wins over both. Label decisions change nothing.
- A comment whose root post has an active admin removal reads the root's status, so a reply's text never shows inside a removed thread. A live reply under an author-deleted or community-withdrawn root keeps its own excerpt.
- Placeholder rows are visible rows: they are listed, count as unread, and follow retention. Blocks and preferences still hide them.
- An upvote group on a deleted or removed item keeps its count and never bumps again.
- Restoring a removal does not backfill notifications for activity during it.
- Account erasure deletes rows outright. It is a privacy deletion, not a placeholder.

A post shows a placeholder only if it was publicly visible at the moment it was deleted or removed. Columns cannot tell that afterwards, so the withdrawing transaction records it in `notification_public_post_withdrawals` (migration 055, postv2 only, no foreign key):

- `authorDelete` is written in the tombstone transaction by `RecordPostAuthorDeleteWithdrawalTx`, after the soft delete, when the post passes `admittedPostsPredicate` for the anonymous viewer. A post soft-deleted only by `compensateAuthorDelete` gets no marker and stays hidden.
- `communityWithdrawal` is written by `compareAndSwapWithWithdrawal` in `admission_repo.go`, inside the admission transaction of `ApplyAcceptanceDelete` and `ApplyRemoval`. It locks the admission row, decides whether the post was public and undeleted just before the write, and upserts the marker with the event's rev. A removal commit is an acceptance delete plus a removal put at one rev, applied in either order, so a non-public `ApplyRemoval` keeps a marker with the same rev and deletes any other. A marker failure rolls back the admission write. This is the one write notifications make into another domain.
- An admin removal does not stop either marker.
- Lifting a removal needs no marker change. The read rule requires `status = 'removed'`, so a stale marker is ignored.

Legacy posts never get markers, so a deleted or removed legacy post is hidden, not a placeholder. Nearly all of them are aggregator posts.

A post that was never public (pending, rejected, awaiting re-acceptance, missing its admission row, or CID-mismatched) still gets rows written; they stay hidden until the post is accepted and then appear with their original `sort_at`, possibly already read. Deleted or removed while in that state, the post stays hidden.

## Write path

A notification change runs inside the index transaction that caused it, and only when that mutation applied. Rev-gated replays, unchanged CIDs and rejected events change nothing. Every early-commit branch in the comment and vote consumers writes its notifications before it commits. Consumers depend on `notifications.Repository` and never import the service: the pure `Fanout*` functions in `fanout.go` return intents, and `ApplyTx` and `ApplyUpvoteGroupTx` write them. Record-keyed rows insert `ON CONFLICT DO NOTHING`, so duplicate delivery, direct fetch and redrive are idempotent.

The write gate reads reference state through `Lookups.ReferenceStates` and refuses fan-out when the record, its root, its parent or the vote subject is deleted, community-removed, admin-removed or not indexed. A comment that arrives before its root or parent loses its notifications, and nothing backfills them (decided 2026-10-06); otherwise a comment pointing at a nonexistent post could plant rows that stay hidden forever. Pending and rejected posts read live here. This is deliberately not the read-time classification.

Every notification-writing consumer transaction runs at READ COMMITTED, and `ErasureGateTx` refuses any other level. It takes the shared advisory lock on the actor's erasure key, then checks `deleted_accounts` in a separate statement. It runs right after the rev check wins and before any content row is locked; moving it later deadlocks with erasure. Lock order everywhere is subject row, then notification row, then the `users` foreign key.

A recipient erased mid-fan-out raises 23503 on `notifications_recipient_did_fkey`. `ApplyTx` and the group upsert skip that recipient inside a savepoint and keep the caller's earlier writes. Any other error aborts the transaction, and the event is retried or dead-lettered. `ApplyTx` writes all intents in one `unnest` INSERT under one savepoint, because Postgres caches only 64 subtransaction IDs per backend; it falls back to per-intent savepoints only on that 23503. An unparsable thread URI means no recipient, never an error, so a malformed record cannot stall a Jetstream lane.

## Read-time visibility

`notificationVisibility` in `notification_visibility.go` builds the one predicate, used by both `List` and `CountUnread`, so list and count always agree. Extend it there. Post references are classified with `admittedPostsPredicate` bound to the anonymous viewer, never the recipient, so a pending post cannot leak its preview even to its author. A row is visible when no reference it needs reads hidden, neither party blocks the other (per voter for groups), an upvote group is alive, and the reason is not disabled.

Attacker-chosen URIs can name phantom subjects and roots, so every reference check is a positive EXISTS or join, never "no deleted row exists". The list query wraps the predicate in `CASE WHEN ... THEN true ELSE false END` as a planner estimate barrier; without it the planner abandons the recipient sort index. `TestNotificationList_RecipientSortIndexWithMostlyBlocked` catches that.

## Seen state

Each user has one monotonic `seen_at`, as in Bluesky, with two changes (decided 2026-09-28). The client sends the largest `sortAt` it displayed, not its last poll time, and the server stores `GREATEST(seen_at, LEAST(seenAt, now()))`. It never moves backwards or ahead of server time. The read sweep trusts `seen_at`, so the clamp is load-bearing.

Unread is `sort_at > seen_at`. A user who has never marked anything seen has a NULL `seen_at`, and only their newest visible row is unread, so a new user is not shown a huge count. A zero or epoch `seenAt` turns that into everything unread.

`getUnreadCount` stops at 101, meaning "100+". Keep its bound written as `n.sort_at > COALESCE((SELECT seen_at ...), '-infinity')` with no join: a joined or OR'd `seen_at` scans the whole read history. `TestNotificationUnreadCount_SeenAtBoundsRecipientIndexScan` pins it.

`listNotifications` reads `seen_at`, the page and the upvote aggregate in one REPEATABLE READ transaction and returns the stored `seenAt`. Clients judge deeper pages against page one's `seenAt`, so the `updateSeen` sent after page one does not mark them read.

`updateSeen` and `putPreferences` return `AccountNotIndexed` (400) for a caller with no `users` row.

## Listing

Paging is keyset on `(sort_at DESC, id DESC)`. The cursor is base64url of `<RFC3339Nano UTC>|<id>`, and a malformed one is `InvalidCursor` (400), never echoed or logged. `limit` is 1 to 100, default 50. A bumped group moves above the cursor and is skipped by later pages; clients reload page one.

Hydration is batched per page, never per row. A reference the list query saw as live but that is missing or deleted at hydration omits the whole row; rows are never half-filled. So a page can be short, even empty, while `cursor` is present. One warning per request, "notifications: omitted unhydratable rows", gives counts by reason and cause, never identifiers. Times are RFC 3339 with nanoseconds; never truncate them, because clients echo `sortAt` into `updateSeen`.

## Previews

Decided 2026-10-03.

- A post preview is its title, or when the title is absent or whitespace-only, a body excerpt. An image post without a real title has no text, only its thumbnail. `rootPost.title` is omitted when blank and never falls back to the body. Excerpts are cut at 140 grapheme clusters.
- Each reference carries its own author's self-labels in `labels`, unfiltered and not inherited between positions. Values over 128 bytes are dropped and the first 10 kept, to stay valid against `com.atproto.label.defs#selfLabels`. A live comment whose stored labels do not parse omits the row rather than show unlabeled text.
- A post reference may carry `thumbnail`: the first image of an image post or the link card's thumbnail. `posts.PreviewThumbnail` is the only source. It projects only a blob the stored record carries with a valid CID through the image proxy, and rejects an embed already claiming the served `#view` shape or a URL string in the record. Feeds, `post.get` and comment views do not reject those. Comments, videos and quoted posts get none. With the image proxy disabled, as in dev, there are no thumbnails.
- `thumbnailAlt` is the first image's alt text, bounded to 1,000 grapheme clusters and 10,000 bytes and omitted when blank. Link cards have none.
- A live `rootPost` also carries the post's `author`, `createdAt` and `stats`, taken from the same post hydration; all three are absent when the root post has a status.
- Community `contentWarnings` are not applied. That waits until community NSFW is part of the NSFW system.

`TestNotificationListLexicon_Contract` checks view field names against `defs.json`, so a new view field needs a lexicon property.

## Preferences

Each reason can be turned off. Preferences are AppView-local, not a PDS record, and default to all on. `putPreferences` takes a partial update and returns the full object. They filter at read time only: rows are still written while a reason is off, so turning it back on shows them again.

## Retention

The policy is the one Bluesky's AppView ran while it was Postgres-backed (atproto PR 1893, removed in early 2024). A job in `cmd/server` runs hourly; the constants are in `notification.go`.

- Read rows go once `sort_at` is more than 30 days before `seen_at`. A user who never marked anything seen uses their newest row of any kind as the reference, so a hidden newest row can delete their only visible unread row.
- Unread rows go only for users with a `seen_at` and more than 500 unread, and only rows more than 180 days older than their newest.
- Empty upvote groups go.
- Rows whose required reference reads hidden go once `sort_at` is more than 7 days old (decided 2026-10-06). Rows hidden only by a block, a preference or the alive rule stay. A reply to a post still pending after 7 days is lost.

Each sweep deletes at most 10,000 rows per transaction with `FOR UPDATE SKIP LOCKED` and loops while batches come back full. A failing sweep logs `notification retention sweep failed` and the next sweep still runs.

## Erasure

`user_repo.go` `Delete` takes the exclusive erasure lock, writes the `deleted_accounts` marker, deletes content, then deletes notifications where the user is recipient or actor, their withdrawal markers and their `notification_state`, then the `users` row. Deleting notifications before content deadlocks with a consumer that holds a content row; a test pins the order. Groups the user voted in are left to the read rule and the empty-group sweep. Other users' rows keep `root_post_uri` pointing at the erased author's post, as `comments.root_uri` does.

## Operations

- No new env vars. Bridged totals depend on `TRUSTED_BRIDGE_PDS_HOSTS`, and thumbnails on `IMAGE_PROXY_ENABLED` with an absolute `IMAGE_PROXY_BASE_URL` or `IMAGE_PROXY_CDN_URL`.
- Migration 054 builds `idx_votes_voter_subject_upvotes` `CONCURRENTLY` outside a transaction, with no `IF NOT EXISTS`. If interrupted, drop the INVALID index by hand (`DROP INDEX CONCURRENTLY idx_votes_voter_subject_upvotes`) before retrying.
- A new migration after 059 needs a `MigrateDownOne` step at each rollback site in `internal/db/postgres` (grep `MigrateDownOne(t, db, 59`) and an update to the "035 through 059" comment in `admission_repo_schema_test.go`.
- The lexicons go out in the notifications release in [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md). Caddy needs no change: `@appview` already routes `/xrpc/*`.
- T2 covers replies, mentions, upvotes and mark-seen with real sign-in. Bridged totals have no T2, because the hermetic stack has no bridge.
