# In-App Notifications PRD

Status: Backend implemented 2026-10 (see [NOTIFICATIONS_IMPLEMENTATION.md](NOTIFICATIONS_IMPLEMENTATION.md) for decisions made during the build). Draft 3 (2026-10-01) added decision 7; decisions 8 and 9 were added 2026-10-02 to 2026-10-04.
Repos: `coves` (backend + lexicons), `coves-frontend` (SvelteKit), `coves-mobile` (Flutter)

## Summary

Tell users when someone replies to their post, replies to their comment, @mentions them, or upvotes their content. Notifications appear in-app on web and mobile: a list view plus an unread badge. Push delivery is out of scope.

Nothing exists today:
- **Mobile:** a placeholder `NotificationsScreen` is wired into the bottom nav as tab 3 (`lib/screens/home/notifications_screen.dart`).
- **Frontend:** only leftover Photon "inbox" i18n strings, and no route.
- **Backend:** no table, service, or lexicon. The only "notifier" is `internal/core/adminreports/notifier.go`, the operator Telegram alert, which is unrelated.

## Decisions made

Confirmed by the product owner on 2026-09-28:

1. **Delivery:** in-app only. No push of any kind.
2. **Events:** replies to my post, replies to my comment, @mentions, upvotes on my posts and comments. Downvotes never notify.
3. **Upvotes are grouped per item on the server:** one notification per post or comment ("alice and 12 others upvoted your comment"). A new visible upvote moves the row to the top and marks it unread.
4. **Per-type settings:** each of the four reasons can be turned off.
5. **Activity time is the AppView's index time.** This deliberately differs from Bluesky, which uses `least(createdAt, indexedAt)`. Coves regularly indexes records late: out-of-order references are dead-lettered and redriven on a timer, external PDSes lag, and Jetstream rewinds its cursor on reconnect. Under Bluesky's rule, a late reply would arrive already read.
6. **Seen state follows Bluesky's model, with two changes.** Like Bluesky, each user has one monotonic `seen_at`. Unlike Bluesky, the client sends the newest `sortAt` it displayed rather than its last poll time, and the server clamps `seenAt` to server time.

Confirmed by the product owner on 2026-10-01:

7. **Notifications about deleted or moderator-removed content stay visible as placeholders, and that content gets no new notifications.**

   | State of the post or comment | New notifications | Existing notifications |
   |---|---|---|
   | Comment deleted by its author | none | stay; the comment's excerpt is replaced by a placeholder ("[Deleted Comment]") |
   | Postv2 post deleted by its author (firehose tombstone) while publicly visible | none | stay; title and excerpt replaced by "[Deleted Post]" |
   | Postv2 post removed by a moderator (admission `status = 'removed'`) while publicly visible | none | stay; title and excerpt replaced by "[Removed by moderator]" |
   | Post pending, rejected, awaiting re-acceptance, missing its admission row, or CID-mismatched (never publicly visible in its current form) | written, hidden | hidden |
   | Post deleted or removed while not publicly visible; postv2 soft-deleted only by `compensateAuthorDelete`; legacy (pre-postv2) post soft-deleted or removed | none | hidden |
   | Account erased | n/a | deleted (erasure is a privacy deletion) |

   - "No new notifications" is enforced at write time (see Write-time rules). Restoring a removed post does not backfill notifications for activity during the removal.
   - Author deletes no longer delete notification rows. A re-created comment's surviving mention rows count toward its 10-mention cap, so the cap carries over.
   - An upvote group on a deleted or removed item keeps its count and stays listed, but never bumps again.
   - Placeholder rows are visible rows: they are listed, count toward `getUnreadCount`, and follow retention like any other row. Blocks and preferences still hide them.
   - A post shows a placeholder only if it was publicly visible in its current form at the moment it was deleted or removed. Existing columns cannot tell this afterwards, so the deleting or removing transaction records it (see Public-withdrawal markers). A post deleted or removed while pending or rejected stays hidden.
   - Legacy posts (`social.coves.community.post`, about 99% aggregator posts) are either live or hidden. No path records a marker for them, so a legacy post soft-deleted by the rematerializer or the legacy delete path, or removed by a moderator, is hidden. Live legacy posts are listed as before.
   - The API returns a status code, not placeholder text: each referenced view (`rootPost`, `subject`, `record`) carries a `status` and omits its title and excerpt. Clients render the bracket text. A placeholder row still opens the post thread, where the existing deleted and removed states render.

Confirmed by the product owner on 2026-10-02:

8. **Bridged upvote totals (Lemmy, via Tidepool) join the upvote notification, without names.** The bridged-vote poller stores each item's fediverse upvote total in `bridged_upvote_count`. These totals have no voters attached.
   - `upvoteCount` = qualifying native upvotes + the item's bridged upvote total. `recentUpvoters` lists only native voters. An item with only bridged upvotes shows a count and no names ("5 people upvoted your post"). A mixed item shows "alice, bob and 3 others". Bridged downvotes are ignored.
   - **Bump.** Each poll that finds the bridged total above the item's high-water mark (the highest bridged total it has ever reached, stored on the post or comment) moves the group to the top, marks it unread, and raises the mark. That is one bump per new high, however large the increase. A decrease, or a rise back up to the mark, only changes the count and never bumps: 5 → 3 → 5 → 4 → 5 notifies nothing, and 6 then notifies once.
   - **Launch baseline.** Totals already stored when this ships notify no one. Only increases observed after launch create or bump a group, and the count always includes the full total.
   - **No age gate.** The activation cutoff and the 7-day freshness gate do not apply to bridged totals, and neither does any check on the bridge's `asOf` age. The poller observes increases live, so any increase observed after launch bumps, even on an item created long before (product owner, 2026-10-03).
   - **Empty.** A group with no qualifying native upvote and a bridged total of 0 is deleted, the same rule as native delete-if-empty.
   - **Gates.** Every recipient gate applies: erased, aggregator, bridge-PDS and unindexed recipients get nothing. The self and block rules need an actor, so they do not apply. A deleted or removed item keeps its group and count but never bumps (decision 7).
   - **Trusted bridge only.** Only a bridge listed in `TRUSTED_BRIDGE_PDS_HOSTS` can create or bump a group. That is the poller's own and only dial list. With the setting empty, bridged totals do not count toward `upvoteCount`, do not keep a group alive, and never create or bump one.
   - **The gate is the whole setting, not per host** (product owner, 2026-10-03). While any bridge is configured, every stored bridged total counts and keeps its group alive, including totals last written by a bridge that has since been removed from the list. A removed bridge's totals never bump again, because the poller stops dialing it. There is no per-host provenance column.
   - Only the poller's path for natively authored content can reach an eligible recipient. Totals on bridge-authored records (`bridgedStats` via Jetstream) belong to bridge-PDS accounts, which are never notified.
