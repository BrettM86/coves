# Moderation

Formerly `docs/PRD_ADMIN_MODERATION.md`. Code comments cite this file by section heading and by decision ID (Q-I6, Q-ORACLE, D-QUOTES), so keep both stable.

Server admins remove and restore posts and comments, and apply or retract an NSFW label on posts. The service is `internal/core/moderation`, the state is in migrations 050 to 052, and the served endpoints are `social.coves.moderation.removeContent`, `restoreContent`, `labelContent`, `retractContentLabel`, `getSubjectState`, `listActions` and `listAdminActions`. The other schemas in that lexicon directory (the `ban` family, `ruleProposal`, `tribunalVote`, `vote`) have no routes.

## Scope

A removal redacts content from this instance's served views and nothing else. It never writes to the author's PDS, never writes or deletes a community's acceptance or removal records, and never rewrites author self-labels. That holds for communities hosted on this instance too: community admission stays a separate constraint on visibility.

Instance removals are not sent over ActivityPub. Tidepool consumes repository records, not labels, and turning an instance decision into an ActivityPub `Delete` would act as the author or claim community authority the instance does not have. Copies already delivered to Lemmy stay there.

Content state is several independent facts, never one `deleted` flag: the author's record lifecycle, community admission, active moderation decisions (each with its own authority and scope), and active classifications such as NSFW. An author edit, a firehose replay, a delete and recreate at the same URI, or community re-acceptance does not clear a standing removal. Decisions are keyed by subject URI; a different URI is a different subject.

## Authority

Admins are the DIDs listed in `MODERATION_ADMINS`. An empty list grants nobody, and config validation rejects an entry that is not a DID. `RequireInstanceAdmin` in `internal/api/middleware/admin_auth.go` checks the caller's DID (OAuth session or PDS service JWT) against the list before any handler or idempotency replay runs. Never authorize by handle, community creatorship, PDS ownership or a caller-supplied actor field. The authority DID on every action is `INSTANCE_DID`.

Any admin may reverse another admin's decision within the same authority and scope, and history keeps both actors. Removing a DID from the list does not erase or retract its past actions.

The list is read at boot. To change it in production, edit `MODERATION_ADMINS` in `/opt/coves/.env` and redeploy the AppView (`scripts/deploy.sh appview`). `deploy.sh` exports `.env.prod` over `.env`, so `.env.prod` must not also set it.

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

Reasons are the tokens in `moderation.defs`: spam, harassment, doxing, illegal-content, rule-violation and moderator-discretion. There is no public free text; private notes are admin-only. A `csam` report reason exists only in `adminreports`; an admin picks illegal-content when acting on one.

A removal's reason cannot be corrected out of the public log. Hiding is decided per entry, from its own reason and its reversed action's reason, and there is no amend API, so a restore and re-removal leaves the first entries public. Choose doxing or illegal-content on the first removal.

Doxing and illegal-content are remove-only. Q-RESTORE-REASON (2026-09-29): `restoreContent` rejects them with `UnsupportedReason`. Q-LABEL-REASON (2026-09-30): `labelContent` and `retractContentLabel` reject them the same way.

Public modlog entries whose reason, or whose reversed action's reason, is doxing or illegal-content omit the subject URI, CID and community, and are left out of any query filtered by subject, collection or community. The record is still fetchable from its author's PDS, and the log must not link to it. Q-ORACLE, accepted 2026-09-29: someone who already holds a URI can see that it is removed, find no public entry naming it, and infer the reason was doxing or illegal content.

## Errors

The vocabulary is the sentinel list in `internal/core/moderation/errors.go`: AuthRequired, Forbidden, InvalidRequest, InvalidSubject, SubjectNotFound, DecisionNotFound, InvalidDecision, ContentChanged, StateConflict, IdempotencyConflict, UnsupportedReason, UnsupportedLabel, InvalidCursor and ModerationUnavailable. `tests/lexicon_moderation_test.go` pins each endpoint to the subset it can return. `ErrSubjectNotIndexed` and `ErrResolverUnavailable` never reach the wire alone.

Authorization runs before any subject or decision lookup. InvalidDecision (a reversal aimed at the wrong action kind, at another authority or scope, or at a decision no longer active) mutates nothing. A stale `expectedVersion` is StateConflict; a reviewed CID that no longer matches the indexed record is ContentChanged. An unindexed but well-formed URI has a state from `getSubjectState` (version `v0`, record unavailable), while `removeContent` and `labelContent` return SubjectNotFound for it.

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

There is no write-path refusal (decided 2026-09-20). `feed.vote.create` and `comment.create` still forward to the author's PDS without a moderation lookup, and the consumer indexes votes and comments that target removed subjects. They stay unserved through the parent's state and appear on restore. Clients hide vote and reply controls on a removed-comment placeholder as they do on an author-deleted one.

## Idempotency and versions

Every mutation carries an idempotency key and the `expectedVersion` from `getSubjectState`. Keys are scoped to actor DID plus authority DID and bound to a fingerprint of the whole request; reuse with a different body is IdempotencyConflict, and a replay returns the original result, not current state. `MODERATION_IDEMPOTENCY_RETENTION` (default 24h) sets how long keys live, and `MODERATION_IDEMPOTENCY_MAX_LIVE_KEYS` (default 1000) caps live keys per actor; a request over the cap is InvalidRequest. After a key expires, the monotonic version still stops an old retry from reversing newer state.

## Durability

`moderation_*` tables hold decisions, not index state. They have no foreign key to posts or comments, so decisions survive deletes, recreates at the same URI, and reindexing. Backups and any truncate or reindex procedure must keep them.

## Not built

- Federated moderation through signed labels: see [plans/MODERATION_LABELS.md](plans/MODERATION_LABELS.md). The schema already accepts `origin = inherited` and the `apply-removal` and `retract-removal` actions.
- Community-admin moderation. `scope_kind = community` exists in the tables, but nothing writes it.
- Evidence retention for illegal-content removals, which keep no snapshot, and modlog editing.
- Comment removal across the Lemmy bridge. `community.removal` is post-scoped, so a Lemmy moderator removing a native Coves comment is recorded only in Tidepool and the comment stays visible here, and a moderator-removed Lemmy comment arrives as a repo delete and shows as deleted by its author.