9. **Previews carry their author's self-labels and a post thumbnail; blank titles count as none; upvoters are named in indexing order** (product owner, 2026-10-03).
   - **Labels.** Each preview (`rootPost`, `subject`, `record`) carries the self-labels its own author applied (`nsfw`, `spoiler`, `violence`), as feeds serve `record.labels`. The app blurs per the viewer's setting. The server hides nothing, and labels are not inherited between positions. A reply under an NSFW post carries the label on `rootPost` only, so the app can blur the whole notification. Unknown values pass through as in feeds. To stay valid against `com.atproto.label.defs#selfLabels`, values over 128 bytes are dropped and at most the first 10 are kept. For a list that already meets those bounds, a notification blurs exactly when the feed does; for an oversized list the notification's labels are bounded and can differ from the feed's (ten unknown values followed by `nsfw` lose the `nsfw`). Community `contentWarnings` are not applied; they are postponed until community NSFW is part of the NSFW system.
   - **Thumbnail.** A post preview carries an optional `thumbnail`: the link card's thumbnail for a link post, or the first image for an image post. The URL is built by the same image-proxy projection the feeds use, never from a raw blob reference, and only from an image the stored record actually carries as a blob with a valid CID. The URL must be an absolute image-proxy URL that passes the lexicon's URI check. An embed already claiming the served (`#view`) shape, a URL string inside the record, or a malformed or oversized blob CID never produces a thumbnail. The root post line (`rootPost`) carries its post's thumbnail too. Comments, video posts (for now) and quoted posts have none, and neither do links to image hosts that offer a gallery but no link-card thumbnail. Deployments running without the image proxy (development, or production with `AllowUnproxiedMedia`) show no notification thumbnails; their feeds are unchanged.
   - **Alt text.** An image post's thumbnail carries the first image's alt text (`thumbnailAlt`), so screen readers can announce something. It is set only when the thumbnail is, omitted when the alt is empty or whitespace-only, and cut to the images lexicon's alt limits (1,000 grapheme clusters, 10,000 bytes), so a hostile record cannot make the page fail validation. A link card's thumbnail has no alt text, because the link embed has none for its image.
   - **Blank titles.** A whitespace-only title counts as no title, so the preview falls back to the body excerpt, and `rootPost` omits its title. Exception: an image post with no title, or a blank one, shows only its thumbnail and no body text. An image post with a real title shows the title and the thumbnail. An untitled image post whose image can't be shown displays no text.
   - **Upvoter order.** `recentUpvoters` is ordered by the existing AppView indexing timestamp, descending; ties by higher vote ID. It is not ordered by the vote record's `createdAt`. Someone who removes an upvote and upvotes again is named first, but the notification does not move to the top, because a voter bumps an item's upvote notification only with their first upvote on it.
   - Deleted and removed references (decision 7) carry no labels, no thumbnail and no alt text.

## Non-goals

- Push notifications and email digests.
- Community-level events: moderator actions, new posts in subscribed communities, community (`!`) mentions, bans.
- Notifying users who read through another AppView. Notifications are AppView-local derived state, as in Bluesky.
- Backfill. Notifications start at the activation time (below).
- Naming bridged voters. Bridged upvotes arrive as per-item totals without actors, so they add to an upvote group's count and bump it (decision 8), but never appear in `recentUpvoters`. Notifications from the Jetstream `bridgedStats` record channel are also out, because its items belong to bridge-PDS accounts.
- Real-time transport. Clients poll.
- An authenticated T2 harness. It is a separate prerequisite; see Testing.
- Community content warnings in notification previews. They are postponed until community NSFW is part of the NSFW system (decision 9).

## Prerequisite

T2 cannot mint a sealed AppView session today (`docs/TEST_ARCHITECTURE.md` §3.4b and §6, items 1–2). Every endpoint in this PRD requires one. That work is being tackled separately. Until it lands, authenticated endpoint behaviour is proven at T1 (see Testing).

## Architecture

### Signal sources

All four events already flow through existing Jetstream consumers, and bridged upvote totals (decision 8) through the existing bridged-vote poller. No new wanted collections are needed, so `cmd/contract-manifest` is unaffected.

| Reason | Source record | Recipient | Hook points |
|---|---|---|---|
| `postReply` | Comment with `parent_uri == root_uri` | Post author | `createComment` → `indexCommentAndUpdateCounts`, including its resurrection branch |
| `commentReply` | Comment whose parent is a comment | Parent comment's repo DID | Same as `postReply` |
| `mention` | Comment or postv2 post with a user `#mention` facet | Each mentioned user DID | Comment: create path, plus `updateComment` for added mentions. Post: `insertAuthorPost` (covers Jetstream and the acceptance direct-fetch path at `authorpost.go:1281`), plus `applyPostContentUpdate` for added mentions |
| `upvote` | Vote row, any direction, create or replacement | Subject author | `indexVoteAndUpdateCounts`, including its stale-vote replacement branch, and `deleteVote` |
| `upvote` (bridged total) | A poll that changes a native item's stored `bridged_upvote_count` | Subject author | `BridgedVotesRepository.ApplyAggregate`, driven by the bridged-vote poller (`internal/core/bridgedvotes`) |

**Post author resolution.** A `social.coves.community.postv2` URI's repo DID is its author. Legacy `social.coves.community.post` events are dropped by `PostEventConsumer`, but legacy rows still exist, so replies to legacy posts resolve the author from `posts.author_did`. If that row is missing, the event produces no notification. Mentions are fanned out only from postv2 posts and comments.

**Community mentions.** A facet whose DID resolves to a community is ignored.

### Transactionality

A notification mutation runs inside the transaction of the index mutation that caused it, and only when that mutation actually applied. A rev-gated no-op, an unchanged-CID update, or a rejected event produces no notification change. A failure in notification code fails the transaction and uses the consumer's existing retry and dead-letter path.

The hook functions above commit at several points, including early commits in the vote consumer's branches and the comment resurrection branch. Each commit point that applies a qualifying mutation must include the notification mutation before it commits. The consumers call `notifications.Fanout(...)` for pure intent computation and `Repository.ApplyTx(ctx, tx, intents)` for the writes. They never import the service.

The poller's `ApplyAggregate` follows the same rule. In one READ COMMITTED transaction it locks the item row, reads the stored total, applies the guarded aggregate update, and then writes the group change. A stale `asOf` that loses the guard changes nothing. A failed notification write rolls back the aggregate, and the batch is retried on the next sweep.

### Activation cutoff

The migration writes a single row, `notification_activation(activated_at = NOW())`. Fan-out ignores any source record whose `createdAt` is earlier than `activated_at`. This stops redrives and first-time indexing of pre-deploy activity from producing notifications, even when that activity is recent.

**Bridged-total exception (decision 8).** A bridged-total increase observed by the poller is not subject to the activation cutoff. It has no source record, and neither the item's `createdAt` nor the bridge's `asOf` is compared with `activated_at`. The launch baseline is the total stored when this ships: only a polled total greater than the stored total is an increase. The recipient rules and the deleted/removed rule still apply (Write-time rules).

### Freshness gate

After activation, fan-out also ignores source records whose `createdAt` is more than 7 days before index time. This bounds cursor rewinds, redrives, and first-time indexing of an older repo.

**Bridged-total exception (decision 8).** A bridged-total increase observed by the poller is not subject to the freshness gate. It bumps whatever the item's age and whatever the age of the bridge's `asOf`, as long as that `asOf` passes the stored-`asOf` guard (Transactionality). The recipient rules and the deleted/removed rule still apply (Write-time rules).

Mentions added by an edit are gated on the edit's index time, not the record's original `createdAt`. A new mention on an old record still notifies.

### Write-time rules

A notification row is not written, and an upvote group is not bumped, when:

- The actor is the recipient. This covers the automatic author upvote on every create.
- The recipient is ineligible:
  - not in `users`, or in `deleted_accounts`;
  - an aggregator (`aggregators.did`);
  - hosted on a bridge PDS according to `BridgeTrust`. The comment and post consumers already carry `BridgeTrust`. The vote consumer gains a `WithVoteBridgeTrust` option.
- The actor is erased (`deleted_accounts`). The comment consumer has no erasure gate today, so this check is new.
- Either party blocks the other at write time. A block created later is enforced at read time, but it does not undo a bump that already happened.
- For `mention`: the recipient already gets a `postReply` or `commentReply` for the same record. One record produces at most one notification per recipient.
- For a comment whose parent comment author is also the post author: `commentReply` only.
- For mention edits: the new facet's mentioned DIDs are compared with the stored facets. Only DIDs that were not previously mentioned notify. Removed mentions do not retract notifications.
- **Deleted or removed content (decision 7).** The subject, the source's parent or the root post is author-deleted or moderator-removed, or the subject or parent comment is deleted. A comment's parent can be a post other than its root, so the comment gate checks the root and the parent separately. This is read inside the writing transaction from existing columns:
  - a deleted comment has `comments.deleted_at` set;
  - a deleted post has `posts.deleted_at` set, whatever its collection;
  - a removed post's own-community admission row (the `visiblePostsPredicate` join key) has `status = 'removed'`.

  The gate does not read the public-withdrawal markers: a post deleted or removed while pending gets no new rows either.

  It applies to every fan-out path: comment create, resurrection and edit mentions, post create and edit mentions (a removal can arrive before the post is indexed), and the upvote bump on create and replacement. Group maintenance is not gated. It also applies to a bridged-total increase (decision 8): a withdrawn subject or root keeps its group and count but is never bumped. Pending, rejected and other never-public posts are not gated; their rows are written and hidden at read time. Restoring a removed post does not backfill.

For a bridged-total increase (decision 8), only the recipient rules and the deleted/removed rule apply. There is no actor, so the self, erased-actor and block rules have nothing to check. The activation cutoff and the freshness gate are the only gates it skips (see their bridged-total exceptions).

### Upvote group maintenance

Maintenance is separate from notification production. It runs after **every** committed change to the vote set of a subject: create, stale-vote replacement (including a replacement by a downvote), and delete.

- The changed vote is a live `up` from a qualifying voter (not self, eligible, not blocked) on a subject that is not deleted or removed: upsert the group and set `sort_at = GREATEST(sort_at, now)`. A group on a deleted or removed item keeps its count but never bumps.
- Any other change (downvote replacement, retraction): if no live qualifying upvote remains on the subject and its bridged upvote total is 0, delete the group. Never move `sort_at` backwards.
- **Bridged totals (decision 8).** These run in the poller's aggregate transaction, comparing the new total with the stored one:
  - An increase on an item that is not deleted or removed, for an eligible recipient, upserts the group and sets `sort_at = GREATEST(sort_at, now)` only when the new total exceeds the item's high-water mark. It bumps once per new high.
  - A decrease never bumps. It deletes the group by the rule above, so only when no qualifying native upvote remains and the total is now 0.
  - An unchanged total, and a stale `asOf`, change nothing.
  - "Alive" (at least one live qualifying upvote, or a bridged total above 0) is one SQL helper. Delete-if-empty, the empty-group sweep and the read-time rule all use it.
  - With `TRUSTED_BRIDGE_PDS_HOSTS` empty, the helper ignores bridged totals. Otherwise it counts every stored total, whichever bridge wrote it. It never filters by host.

The consumer does not handle `update` operations or URI conflicts. A "direction flip" therefore only exists as the replacement branch or as delete plus create, and must be tested through those paths.

### Read-time rules

Applied identically by `listNotifications` and `getUnreadCount`, in SQL before pagination:

- **Post visibility (decision 7).** The subject post, the source post, and the root post of any comment are each classified using the existing admission-aware predicate (`internal/db/postgres/post_visibility.go`) bound to the **anonymous viewer**, not the recipient. The first matching state applies:
  1. **Removed by moderator:** the post's own-community admission has `status = 'removed'`, and the post has a `communityWithdrawal` marker (see Public-withdrawal markers). Listed as a placeholder.
  2. **Live:** not deleted, and the predicate passes. Listed normally.
  3. **Deleted:** `deleted_at` is set, and the post has an `authorDelete` marker. Listed as a placeholder.
  4. **Hidden:** anything else, including pending, rejected, awaiting re-acceptance, missing-admission and CID-mismatched postv2 posts (even from their own author), posts deleted or removed in one of those states, postv2 posts soft-deleted only by `compensateAuthorDelete`, deleted or removed legacy posts, and posts that are not indexed. The row is not listed or counted.
  - A pending post that mentions someone does not leak its preview.
  - If a pending post is later accepted, its rows become visible with their original `sort_at`, so they may already be marked read. This is accepted.
  - Rows written before a removal stay as placeholders. Write-time rules stop new rows during the removal, so third-party writes straight to the PDS produce nothing new.
- **Comment visibility (decision 7).** Source and subject comments must be indexed. A comment with `deleted_at` set is listed as a placeholder.
- **Blocks.** Neither party blocks the other (`user_blocks`, in both directions). For upvote groups this applies per voter.
- **Preferences.** The reason is not in `notification_state.disabled_reasons`.
- **Upvote groups.** At least one live qualifying upvote exists, checked with `EXISTS`, or the item's bridged upvote total is above 0 (decision 8; ignored when `TRUSTED_BRIDGE_PDS_HOSTS` is empty, never filtered by host). A group whose voters are all hidden and whose bridged total is 0 is not listed and not counted.

### Erasure

`postgresUserRepo.Delete` (`internal/db/postgres/user_repo.go:278`) runs erasure in one transaction. It hard-deletes the user's comments, votes, posts, and `users` row. It gains two steps:

1. `DELETE FROM notifications WHERE recipient_did = $1 OR actor_did = $1`, plus deletion of the user's `notification_state` row and of the `notification_public_post_withdrawals` markers for the user's posts (both kinds).
2. Upvote groups this voter contributed to are left in place. The read-time `EXISTS` rule hides any that lose their last voter and have no bridged total. A periodic sweep deletes groups with no qualifying voters and a bridged total of 0.

**Recipient race.** `notifications.recipient_did` has a foreign key to `users(did) ON DELETE CASCADE`. The fan-out insert takes a `KEY SHARE` lock on the users row, which blocks until an erasure that is deleting it commits, and then fails. Erasure therefore can't interleave with a recipient insert.

**Actor race.** Actors have no foreign key, because votes and comments arrive before their authors are indexed. Fan-out takes `pg_advisory_xact_lock_shared(hashtext('erasure:' || actor_did))` and then checks `deleted_accounts`. Erasure takes the exclusive form of the same lock before inserting the `deleted_accounts` marker.

### Data model

The migration takes the next free number at implementation time (it landed as 053), with Goose Up and Down sections.

```sql
CREATE TABLE notification_activation (
    singleton    BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    activated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO notification_activation (activated_at) VALUES (NOW());

CREATE TABLE notifications (
    id            BIGSERIAL PRIMARY KEY,
    recipient_did TEXT NOT NULL REFERENCES users(did) ON DELETE CASCADE,
    reason        TEXT NOT NULL CHECK (reason IN ('postReply','commentReply','mention','upvote')),
    record_uri    TEXT,          -- the reply or mentioning record; NULL for upvote groups
    record_cid    TEXT,
    actor_did     TEXT,          -- NULL for upvote groups
    subject_uri   TEXT,          -- recipient's own post/comment; NULL for mention
    root_post_uri TEXT NOT NULL, -- navigation target for every reason
    record_created_at TIMESTAMPTZ, -- display only ("2h ago"); NULL for upvote groups
    sort_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- index time; bumped by new upvotes
    CHECK (
        (reason = 'upvote' AND record_uri IS NULL AND record_cid IS NULL AND actor_did IS NULL
            AND subject_uri IS NOT NULL)
     OR (reason IN ('postReply','commentReply') AND record_uri IS NOT NULL AND record_cid IS NOT NULL
            AND actor_did IS NOT NULL AND subject_uri IS NOT NULL)
     OR (reason = 'mention' AND record_uri IS NOT NULL AND record_cid IS NOT NULL
            AND actor_did IS NOT NULL AND subject_uri IS NULL)
    )
);

CREATE UNIQUE INDEX uq_notifications_record
    ON notifications (recipient_did, reason, record_uri) WHERE reason <> 'upvote';
CREATE UNIQUE INDEX uq_notifications_upvote_group
    ON notifications (recipient_did, subject_uri) WHERE reason = 'upvote';
CREATE INDEX idx_notifications_recipient_sort
    ON notifications (recipient_did, sort_at DESC, id DESC);
CREATE INDEX idx_notifications_record ON notifications (record_uri) WHERE record_uri IS NOT NULL;
CREATE INDEX idx_notifications_actor ON notifications (actor_did) WHERE actor_did IS NOT NULL;

CREATE TABLE notification_state (
    did              TEXT PRIMARY KEY REFERENCES users(did) ON DELETE CASCADE,
    seen_at          TIMESTAMPTZ,              -- NULL: never marked seen
    disabled_reasons TEXT[] NOT NULL DEFAULT '{}'
);
```

**Writes.** Record-keyed rows use `INSERT ... ON CONFLICT DO NOTHING`. Upvote groups use `ON CONFLICT DO UPDATE SET sort_at = GREATEST(notifications.sort_at, EXCLUDED.sort_at)`. Duplicate delivery, rev-gated replays, direct fetch, and redrive are therefore all idempotent.

**`sort_at`** is `NOW()` at the moment the qualifying mutation is applied. Record `createdAt` never influences ordering or unread, so a future-dated record can't pin itself to the top and a late record isn't born read.

**Upvote actors and counts** are not stored. They are derived at read time from live `votes` rows that pass the read-time rules, so they can't drift from the votes table. The count adds the item's `bridged_upvote_count` (decision 8), read from the posts or comments row. The actors are native voters only. Upvote groups already store no actor (the CHECK above), so bridged-only groups need no schema change.

**Author deletion keeps rows (decision 7).** When a comment or postv2 post is deleted by its author, `deleteComment` and `tombstoneAuthorPost` keep every notification row, including on a zero-row soft delete. The read-time rules show those rows as placeholders when the post has an `authorDelete` marker, and hide them otherwise. A post soft-deleted outside the firehose by `compensateAuthorDelete` also keeps its rows, so that path needs no notification housekeeping.

- A comment re-created at the same URI (resurrection) keeps its rows. Recipients who already hold a row are not notified again. The surviving mention rows count toward the 10-mention cap.
- A resurrection under a different parent, in the same transaction:
  - deletes the record's reply row whose subject is no longer the parent;
  - deletes the new reply recipient's mention row, because the reply takes precedence;
  - moves the remaining rows to the new root when the root changed.

**Public-withdrawal markers (decision 7).** "Publicly visible at the moment it was deleted or removed" cannot be read from existing columns afterwards:
- For a hosted community, `tombstoneAuthorPost` withdraws the acceptance after the soft delete, and `ApplyAcceptanceDelete` returns the admission to `pending` and clears `accepted_cid`.
- `ApplyRemoval` clears the acceptance columns, and it inserts a `removed` row for a pre-emptive removal of a post that was never accepted.

So the transaction that withdraws a post records whether the post was publicly visible just before it, using `visiblePostsPredicate(anonymousViewerSQL)` (reused, never copied). The next migration adds:

```sql
CREATE TABLE notification_public_post_withdrawals (
    post_uri      TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('authorDelete', 'communityWithdrawal')),
    community_rev TEXT,  -- the community event's rev; NULL for authorDelete
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (post_uri, kind),
    CHECK ((kind = 'authorDelete') = (community_rev IS NULL))
);
```

Markers are written only for postv2 posts. A row means "this post was publicly visible just before this withdrawal applied". There is no foreign key, matching `notifications.root_post_uri`.

- **`authorDelete`:** written by the tombstone transaction after the soft delete and before `withdrawAcceptance`, on the applied and the zero-row branch alike, when the post passes the predicate. This writer does not check `deleted_at` (the predicate does not read it), because the post is already soft-deleted when it runs. A post soft-deleted only by `compensateAuthorDelete`, whose acceptance is withdrawn before the firehose tombstone arrives, gets no marker and stays hidden (fail closed).
- **`communityWithdrawal`:** a moderator removal is the commit `{acceptance-delete, removal-put}` at one community rev, and the two halves can be applied in either order. Here "publicly visible" means not deleted (`p.deleted_at IS NULL`) and passing the predicate. The `deleted_at` clause matters: `compensateAuthorDelete` soft-deletes a post while its admission can stay accepted until the withdrawal arrives, and a removal in that window must not record a public withdrawal. Both writers record, in the same transaction as the admission write and only when the write applied:
  - `ApplyAcceptanceDelete`: when the post was publicly visible before the write, upsert the marker with this event's rev.
  - `ApplyRemoval`, when the prior own-community status was not `removed`:
    - the post was publicly visible before the write: upsert the marker with this event's rev;
    - it did not, and the marker's rev equals this removal's rev (the acceptance-delete half of the same commit came first): keep the marker;
    - otherwise: delete the marker.
  - The same-rev rule keeps the marker even if the post was soft-deleted between the two halves.
  - `ApplyRemoval` on a post that is already `removed` leaves the marker alone.
  - Only the post's own community counts: the posts row must have `community_did` equal to the event's community (the fork case writes nothing).
  - A refused (stale) admission write changes no marker.
  - "Before the write" is the committed admission row the write replaces. Each writer locks the admission row (`FOR UPDATE`) before deciding, so a concurrent admission write on the same row is seen once it commits. A row that did not exist when locked counts as not visible, even if a concurrent insert creates it. A failed marker write rolls back the admission write.
- **Lifting a removal** (`ApplyRemovalDelete`, or a restoring `ApplyAcceptance`) needs no marker change. The read rule requires `status = 'removed'`, so a lifted removal's marker is ignored, and the next removal recomputes it. A marker left by an unpaired acceptance deletion is ignored the same way.
- Erasure deletes the user's markers (`split_part(post_uri, '/', 3) = did`).

### Unread and seen

These follow Bluesky's `app.bsky.notification` model, with the two changes in decision 6.

- **Unread:** `sort_at > seen_at`. Placeholder rows (decision 7) are visible rows and count. When `seen_at` is NULL, only the newest visible notification is unread. That matches Bluesky's first-load behaviour, so a new user isn't shown a huge count.
- **`updateSeen(seenAt)`:** the server stores `GREATEST(seen_at, LEAST(seenAt, NOW()))`. It never moves backwards and never runs ahead of server time.
- **Client watermark:** the client sends the largest `sortAt` among the notifications it displayed on page one. It never sends its own clock.
- **Accepted race:** a notification whose transaction stamped `sort_at` before the page-one query but committed after it gets a timestamp at or below the watermark. It is marked read without being shown. Consumer transactions last milliseconds, so this is documented and accepted. Bluesky has the same gap in a wider form.
- **Page state:** `listNotifications` returns the stored `seenAt`. Clients compute `isRead` for every later page against page one's `seenAt`, so the `updateSeen` issued after page one doesn't retroactively mark deeper pages read. This matches Bluesky's `feed.ts`.

### Pagination

Pagination is keyset on `(sort_at DESC, id DESC)` with an opaque cursor, base64 of the pair.

Upvote groups can move upward. A group bumped above the cursor while the user scrolls is not shown on later pages. It appears at the top on the next page-one load, when it is unread new activity anyway. Clients reconcile by reloading page one on pull-to-refresh, and whenever the unread count increases.

A malformed or undecodable cursor returns `InvalidCursor` (HTTP 400). Mobile applies its existing Discover recovery: replace page one once, then offer a manual retry.

### Query cost

- **`getUnreadCount`:** `SELECT count(*) FROM (<visible unread rows> LIMIT 101)`. The result is reported as a number up to 100, with `101` meaning "100+". It never scans past 101 visible rows.
- **Read-time rules** are `EXISTS` and anti-joins in the same statement as the page query, so a page is never short because of filtering done after the fetch.
- **Bridge-PDS eligibility** is write-time only. There are no read-time network or config lookups. Whether bridged totals count (decision 8) is fixed when the repository is built at boot, from whether `TRUSTED_BRIDGE_PDS_HOSTS` is empty. It is one boolean per deployment, not a per-host check: while the setting is non-empty, every stored total counts, including one from a bridge since removed.
- **Hydration** batches profile and record lookups per page (`GetByDIDs`, `GetViewsByURIs`). It never queries per row.
- **Verification:** the implementation includes an `EXPLAIN` check on a T1 fixture with a large, mostly filtered history (for example 10k rows with 90% blocked), confirming the recipient/sort index is used.

### Retention

This adopts the policy Bluesky's AppView ran while it was Postgres-backed. That was PR bluesky-social/atproto#1893 (December 2023), removed in the February 2024 "AppView v2" rewrite. Bluesky's current public code has no retention at all, and production behaviour lives in a private dataplane.

- **Read rows** are deleted once `sort_at < seen_at - 30 days`. A user who has never marked anything seen uses their newest `sort_at` as the reference instead.
- **Unread rows** are deleted only when a user has more than 500 unread, and only those more than 180 days older than the user's newest notification. Someone who stops opening the app keeps a bounded backlog.
- **Empty upvote groups**, meaning groups with no live qualifying voter left and a bridged upvote total of 0, are deleted by the same job.
- **Event-driven deletes** (erasure) happen at write time as described above, like Bluesky's `deleteActor`. Retention does not replace them. Author deletes keep their rows as placeholders (decision 7), and retention removes those rows like any other.

**Scheduling.** A ticker goroutine started in `cmd/server`, alongside the redriver's `PruneDeadLetters` loop, runs every hour. Each statement deletes at most 10,000 rows, keyed by `id`, and the job loops until a batch comes back short, so it never holds a long lock. The thresholds are constants, not config. They can become config later if production data calls for it.

**Volume.** Upvote groups collapse the largest notification category, which is one row per like in Bluesky, into one row per item. Reply volume per user should be comparable or higher in forum threads. The 30-day read window keeps the table at roughly a month of activity per user either way.

### Package layout

- `internal/core/notifications/`:
  - `notification.go`: types, reasons, error sentinels.
  - `interfaces.go`: `Repository` (with `ApplyTx(ctx, *sql.Tx, []Intent)`) and `Service`.
  - `fanout.go`: pure intent computation and write-time rule evaluation, given injected lookups.
  - `service.go`: list, count, seen, preferences, hydration.
- `internal/db/postgres/notification_repo.go`.

## Lexicons

New namespace: `social.coves.notification.*`. These follow the Lexicon Style Guide (camelCase `knownValues`) and are written so another forum AppView could implement them.

| NSID | Type | Purpose |
|---|---|---|
| `listNotifications` | query | Params `limit` (1–100, default 50) and `cursor`. Returns `notifications[]`, `cursor`, `seenAt`. Errors: `InvalidCursor`. |
| `getUnreadCount` | query | Returns `count` (0–101; 101 means 100+). |
| `updateSeen` | procedure | Input `seenAt`. Stored as described under Unread and seen. |
| `getPreferences` | query | Returns `{ postReply, commentReply, mention, upvote }` booleans. |
| `putPreferences` | procedure | Partial update of the same four booleans. |
| `defs` | defs | `notificationView`, the `reason` knownValues, `preferences`. |

All of these require auth, and the recipient is always the caller.

**`notificationView`** fields:

- `reason`: `postReply`, `commentReply`, `mention`, or `upvote`. The set is open.
- `sortAt`: the index time; this drives ordering and unread.
- `isRead`
- `rootPost`: strong ref, title, and community. Every reason carries this, so every row can be opened on its post.
- `subject` (`postReply`, `commentReply`, `upvote`): strong ref to the recipient's own post or comment, plus a preview.
- `record` (`postReply`, `commentReply`, `mention`): strong ref, text excerpt, and `createdAt` for display.
- `status` on `rootPost`, `subject` and `record` (decision 7): open knownValues `deleted` and `removedByModerator`, absent when the reference is live. When it is present, the view omits the title, community, preview or excerpt. A strong ref's CID is the referenced record's current CID.
- `labels` on `rootPost`, `subject` and `record` (decision 9): `com.atproto.label.defs#selfLabels`, the referenced record's author self-labels. Absent when there are none or when the reference has a `status`.
- `thumbnail` on `rootPost`, `subject` and `record` (decision 9): a post's link-card thumbnail or first image, as an absolute image-proxy URL. Never set for comments, for references with a `status`, or when the deployment runs without the image proxy.
- `thumbnailAlt` on `rootPost`, `subject` and `record` (decision 9): the alt text of an image post's first image, bounded like the images embed's `alt` (1,000 grapheme clusters, 10,000 bytes). Set only alongside an image post's `thumbnail`; absent for link-card thumbnails and when the alt is blank.
- `author` (`postReply`, `commentReply`, `mention`): `social.coves.actor.defs#profileView`.
- `upvoteCount` and `recentUpvoters` (`upvote` only):
  - `upvoteCount` is the qualifying upvotes from accounts on this network plus the item's bridged upvote total from federated platforms (decision 8).
  - `recentUpvoters` is up to 3 `#profileView` entries for the most recent qualifying voters on this network, ordered by the existing AppView indexing timestamp, descending; ties by higher vote ID. It may be empty when every counted upvote is bridged, so clients render "N people upvoted" without names.

**Previews.** A post preview is its title. When the title is absent or whitespace-only, it falls back to a text excerpt, except for an image post, which has no text preview and shows its `thumbnail` instead. A comment preview is an excerpt truncated to 140 grapheme clusters. A deleted or removed reference has no preview; it carries `status` instead, and clients render "[Deleted Comment]", "[Deleted Post]" or "[Removed by moderator]". If a referenced record is not yet indexed, or a reference the list query saw as live is deleted before hydration, the row is omitted from that response. It is not returned half-hydrated.

**Preferences** are AppView-local, not a PDS record, as with Bluesky's preferences. A user who switches AppViews loses them. That matches the notifications themselves.

**Routing.** `/xrpc/*` is already covered by the Caddy `@appview` matcher and by the frontend proxy's path validation. The implementation confirms this with `caddy_allowlist_test.go` and the proxy tests.

## Frontend (`coves-frontend`)

- **Route `/notifications`** (auth required), with infinite scroll.
  - Rows render per reason, with an unread highlight.
  - A reference with `status` renders as "[Deleted Comment]", "[Deleted Post]" or "[Removed by moderator]" in place of its title or excerpt (decision 7).
  - Replies and comment mentions open `rootPost` with the comment focused. Post mentions and upvote groups open the subject or `rootPost`. Placeholder rows navigate the same way, and the thread shows its existing deleted or removed state.
  - A target that no longer exists shows the existing not-found state.
- **Nav bell with a badge.**
  - Polls `getUnreadCount` every 30 seconds while `document.visibilityState` is visible, and once on focus. Stops when logged out.
  - Shows "99+" when the count is above 99.
  - When the count rises while `/notifications` is open, page one reloads.
- **Mark seen.** After page one renders, call `updateSeen` with the largest displayed `sortAt`. Compute `isRead` for later pages against page one's `seenAt`. Set the badge from the next `getUnreadCount` response, not by optimistically zeroing it.
- **Settings.** A "Notifications" section in `/settings`, using `ToggleSetting.svelte`, with four toggles backed by `get/putPreferences`.
- **API.** NSIDs go in `src/lib/api/coves/client.ts`, types in `types.ts`.
- **i18n.** Replace the Photon `inbox` keys in `en.json`. Other locales fall back to English.
- **States.** An empty state, a retryable error, and 401 handling through the existing session-expiration flow.

## Mobile (`coves-mobile`)

- **Notifications screen.** Replace the placeholder `NotificationsScreen` with a paginated list backed by `NotificationsProvider` (`lib/providers/`) and `NotificationService` (`lib/services/`, using `coves_http.dart` and `auth_interceptor.dart`).
- **Badge.** On the bell nav item in `main_shell_screen.dart` (`_buildNavItem(3, 'bell', …)`). It refreshes on `AppLifecycleState.resumed`, on tab switch, and every 30 seconds while foregrounded.
- **Mark seen.** Same rule as web.
- **Placeholders.** A reference with `status` renders as "[Deleted Comment]", "[Deleted Post]" or "[Removed by moderator]" in place of its title or excerpt (decision 7). Placeholder rows navigate like any other row.
- **Navigation.** Every row opens `PostDetailScreen` for `rootPost.uri`, passing `focusCommentUri` for replies, comment mentions, and comment upvotes. `FocusedThreadScreen` is not used directly, because it needs a hydrated thread and provider. `PostDetailScreen` already fetches and focuses a comment outside the loaded tree. A missing target shows the existing not-found state.
- **Settings.** Four switches on the existing settings or profile screen.
- **Refresh.** Pull-to-refresh replaces page one. `InvalidCursor` uses the Discover recovery rules.

## Testing

Tiers follow `docs/TEST_ARCHITECTURE.md`. Breadth goes at T0 and T1.

**T0** (`internal/core/notifications`, fan-out with fake lookups):
- Every reason.
- Self-suppression, including the automatic author upvote.
- Mention deduplication against replies, and `commentReply` precedence.
- Community-DID mentions ignored.
- Edit fan-out: only newly added DIDs notify, and unchanged facets do not.
- The activation and 7-day gates at their boundaries, including old record with a new mention, and future-dated `createdAt`.
- The deleted/removed gate on every fan-out: no intents, and no upvote bump, when a referenced post is deleted or removed or a referenced comment is deleted.
- Cursor encode/decode, and tampered-cursor rejection.

**T1** (Postgres):
- **Idempotency.** The same comment, post, direct-fetch post, or vote applied twice yields one row. A rev-gated stale replay yields no change.
- **Upvote lifecycle through the real consumer paths:**
  - create;
  - a second voter bumps the group;
  - a stale-vote replacement by a downvote removes the last voter's group;
  - delete-then-create flip;
  - a blocked voter's upvote does not bump;
  - the group is hidden when all voters are blocked.
- **Bridged totals (decision 8):**
  - a poll increase above the item's high-water mark creates or bumps the group; a rise back up to a previous high after a decrease does not;
  - an increase on an item older than 7 days bumps, and so does an increase whose bridge `asOf` predates activation and is more than 7 days old but newer than the stored `asOf` (no activation or freshness gate);
  - two concurrent applications of the same total bump once (the previous total is read under the item's row lock);
  - an unchanged total, a stale `asOf` and a decrease do not bump;
  - a drop to 0 with no qualifying native upvote deletes the group;
  - totals stored before launch notify no one;
  - a native vote removal keeps a group that has a bridged total;
  - `upvoteCount` adds the total, and `recentUpvoters` stays native-only;
  - the recipient gates and the deleted/removed gate apply;
  - with `TRUSTED_BRIDGE_PDS_HOSTS` empty, bridged totals are ignored;
  - after a restart that removes one of two trusted hosts, the removed host's stored totals still count and never bump again;
  - erasure racing the poller deadlocks nowhere.
- **Transactionality.** A failing notification write rolls back the comment, post, or vote index on each commit branch, including comment resurrection and the vote consumer's early-commit branches.
- **Visibility:**
  - pending, rejected, awaiting-reacceptance, missing-admission and CID-mismatched postv2 posts as source, subject, and root are hidden;
  - author-deleted and moderator-removed postv2 posts that were public at that moment, and deleted comments, are listed with `status` and no excerpt, and count as unread;
  - a post deleted or removed while pending stays hidden, and so does a deleted or removed legacy post;
  - list and count agree in every case.
- **Author deletion keeps rows.** `deleteComment` and the post tombstone, including a zero-row soft delete, leave every row. The tombstone records an `authorDelete` marker only for a post that was publicly visible. `ApplyRemoval` and `ApplyAcceptanceDelete` maintain the `communityWithdrawal` marker in both delivery orders of a removal commit, write none for a soft-deleted post that is still accepted, follow the committed state under a concurrent admission write, and roll back with a failed marker write. Erasure deletes both kinds. A resurrection keeps the rows, its mention cap carries over, and a different-parent resurrection repairs the old reply row and the root.
- **Write-time gate.** On each consumer path, a comment, edit, post or upvote whose referenced post is deleted or removed (including a comment's parent post that is not its root), or whose parent or subject comment is deleted, writes no row and bumps no group. A pending root still gets its row. Restoring a removed post does not backfill.
- **Blocks in both directions**, on list and count.
- **Preferences**, on list and count.
- **Unread:**
  - NULL `seen_at` (only the newest row is unread);
  - backwards `updateSeen` is ignored;
  - a future `seenAt` is clamped;
  - a delayed reply indexed after `seen_at` is unread.
- **Erasure:**
  - recipient and actor rows are deleted;
  - `notification_state` is deleted;
  - a redrive of an erased actor's comment creates nothing;
  - concurrent erasure and fan-out, for both the recipient and actor paths (lock-bite tests per `feedback_concurrency_lock_bite`).
- **Eligibility.** Aggregator, bridge-PDS, unknown, and erased recipients get no rows.
- **Retention:**
  - read rows are kept up to exactly 30 days before `seen_at` and deleted after;
  - a user who has never marked anything seen uses their newest row as the reference;
  - the unread cap applies only above 500 unread;
  - empty upvote groups are swept;
  - deletion runs in 10,000-row batches and continues until a batch comes back short.
- **Count bound.** A count over 101 visible rows returns 101, and the `EXPLAIN` check passes.
- **Handlers.** Every endpoint is tested at T1 with a sealed session minted in-process (`store.SaveSession` + `client.SealSession`), the same pattern existing viewer-scoped handlers use.

**T2** (`tests/e2e`, hermetic stack):
- **Available now:** every new NSID returns 401 without a session.
- **Blocked on the auth prerequisite:** the pipeline flow for each reason, where user A replies, mentions, or upvotes through a real PDS write, then user B lists notifications, marks them seen, and the count clears. T2 observes only through serving endpoints and the consumer-health endpoint (§3.4 rule 1), and every notification endpoint is viewer-scoped. Until the session unlock, this is the same gap as block enforcement: the consumer wiring is proven at T1 against real Postgres, and T2 proves only the auth boundary.

**Frontend:** client and polling-store unit tests (visibility pause, logout stop, count-rise reload), plus browser tests for the route, badge, mark-seen watermark, page-one `seenAt` read state, and settings.

**Mobile:** provider and service unit tests, widget tests for the list, badge, empty and error states, and navigation. One integration test runs against the local dev stack, using local PLC and PDS and real accounts.

## Rollout

1. The backend ships first. The endpoints are additive, and `activated_at` is set by the migration.
2. The frontend and mobile ship independently after that. Older mobile builds keep the placeholder tab.
3. There is no feature flag. Notifications accrue from activation onward.

## Chunks for `/prd-loop` (one loop per repo)

**`coves`**: done, built in 28 chunks; see `NOTIFICATIONS_IMPLEMENTATION.md`.

**`coves-frontend`**
1. API client and types, the polling store, and the nav badge.
2. `/notifications` route with the mark-seen watermark and page-one read state. **Manual, outside `/prd-loop`:** the visual design is decided while building this chunk.
3. Settings toggles.

**`coves-mobile`**
1. Service and provider, plus the nav badge.
2. Notifications screen, `PostDetailScreen` navigation, and mark-seen. **Manual, outside `/prd-loop`:** the visual design is decided while building this chunk.
3. Settings toggles.

## Resolved questions

1. **Community bans do not suppress notifications.** A ban currently gates only post admission (`internal/core/posts/admit.go`). There is no ban check on comments, votes, or reads (`comments.ErrBanned` and `votes.ErrBanned` are defined but unused). A banned user's comments stay visible in threads, so notifying about them is consistent. If bans are later extended to comments, notifications follow automatically through the read-time visibility rules.
2. **Retention:** Bluesky's former Postgres-era policy (30 days after read, a 500-unread cap with 180-day cutoff). See Retention.
3. **Removed posts (revised 2026-10-01, decision 7):** existing notifications stay, listed as "[Removed by moderator]" placeholders when the postv2 post was publicly visible at the moment of removal; otherwise they stay hidden. Removal stops new ones at write time. The 2026-09-28 resolution hid them through the anonymous-viewer rule.
4. **`compensateAuthorDelete` housekeeping (moot, 2026-10-01).** A backlog concern noted that `compensateAuthorDelete` (`internal/core/posts/service.go`) soft-deletes a postv2 without notification housekeeping. Author deletes now keep rows by design (decision 7), so no path needs that housekeeping.
