# PRD: Admin Moderation, Public Modlog, and Federated Decisions

**Status:** Local milestone (phases 1a and 1: authority, remove/restore, NSFW apply/retract, read-path enforcement, media blocks, public/admin modlog, optional CDN purge) implemented on the backend. The federated milestone (§7 labeler, label publication/ingestion, §10 items 10–16) is not implemented.

**Date:** 2026-09-17

**Scope:** Server-admin moderation of posts and comments, designed for later community-admin moderation

**Related:** [Author-owned posts](PRD_AUTHOR_OWNED_POSTS.md), [Governance](PRD_GOVERNANCE.md), [Test architecture](TEST_ARCHITECTURE.md)

## 1. Summary and recommendation

Give each Coves instance multiple DID-authenticated administrators who can remove and restore posts and comments from that instance's served views, and apply or retract NSFW labels on posts. NSFW uses the existing client blur/reveal treatment without excluding the post from feeds; full AppView removal is a separate action. Removed comments retain the machine-readable `moderator-removed` state, with authority scope selecting **[Removed by server admin]** or, later, **[Removed by community moderator]**. Record each removal and restoration in a public, paginated modlog, and every action, NSFW included, in an admin-only log with private notes.

**Use atProto labels to distribute moderation decisions, with a separate action history and local enforcement policy.** Labels are the right federation primitive for “instance Y chooses to follow Coves.social's removals.” They do not themselves delete content, grant moderation permissions, provide a complete audit log, or force another instance to comply.

The three responsibilities are:

1. **Decision and history:** who acted, under whose authority, on which content, why, and which earlier action was reversed.
2. **Signed signal:** an atProto label identifying the issuing authority, subject, and removal or NSFW classification, with independent retractions.
3. **Enforcement:** each AppView decides which authorities it trusts and redacts content accordingly.

This is label-integrated moderation, rather than a local deletion feature with federation added later. Community moderation can reuse the action model, modlog, label ingestion, and view redaction while retaining a separate community authority and scope.

### Confirmed product decisions

- Start with server-level admins; community-admin permissions and tools come later.
- Support multiple individual admins and an action-by-action site-wide modlog.
- **Initial actions: remove/restore posts and comments, and apply/retract NSFW labels on posts.** The NSFW addition supersedes the earlier remove/restore-only scope.
- **NSFW means the existing blur/reveal treatment only.** It does not delist the post, exclude it from default feeds/search, or impose a new ranking penalty. A separate removal can suppress the post throughout the instance's AppView when needed; revealing NSFW content cannot bypass that removal.
- **Removal state and authority are separate.** Keep `moderator-removed` rather than adding an `admin-removed` state; distinguish server admins and community moderators using scope/source metadata and display copy.
- Removing a quoted post redacts that quote's preview; it does not independently remove the containing post or introduce an embed-only removal action.
- **The modlog is public, with private notes.** Public disclosure of individual moderator identities remains a separate decision.
- Another instance must be able to opt into Coves.social's moderation decisions.
- **A removed post's thread returns `NotFound`** (2026-09-20). Its comments stay in their authors' repositories but are not reachable through the thread endpoint or profile activity. Preserved discussion behind a redacted header is later scope and would need a versioned thread endpoint.
- **Removed comments reuse the existing deleted-comment placeholder** (2026-09-20): `record` absent, `isDeleted`, `deletionReason: moderator`, plus optional moderation attribution. No versioned comment endpoint. The published `commentView` is corrected in place under an approved evolution-rule exception (section 14.4).
- **Removal covers Coves-served media bytes** (2026-09-20), on cache hits as well as misses, with the cached bytes purged. A removal blocks the record's (owner DID, blob CID) pair; a removal with reason `illegal-content` also blocks that CID for every owner, so a re-upload from another account is not served. Decided 2026-09-28 (Q-I6): an image the author adds to the subject after an `illegal-content` removal (by an edit, a recreate, or a create the consumer does not index) gets an owner-scoped block only; every-owner blocks cover only the CIDs indexed when the admin acted. Restoring the decision lifts both.
- **Simultaneous author deletion and removal shows the author-deleted placeholder** (2026-09-20); moderation history is retained.
- **The public modlog names the acting admin** (2026-09-20). The API carries the actor DID (`#actorRef`); clients resolve and display the handle.
- **No admin inspection or retained-evidence storage in this release** (2026-09-20). No `getSubject` endpoint; an admin reviewing a restore reads the current record from its PDS.
- **Labels for public subjects are published by default** (2026-09-20); restricted subjects are excluded until sharing rules exist.
- **The labeling authority is the instance DID** (2026-09-20, `INSTANCE_DID`, a `did:web`), with a separate `#atproto_label` key and an `#atproto_labeler` service added to its DID document. No dedicated labeler account. Whether to reuse an existing label publisher remains open.
- **Trust configuration is operator-managed** (2026-09-20), with attributed changes and no per-user opt-out of mandatory removals.
- **Removed posts use `#moderatedPost` wherever the schema allows it** (2026-09-20). The `#notFoundPost` fallback this decision allowed was not built: if a supported older decoder fails on the new variant, admin actions stay disabled (`MODERATION_ADMINS` empty) until supported builds pass, as in `docs/LEXICON_PUBLISHING.md` release gate 5.
- **Public reason codes** (2026-09-20): `spam`, `harassment`, `doxing`, `illegal-content`, `rule-violation`, `moderator-discretion`. A private `csam` classification is shown publicly as `illegal-content`. Public modlog entries with reason `illegal-content` or `doxing` omit the subject URI, because the record is still fetchable from its author's PDS and the log must not work as a link to it. Decided 2026-09-29 (Q-ORACLE): omitting the subject keeps doxing and illegal-content subjects out of the browsable and filterable log, but an observer who already holds a URI can see that it is removed and, finding no public entry for it, infer that the reason is doxing or illegal content; the user accepted this on 2026-09-29.
- **No public free-text explanation in this release** (2026-09-20). The public log carries the reason code only; private notes stay admin-only. With no public text there is no correction procedure to define.
- **No write-path refusal for removed subjects** (2026-09-20). Clients hide controls on placeholders; the consumer indexes normally (section 5.2).

Everything below is the recommended contract unless marked confirmed above. Section 12 lists decisions to approve before implementation; unresolved choices must not silently become requirements.

## 2. Goals, boundaries, and release scope

### Goals

- Let trusted admins act on any indexed post or comment served by their instance, including content authored or hosted elsewhere.
- Make removals effective across APIs and clients, rather than merely hiding text with client-side styling.
- Distinguish moderator removal from author deletion and preserve useful thread navigation.
- Let admins mark posts NSFW using the same blur/reveal presentation as author-applied NSFW, without rewriting the author's record or changing feed eligibility.
- Make decisions attributable, reversible, and auditable without publishing sensitive reports or removed content.
- Publish and consume signed removal signals with explicit trust, source attribution, and reliable retraction.
- Avoid redesigning the core when community-admin moderation is added.

### Proposed scope

| Capability | Initial local milestone | Federated milestone | Later |
|---|---|---|---|
| Multiple instance admins, remove/restore posts and comments | Required | Reused | Granular roles and in-app admin management |
| Public site-wide modlog; admin-only notes | Required | Source attribution for inherited actions | Community-specific moderation tools |
| Shared removal/tombstone API contract | Required | Inherited removals use the same contract | Separate delisting or additional warning behaviors |
| Apply/retract post NSFW labels | Required; existing blur/reveal treatment, no feed exclusion | Signed NSFW labels and independent retractions from trusted sources | Other classification values and comment labeling |
| Signed labels, queries, stream, negation | Durable action history and current decisions; no publisher, outbox, or pending-delivery UI yet | Required, including durable publication/retry and initial state export | Additional classifications |
| Operator-configured trusted labelers | — | Required | User-selected and community-selected labelers |
| Existing user reports linked to decisions | Optional link; do not require a report to act | Reused | Full triage queue and appeals workflow |

The local milestone is useful on its own, but **the federation requirement is complete only after the federated milestone demonstrates two independent AppViews**. “Federation-ready schema” alone does not satisfy it.

### Non-goals

- Deleting another person's record from their PDS, removing content from every server, or performing PDS/relay takedowns.
- Account bans, community bans/quarantine, reply locks, automated classification, bulk moderation, or retroactive account-wide removals.
- Community moderator appointment, governance, ownership transfer, or permission tiers.
- A full Ozone replacement, general activity logging of every admin interaction, or migrating every existing admin-like endpoint into this release.
- Guaranteed removal of copies already downloaded or independently cached by third parties.

The admin log covers every successful remove, restore, NSFW application, and NSFW retraction; the public log covers removals and restorations only (Q-LABEL-LOG). In-app editing, amendment, and redaction of modlog entries are deferred. The public log has no free-text field in this release, so no disclosure-correction procedure is needed yet; define one before public explanations are ever added. Failed authorization and failed requests belong in private operational/security audit records, not the public modlog.

## 3. Existing foundations and gaps

Verified against the backend source on 2026-09-17. Historical PRDs describe both replaced architecture and unimplemented plans; they are not proof of shipped behavior.

| Existing foundation | What it provides | Remaining work |
|---|---|---|
| `internal/core/comments/comment.go`, `view_models.go`, `comment_service.go` | Author/moderator deletion reasons and content-free comment placeholders preserving replies | Source-aware, reversible moderation state and a shared rendering contract |
| `internal/db/postgres/comment_repo.go` | Soft deletion with actor/reason; deletion can blank content | Do not implement reversible moderation by treating author deletion as the same state |
| `social.coves.community.removal` and the author-post admission consumer | Community-owned, URI-scoped post removal; atomic acceptance/removal transitions | Instance policy is a separate layer, not a write to a remote community's repository |
| `internal/core/adminreports/`, `internal/api/routes/adminreport.go` | Authenticated user report submission and report persistence | Admin action workflow and modlog; submitting a report is not an admin capability |
| `internal/db/postgres/community_repo_memberships.go` | A community-level moderation table/repository with instance and broadcast fields | No complete per-content, per-human-actor action ledger or signed label distribution |
| Community suggestion status authorization | DID allowlist; route documentation says it reuses `COMMUNITY_CREATORS` | Explicit server-moderation authority; creator privileges must not automatically confer it |
| Post/comment self-labels | Author-supplied content warnings | Independent signed labels, trust policy, ingestion, and enforcement |
| Web `src/lib/feature/post/Post.svelte`; mobile `lib/models/post.dart` and `lib/widgets/sensitive_content.dart` in the sibling client repos | Existing NSFW presentation reads author self-labels; blur/concealment and Show/Hide controls already exist | Read effective moderator labels as well, preserve source identity, and reuse presentation rather than invent a separate warning UI |
| `internal/core/imageproxy/` | Cached media addressed by owner DID/blob CID | Content-removal-aware serving and shared-blob policy need explicit design |

The `social.coves.moderation.*` schemas contain earlier ban/governance designs. Their existence does not establish implemented moderation endpoints. [Lexicon publishing](LEXICON_PUBLISHING.md) documents moderation schemas held back from publication; review and publish only schemas needed for this work.

## 4. Actors, authority, and permissions

### Identities must remain distinct

- **Actor DID:** the authenticated human administrator performing the action.
- **Authority DID:** the instance moderation service responsible for the decision and signing its labels.
- **Scope:** initially the instance; later a specific community DID.
- **Subject:** the canonical post/comment AT URI. Record the observed CID separately for audit and stale-content checks.
- **Applying instance:** the AppView enforcing its own or a trusted authority's decision.

An admin need not author the content, own its PDS, or own its community. Their authority is to control what their instance serves. A trusted remote labeler can supply decisions; it does not acquire credentials or permission to call local admin APIs.

Server-admin remove/restore and NSFW apply/retract always change an **instance decision only**, even for communities hosted on that same instance. They never write or delete community acceptance/removal records or rewrite author self-labels. Community membership remains an independent constraint when effective visibility is evaluated.

### Initial authorization recommendation

- Operator-managed, explicit allowlist of admin DIDs; an empty allowlist grants nobody access. No self-service grant endpoint initially.
- Authenticate using existing OAuth/DID verification and configured identity infrastructure. Never authorize by handle, caller-supplied actor fields, PDS ownership, or community-creation privileges.
- Recheck authority for every mutation and private read. All configured admins may remove content, apply NSFW, and reverse another local admin's decision within the same authority/scope; retain both actors in history. Reversal never clears another authority's decision or an author's self-label.
- Record the authority and scope at action time. Later role revocation does not erase history or retract decisions automatically.
- Use stable action identifiers, idempotency keys, and a subject-state version to prevent retries and concurrent admins from producing accidental reversals.

## 5. Removal and restoration behavior

### 5.1 Meaning of remove

“Remove” means **redact from this instance's ordinary content responses**, not delete the author's record. An instance removal is not federated over ActivityPub: it reaches only label consumers that explicitly trust this instance, does not withdraw copies already delivered to Lemmy, and does not stop Tidepool delivery. Tidepool consumes repository records, not labels, and translating an instance decision into an ActivityPub `Delete` would falsely act as the author or claim community authority this PRD does not grant. Users cannot reveal the content through a “show anyway” button or by omitting a labeler preference header.

Recommended content states are independent:

- Author record lifecycle: present/deleted/unavailable.
- Community admission: accepted/pending/rejected/removed, where applicable.
- Active moderation decisions: zero or more, each with its own source and scope.
- Active content classifications: NSFW can coexist with a removal and is not itself a removal state.

Do not collapse these into a single mutable `deleted` flag. An author edit, ingestion replay, record recreation at the same URI, or community re-acceptance must not clear a standing instance removal.

### 5.2 Presentation and affected surfaces

| Surface | Removed comment | Removed post |
|---|---|---|
| Thread/direct link | Scope-aware **[Removed by server admin]** / **[Removed by community moderator]** placeholder; preserve parent/root links and independently visible replies | Thread endpoint returns `NotFound`; single-post reads return a content-free removed-post view, never the original title/body/embed |
| Feeds, search, profile activity, recommendations | Exclude the content item | Exclude the content item |
| Quotes, embeds, previews, notification hydration | Unavailable/removed subject representation, with no copied body or media | Same |
| Public modlog | Safe action metadata; link to the tombstone where access permits | Same |
| Ordinary author view | Removal notice and safe reason; no bypass just because the caller authored it | Same; instance removal overrides existing author-only admission carve-outs |
| Explicit admin inspection | Separately authorized inspection if retained content is available; do not silently reveal it in normal responses | Same |

- Preserve the existing author-deletion presentation, such as **[Deleted by user]**. If both deletion and moderation apply, show the author-deleted placeholder while retaining the moderation history (decided 2026-09-20).
- A removed comment does not automatically remove its descendants. Keep structural reply counts and pagination coherent; do not return removed text, facets, embed URLs, or quoted snapshots through alternate view fields.
- For otherwise publicly accessible subjects, recommend retaining the subject URI, necessary thread references, and minimal author DID on the tombstone, without author handle/avatar enrichment or removed content. A record URI already exposes its repository owner's DID; this is not an anonymity guarantee. For access-restricted subjects, omit identifying references under the existing access policy. Pin the same allowlist in web/mobile view contracts and tests.
- Decided 2026-09-20: a removed post's thread returns the existing `NotFound` error. Comments under it are not deleted or individually actioned, but are filtered from profile activity so they do not link to a dead thread. Restoring the post restores the thread.
- Do not expose a formerly private/inaccessible community or its thread merely because a public tombstone exists. Existing access checks still apply.
- Decided 2026-09-20: no write-path refusal in this release. `feed.vote.create` and `comment.create` keep forwarding to the author's PDS without a moderation lookup, and the firehose consumer keeps indexing votes and comments that target removed subjects so a restore brings them back; they stay unserved through the parent's state. Clients hide vote and reply controls on a removed-comment placeholder exactly as on an author-deleted one. Interactions made while a subject was removed become visible on restore.

### 5.3 Remove flow

1. Admin opens the content menu and selects **Remove**.
2. The interface identifies the instance-wide scope and requests a structured reason code; a private note is optional.
3. The server validates the authenticated admin, canonical subject, actual content type/community relationship, and expected content/state version. If the content changed since inspection, return a conflict requiring a fresh read.
4. Persist the decision, action history, and local enforcement projection as one recoverable operation. Once federation is enabled, include durable publication intent for publishable decisions in that operation and distinguish local success from pending label publication. The local-only milestone does not report nonexistent publications as pending.
5. Subsequent ordinary reads redact the subject. The public modlog receives one action; the admin sees any remaining restrictions and, when publication applies, delivery status.

Suggested initial reason codes: `spam`, `harassment`, `doxing`, `illegal-content`, `rule-violation`, `moderator-discretion`. Keep private report classifications separate from public reasons. Never automatically copy a report's explanation into the public log. The public mapping is decided (section 1): `csam` is shown as `illegal-content`, and `illegal-content`/`doxing` entries omit the subject URI publicly. Decided 2026-09-29 (Q-ORACLE): omitting the subject keeps doxing and illegal-content subjects out of the browsable and filterable log, but an observer who already holds a URI can see that it is removed and, finding no public entry for it, infer that the reason is doxing or illegal content; the user accepted this on 2026-09-29. The generic removal label carries none of these classifications.

### 5.4 Restore flow

1. Admin selects **Restore** against a specific active local decision and provides a reason. Decided 2026-09-29 (Q-RESTORE-REASON): the restore reasons exclude `illegal-content` and `doxing`, which `restoreContent` rejects with `UnsupportedReason`.
2. Append a restoration event referencing that decision; do not delete or rewrite the original action.
3. Clear only that source/scope's restriction and publish its retraction when applicable.
4. Recompute effective visibility. If another source still removes it, the author deleted it, or community admission/access disallows it, it stays unavailable. Show this outcome explicitly to the admin.

Restoration never recreates an author's deleted PDS record or re-accepts a community-rejected post. If local content was purged, require a valid current record before serving it again; do not reconstruct it from public audit text. Restoring an edited record requires inspection of the current version.

### 5.5 NSFW classification and full AppView removal

**Confirmed behavior:** an admin can mark a post NSFW and retract that classification. It uses the same NSFW badge, blur/concealment, Show/Hide interactions, and existing presentation preferences as author-applied NSFW in each client. Do not add a new filter or delisting behavior. An otherwise eligible post remains in the same feed/search/profile results and retains its ranking eligibility; direct links still work.

The initial UI actions are **Mark NSFW**, **Retract NSFW label**, and the separate **Remove** / **Restore** actions. The NSFW actions target the post as a whole, not one blob or embed. Automated labeling, additional classification values, and moderator-applied comment warnings are later scope.

1. Authenticate the admin and verify the reviewed post/version and expected moderation state, as for removal. Persist the classification and its audit action atomically; when federation is enabled, include its own durable publication intent.
2. Store the decision under its authority, scope, subject URI, and label value. Recommend URI-scoped NSFW so an edit cannot silently discard the admin's classification; the reviewed CID remains audit metadata. Do not update the author's `record.labels` or fabricate an author self-label in a returned record.
3. The served post view includes active moderator classifications separately from the verbatim record. Clients compute NSFW from the author's active self-label **or** any active classification applied by the AppView from a local/trusted authority. Retain provenance; raw arbitrary external labels do not directly become trusted UI state.
4. Retraction references the active local NSFW action and clears only that authority/scope/value. Another authority's NSFW label or an author's self-label keeps the existing sensitive-content presentation active. A deleted/unavailable post does not prevent retracting a decision about its URI.
5. A removal always takes precedence over blur. Applying or retracting NSFW cannot restore removed content; restoring content cannot clear NSFW labels. If the removal is lifted while NSFW remains, the post returns with the existing NSFW treatment. A Show/reveal control never reveals an AppView-removed post.

“Remove from the entire AppView” means the independent instance-removal action applies across all ordinary serving surfaces, including direct reads, feeds, search, profile activity, quotes, and cached hydration. This remains reversible local enforcement, not deletion from the author's PDS. NSFW classification alone is intentionally revealable and is not a security boundary for fetching content. Coves-controlled media serving and shared-blob rules are decided (section 9, Media and retained content).

The web currently derives sensitivity from `post.record?.labels`; mobile's `PostView.isSensitive` also reads only the record's self-labels. Both must consume the new view metadata before moderator labeling is usable. Test existing non-image concealment and accessibility behavior too, not just image blur. Existing supported-client rollout checks must include NSFW metadata; do not fake self-labels to make an old client appear compatible.

## 6. Public modlog

### User experience

- A site-wide **Moderation log** page accessible without authentication.
- Reverse-chronological, cursor-paginated individual actions; stable ordering by server timestamp and action ID.
- Filters: action, subject type/URI, community where publicly disclosable, issuing authority, local versus inherited, and date range. Include actor filtering; public actor identity is approved.
- Each item shows action, time, scope, authority, safe subject reference, reason code, and linkage to the action it reverses.
- Decided 2026-09-30 (Q-LABEL-LOG): NSFW label applies and retractions are left out of the public modlog. The admin log keeps them, and the blur on the post is the public signal.
- Admin view adds authenticated actor DID, private notes, linked report IDs, and publication/ingestion status when applicable. Public fields are explicitly allowlisted; never serialize private records and strip a few fields afterward.

### Audit contract

- Every accepted remove/restore or NSFW apply/retract mutation creates one immutable event. Retrying the same idempotency key returns the original result. A redundant no-op is explicitly reported and must not invent another successful action.
- Scope idempotency keys to authenticated actor DID and authority DID. Bind each key to a canonical request fingerprint, including operation, subject/decision, expected version, reason, and notes; reuse with a different request is an explicit conflict. Authorize before replaying a stored result. Specify a bounded retry/retention window and admission limits; after expiry, monotonic state versions and immutable decision references must still prevent an old retry from reversing newer state.
- Preserve removals, restorations, label applications/retractions, and subsequent reapplications as separate events even though the active state changes. Include the affected label value on classification actions.
- Do not silently rewrite history to correct public text. The initial API has no modlog edit/delete/amend endpoint. An approved operator-only safety-redaction procedure must retain action identity/provenance, record the responsible actor and reason, and prevent disclosure through old public projections/caches; storage retention/deletion rules for the sensitive original are separate. A general amendment UI/API is later scope, not part of the confirmed content actions.
- Do not expose reporter identities, report text, private notes, removed content snippets, credentials, or evidence blobs.
- For content whose existence is access-restricted, show a generic restricted-subject entry rather than revealing its URI/community. Publishing a label also reveals its subject: restricted subjects need an explicit sharing policy, not merely modlog filtering. Decided 2026-10-03: removals are public by default. The public log names a subject (URI and CID) and selects it under its community, collection and subject filters while the subject's own post or comment row is indexed, soft-deleted or not, whatever its community admission status (pending, rejected, community-removed or unadmitted included). A public entry carries no community field of its own; the association is visible through the community filter and the record itself. Only a subject whose row is gone, as after account erasure, is restricted: listed without subject or community, and excluded from subject, collection and community filters. CSAM and doxing stay covered by the reason rule in section 1.
- A signed label proves that the authority issued a signal. It does **not** prove which human clicked Remove. Local actor attribution comes from the authenticated audit record; public actor attribution is an assertion by that authority.

### Inherited actions

Show inherited changes as **Applied removal from [authority]** or **Removal retracted by [authority]**, attributed to the source and the local trust policy. Do not attribute them to a fabricated local human moderator.

Inherited NSFW applications/retractions likewise identify the source and label value. They change classification/presentation only, not the subject's removal state or feed eligibility.

Labels alone do not carry a remote actor DID, explanation, community scope, or action ID. The receiving modlog must work with source/subject/value/timestamps and ingestion metadata alone. Optional links to a source's public action history are supplementary; absence or failure of that history must not block enforcement. Public-label history is not a complete remote administrative audit log.

## 7. Labeling and federation contract

### 7.1 Why labels fit, and what they cannot express

The [atProto label specification](https://atproto.com/specs/label) provides signed statements with source DID (`src`), subject URI (`uri`), optional version (`cid`), value (`val`), issuance time (`cts`), optional expiry (`exp`), and negation (`neg`). Sources expose queries and a resumable label stream.

That directly supports following another authority's removal and later retraction. Enforcement remains an explicit receiver policy. Labels are separate protocol objects: they are **not** ordinary repository records delivered by Coves' existing wanted-collection Jetstream subscription.

A standard label cannot encode the complete moderation action or an arbitrary community-scope field. Do not add unsigned scope fields or pack DIDs, reasons, or structured data into `val`. Keep these in the action model and verified authority configuration.

### 7.2 Proposed wire contract

- Give the instance moderation authority a stable labeler DID with an `#atproto_label` verification key and an `#atproto_labeler` service endpoint. Decided 2026-09-20: this is the instance's own `did:web` (`INSTANCE_DID`). Use a label-signing key distinct from its `#atproto` key, and make the hermetic stack serve a resolvable DID document for it through the configured local resolver.
- Emit a **URI-scoped** removal label: omit `cid` so editing the record cannot evade removal. Preserve the reviewed CID in the action history instead.
- Recommend `!takedown` for vocabulary compatibility with the hard-redaction signal described in the current protocol's labeler-header specification. Here enforcement comes from the receiving AppView's mandatory trust policy on every applicable read, not from a client requesting that header behavior. The signal does not claim that the author's PDS deleted the record.
- `!hide`/custom “hide” definitions are not automatically equivalent to irreversible-to-the-viewer server redaction. Validate `!takedown` support in the chosen publisher/consumer before committing the schema; older tooling and the core lexicon's known-values list may lag the prose specification.
- Restore emits a later label with the same `src`, `uri`, and `val`, and `neg: true`. A negation retracts that source's assertion; it is not a global “safe” verdict.
- For post NSFW, publish `val: nsfw`, with `cid` omitted under the recommended URI-scoped policy. This matches Coves' existing NSFW vocabulary; publish its definition as a revealable content warning for compatible consumers, not as a claim that every atProto client already knows this custom value. A later `neg: true` for `nsfw` retracts only that classification. The `!takedown` and `nsfw` lifecycles are independent.
- Exchange full signed objects (`ver` and `sig` required between services), validate against the source DID's designated label key, and retain provenance. Resolve keys through the configured local/production identity infrastructure.
- Provide `com.atproto.label.queryLabels` and `com.atproto.label.subscribeLabels`, with defined cursor retention, recovery, and key-rotation behavior. An optional Bluesky declaration record does not make Bluesky's AppView understand Coves collections.

Do not make moderation reason codes separate enforcement labels in this release. There is one removal signal and an independent NSFW classification per authority/subject; each is retracted by its own value. Reasons remain in the action log. A labeler declaring a warning must not implicitly acquire takedown authority.

### 7.3 The requested cross-instance example

1. Community **X** is hosted on instance **Y**. A comment **C** under a post in X is indexed by both Y and Coves.social.
2. A Coves.social admin removes C. Coves redacts C locally, logs the action, and publishes its authority's signed URI-scoped removal label.
3. Y's operator has explicitly configured Coves' labeler DID as a trusted removal source, either for all supported Coves content or a configured subset of communities.
4. Y ingests and verifies the label, establishes C's community relationship from validated indexed content, applies its trust policy, and displays **[Removed by server admin]** with Coves.social as the source. Replies follow Y's approved thread policy.
5. Y records an inherited enforcement event. It does not delete C from its author's PDS or write a community-X removal record on Coves' behalf.
6. Coves restores C and publishes the negation. Y clears the inherited restriction after verification, unless another applicable restriction remains.
7. An instance that never trusted Coves continues to make its own decision. Federation does not mean universal enforcement.

### 7.4 Trust and conflict rules

- Subscribe by **DID**, not just endpoint hostname. Endpoint/key discovery does not itself grant trust.
- Trust policy names accepted label values, supported subject collections, and optional community restrictions. Accepting `nsfw` enables its warning treatment, not removals; accepting `!takedown` enables removal enforcement. Unrecognized values do not grant new powers. NSFW is post-only in this release.
- Receive/verify labels for not-yet-indexed subjects into pending state with configured per-source record/byte limits; do not require subject arrival before the label. Apply persisted decisions before the content's first eligible read once its scope is known. On capacity exhaustion, pause that source without advancing its cursor past unpersisted events and surface an actionable health failure; do not silently evict decisions or declare the source caught up. For a stalled source trusted to remove content, reconcile newly indexed subjects' labels or hold them unavailable before serving. A classification-only source does not gain power to withhold posts through an outage: retain known NSFW state, report staleness, and reconcile. The implementation plan must specify limits and a recovery procedure, including expired-cursor recovery, and test the full-capacity case.
- For community-restricted subscriptions, an unknown or forged subject/community relationship cannot authorize removal. For instance-wide policy, a valid trusted exact-URI label can be retained before hydration.
- Any active applicable removal blocks ordinary visibility. Neither local restore nor a remote negation overrides another source's restriction.
- NSFW labels combine across applicable sources without becoming removal restrictions. Retracting `nsfw` cannot negate `!takedown`, and restoring `!takedown` cannot negate `nsfw` or an author self-label.
- No per-subject “ignore trusted removal” override initially. An operator can stop trusting a source; trust-policy changes require attributed audit history and recomputation of affected effective state.
- Preserve original provenance. Do not reissue inherited labels as if they were local decisions: that would create trust loops and amplify one source into apparent consensus.
- Subscriptions are not transitive. Trusting Y does not automatically mean trusting every source Y follows.
- Local mandatory policy is applied independently of user-selectable label hydration. A client-controlled `atproto-accept-labelers` header never widens or narrows the set of mandatory removals. Standard header semantics apply only to optional user-selected label services if that later feature is implemented; an empty optional preference cannot disable server policy.

### 7.5 Delivery, ordering, and outages

- Local enforcement must not depend on an outbound network call succeeding. Use a durable delivery mechanism, expose pending/failed publication to admins, and retry without duplicating actions.
- Serialize application/retraction/reapplication publication for each source/subject/value, for removals and NSFW independently. Assign strictly increasing issuance times for locally issued changes; retries must not give old decisions new timestamps that overtake later retractions.
- Deduplicate incoming events and preserve negation state so replayed older positive labels cannot resurrect a removal. Within each source/subject/value, use the label's `cts` for currency, consistently across queries and streams. A later stream sequence carrying an older `cts` does not replace newer state; sequence controls delivery/checkpointing, not label currency. No ordering across sources is required because their restrictions are independent.
- Define deterministic handling for equal-timestamp conflicts and implausibly future-dated labels before shipping; quarantine ambiguous input rather than let arrival order change policy.
- Bootstrap by backfill plus stream handoff without an event gap. An expired stream cursor triggers reconciliation, not silent cursor reset that loses removals or negations.
- During a labeler outage, retain known active decisions until a verified retraction, applicable expiry, or explicit trust change. Surface staleness; do not assert that unseen new decisions have been applied.
- Verify signatures on ingestion, refresh DID documents for possible key rotation, and reject invalid labels. Do not synchronously resolve a remote labeler for every content read.

When federation is first enabled after the local milestone, export the currently active, sharing-eligible local decisions and establish a gap-free handoff to new mutations. Preserve original action history and use new label issuance times for this initial publication. Do not replay every historical remove/restore as if it just happened, and do not lose a concurrent restoration between baseline export and live publication. Delivery status begins when an actual publication obligation is created; excluded/local-only decisions are not perpetually pending.

## 8. Extension to community-admin moderation

Use explicit **authority + scope + subject + action + actor** from the start. Implement only instance permissions initially; do not build a generic permission language.

Later community work adds:

- Verified community-admin authorization, constrained to that community's content.
- Community-filtered views of the same action history and shared remove/restore and NSFW apply/retract UI.
- Community authority independent from its hosting instance. Moving hosts must not silently convert community decisions into the old host's permanent authority.
- Integration with existing `social.coves.community.acceptance`/`removal` semantics for post membership. A community removal is authoritative for membership in that community; an instance label is policy for a serving instance. Restoration in one layer does not restore the other.
- A defined portable comment-removal authority. Prefer a community-specific labeler identity, potentially served by shared infrastructure, if community labels are published. A single instance-wide labeler cannot express arbitrary community-only scope by adding fields to the standard label object.

The current community removal record pins a post CID for audit but enforces by URI, which aligns with the proposed edit-resistant instance policy. It has no complete human-actor history and is deleted on restore; it cannot serve as the append-only public modlog by itself.

Before implementing community moderation, decide whether labels merely project community repository decisions or are authoritative for particular actions. Do not maintain two independently editable sources of truth for the same community decision. The initial release need only preserve this separation, not solve community delegation now.

### 8.1 Follow-up: moderation across the Lemmy/ActivityPub bridge

Not part of this release. Recorded 2026-09-20 from a federation review of this PRD against Tidepool. Nothing here requires a change to the lexicons this PRD publishes; the work is expected to be mostly in Tidepool, with one Coves lexicon dependency noted below.

**Current state (verified in Tidepool source):**

- Lemmy content moderation arrives as `Delete` with a present `summary` (author deletion omits it); `Undo{Delete}` restores. `Add`/`Remove` are skipped, site-scope bans are unsupported, and locks are stored only to gate outbound replies (`tidepool/internal/ingest/consent.go`, `handler.go`, `moderation.go`). `PLAN.md` decision 18 uses older `Remove`/`Undo{Remove}` wording.
- **Posts, inbound: works.** A Lemmy moderator removing a post atomically deletes its `community.acceptance` and writes a `community.removal` (`moderator-discretion`, or `author-banned` for ban-driven removals) without touching the author's record. Coves renders it as `#removedPost`.
- **Comments, inbound: gap.** `community.removal` is post-scoped. A Lemmy moderator removing a native Coves comment is recorded only in Tidepool's database, so the comment stays visible in Coves. A moderator-removed Lemmy-origin comment arrives as a repository deletion, which Coves indexes with `deletionReason: author` and shows as deleted by its author.
- **Outbound, Coves to Lemmy: nothing.** Tidepool's outbound translator covers posts, comments, votes, and person deletion. No Coves moderation decision is delivered to Lemmy. Instance removals are deliberately not federated over ActivityPub (section 5.1).

**Follow-up scope:**

1. **Inbound comment moderation.** Define a portable comment-removal signal that Tidepool can write and the AppView can index, then render it through the existing placeholder with `deletionReason: moderator` and community-scoped `moderation` attribution. This is the one Coves-side dependency: either a comment-capable successor to `community.removal` or a separate record, decided with the community-moderation PRD (section 8). Do not widen the published post-only `community.removal` in place, and do not infer moderation from a bare repository deletion.
2. **Inbound post moderation.** Keep the working acceptance/removal path. Extend it only where the community PRD needs it: moderator attribution when a verified mapping exists (omit the actor otherwise), lock state surfaced to Coves, and target-sensitive handling of `Add`/`Remove` and site bans.
3. **Outbound moderation, Coves to Lemmy.** Deliver Coves moderation decisions on bridged content as the ActivityPub activities Lemmy expects. A community-authority decision for a Coves-hosted community has a natural sender (the community's Group actor). Whether an instance decision is ever delivered, and under whose authority, is an open product question: an instance has no ActivityPub authority over a Lemmy-homed community, and sending as the author is not acceptable. Tidepool consumes repository records, not labels, so outbound delivery needs a repository-visible signal or a new consumer input.
4. **Acceptance cases.** Both restore orders between a community removal and an instance removal; a `bridgedStats`-only record rewrite and a bridge restore at the same URI leaving an instance decision in force; legacy community-repository content alongside author-owned `postv2`.

## 9. Technical requirements and integration boundaries

### Durable state

The moderation authority needs durable action history, private annotations, current decisions, and label-publication state. Each consuming AppView needs verified label state, trust policy, cursors, and effective visibility projections.

These may share deployment infrastructure, but **moderation-service state is not disposable AppView indexing state**. Backups and rebuild procedures must preserve decisions and audit history. AppView reindexing must not wipe removals. This is a new service responsibility to approve explicitly; the AppView still never edits CAR files or PDS internals.

Logical records, not a prescribed table layout:

| Record | Minimum information |
|---|---|
| Action | Stable ID, actor DID, authority DID, scope, subject URI/type, observed CID, action, classification value when applicable, reason, server time, reversal link, idempotency identity |
| Private annotation | Action/report link, author, note, time; separate access-controlled projection |
| Current decision | Authority/scope/subject, independent removal and content-label decisions keyed by value, active action references, concurrency version |
| Label and delivery | Exact signed payload, issuance/signing metadata, delivery state, ordered publication identity |
| Subscription | Source DID, accepted values/collections/scope, policy version, cursor, health and reconciliation status |

An action should not require a foreign key to an existing content row: content can be deleted or arrive after a label, but the decision must survive.

### API and clients

Proposed API family under `social.coves.moderation.*`: remove content, restore a decision, apply/retract post NSFW labels, list actions, and get effective moderation state. [Section 14](#14-lexicon-change-inventory) inventories candidate NSIDs, types, affected response shapes, and publishing gates. These are design proposals, not published schemas.

- Mutation requests carry the subject/decision, reason, private/public text separately, idempotency key, and expected state/version. The server supplies actor and authority.
- Return action ID and effective state, plus publication status only when publication applies. Use explicit error codes for forbidden access, invalid subject, stale state/content, missing decision, and temporary failure.
- Public post/comment views expose a machine-readable removal state and safe source attribution. Removed content is absent at the server boundary; clients do not infer removal solely from an empty string or parse display copy. NSFW posts retain their normal record/content and gain separate active-classification metadata for the existing client concealment UI.
- Audit all read paths: community/timeline/Discover feeds including existing snapshots and continuations, search, actor activity, thread headers/replies, batch hydration, quotes, previews, notifications, and author-only status views.
- Apply eligibility before page construction and recheck at hydration; refill where needed. A cached candidate or quoted snapshot must not bypass a new removal.
- Web must supply the admin workflow and public modlog for launch, including NSFW application/retraction. Web and mobile must understand tombstones and moderator NSFW metadata before backend rollout; a dedicated mobile admin console can follow. Public modlog access from mobile may initially open the web page.
- New public backend routes require the production Caddy AppView allowlist to be updated alongside deployment.

### Media and retained content

Removing a record reference alone does not prevent someone replaying its cached image URL. The image proxy currently addresses content by DID/blob CID, so implementation must inventory every Coves-controlled media/cache route before claiming complete suppression.

Decided 2026-09-20: removal covers Coves-served media bytes, enforced on cache hits as well as misses, with cached bytes purged. Media served directly from a third-party CDN (Bluesky embeds) is outside Coves' control; removal stops Coves from handing out those URLs. Shared-blob semantics are decided too: block the (owner DID, blob CID) pair for every removal, and block the CID for every owner when the reason is `illegal-content`. Decided 2026-09-28 (Q-I6): the every-owner block covers only the CIDs indexed when the admin acted; an image the author adds after the removal (by an edit, a recreate, or a create the consumer does not index) gets an owner-scoped block only, so an author cannot get a copy of someone else's image blocked for every owner. "Owner DID" means the repository that holds the blob, which is not always the human author: legacy bridged posts live in a Tidepool community repository, so several authors' legacy posts can share one community-DID/CID pair and a pair block then suppresses that image on all of them. That consequence is accepted. The media inventory and acceptance cases must cover both legacy community-owned blobs and author-owned `postv2` blobs. Restoring the decision lifts its blocks. Test both rules, including warm-cache requests, before rollout.

Raw evidence retention and admin inspection are separate from public serving. Avoid copying content into the action ledger; choose a retention policy before implementing retained-evidence storage. A moderation restore cannot reverse an independent legal/storage purge.

### Reuse versus build

Checked 2026-09-20: the pinned `indigo` module already ships `atproto/labeling` (`Label.Sign`, `VerifySignature`, lexicon conversion), so signing and verification are not new work. What remains to build for publication is a sequenced label table, `queryLabels`, and the `subscribeLabels` stream; trusted-label ingestion has to be built regardless, because Ozone publishes labels and does not consume them for a third-party AppView. Recommendation: build the publisher in Go and do not deploy Ozone, which would add a Node service with its own database and event log as a second action store. Confirm before the federated milestone. Its public UI/docs emphasize Bluesky record rendering; native Coves post/comment inspection, authenticated actor attribution, private notes, and the public Coves modlog must be demonstrated, not assumed.

Choose one authoritative action store and a reliable projection into labels; do not perform unrelated dual writes to an Ozone history and a Coves history. A small Coves-specific moderation UI with a reused label publisher is a reasonable outcome. The selection is a short prerequisite investigation, not a commitment to deploy all of Ozone.

## 10. Acceptance criteria and validation

### Local milestone

1. Two distinct real accounts can be configured as admins. Either can remove/restore; an ordinary account, forged actor field, and community creator without an admin grant cannot.
2. Remove a nested comment: it carries `moderator-removed` and instance scope, displays **[Removed by server admin]**, and has no body/embed/facets in responses. The agreed identifying-metadata allowlist is consistent across clients, and visible descendants remain correctly threaded and paginated. Future community scope changes the attribution/copy, not the removal-state value.
3. Remove a post: no feed/search/profile/quote/status or existing Discover continuation exposes its content; its thread returns `NotFound`, comments under it are absent from profile activity, and single-post reads return the approved content-free view without widening community access. Restore brings the thread back.
4. A removal survives an edit, duplicate firehose delivery, restart, and delete/recreate of the same URI. A different URI is a different subject; repost detection is not implied.
5. Restore appends history and releases only its own decision. It cannot revive author-deleted content, bypass admission, or clear another source's removal.
6. Concurrent admins and request retries yield a coherent final state and exactly one action per accepted mutation, with explicit conflict/no-op responses. Cover cross-actor key reuse, same-key/different-body conflicts, authorization revocation, and stale retries after key-retention expiry.
7. The public modlog lists every successful remove and restore with stable pagination; private notes/report data and restricted subjects do not leak. Original actor attribution survives role revocation.
8. A storage failure cannot leave an unlogged effective action or a success entry for an unenforced action. Local-only decisions have no pending-publication claim. Moderating a locally hosted community's content leaves its community acceptance/removal records untouched.
9. Web and mobile render agreed placeholders without crashing or revealing removed content. Coves-controlled media behavior matches the approved policy, including warm-cache requests.

**NSFW local acceptance cases:**

- An admin marks an otherwise eligible post NSFW: web and mobile use the existing badge/blur/concealment and Show/Hide behavior for the viewer's existing settings. The post stays in the same default feed/search/profile eligibility and receives no NSFW-specific ranking penalty.
- The author's record, CID, and self-labels are untouched by the moderation write. Ordinary viewers and the author cannot clear an admin label through the author-edit API. Post edits do not silently erase the active URI-scoped decision.
- Application and retraction each create one attributed admin-log action (not in the public modlog); retries do not duplicate them. A non-admin or an unsupported subject/value cannot create or retract a moderator classification.
- Retracting one source's label leaves NSFW presentation active if another applicable source or the author's self-label remains. A post with no remaining NSFW source returns to its ordinary presentation on refreshed state.
- A post can be both NSFW and removed. Removal suppresses it across the AppView even after it was revealed or cached; NSFW retraction does not restore it, and restoring it preserves any remaining NSFW treatment.
- Moderator label metadata reaches feeds, direct post reads, and refreshed cached views. Supported clients do not accidentally treat omission from a truncated source preview as the absence of NSFW.

### Federated milestone

10. In a hermetic stack, two separately configured AppViews index a real comment in community X on Y. Coves' signed removal causes Y to redact only after Y explicitly trusts the source.
11. An untrusted labeler, wrong signing key, unsupported collection/value, forged community relationship, and out-of-scope subscription cannot impose removal.
12. Restore/negation propagates; old replay cannot undo it. Two trusted sources can remove one subject independently, and retracting one leaves the other effective.
13. Labels arriving before subjects, pending-state capacity exhaustion, consumer restart, duplicate/out-of-order delivery, and expired cursors recover without silent loss of active removals or retractions. A higher stream sequence with an older `cts` cannot replace newer state; equal-timestamp conflicts follow the approved deterministic quarantine policy.
14. Y's modlog distinguishes inherited decisions from local admin actions. Y does not re-publish Coves' decisions under its own identity.
15. Rebuilding AppView indexes from durable state preserves enforcement; stopping trust is audited and recomputes only that policy's effects.
16. Publication failure leaves local removal effective, its retry durable, and delivery status visible. Enabling federation exports existing eligible active decisions exactly once logically, with a concurrent restore/re-remove handled without a gap or stale final state.

**NSFW federation acceptance:** a trusted post-scoped `nsfw` label causes the same existing blur/reveal treatment, and its negation clears only that issuer's classification. A source trusted only for `nsfw` cannot impose removal. Cross-value retractions, duplicate/out-of-order events, restart, baseline export, and publication failure preserve independent NSFW/removal state. A classification-only source outage does not exclude posts from feeds.

Follow [TEST_ARCHITECTURE.md](TEST_ARCHITECTURE.md): broad policy/authorization/state-machine cases at T0; SQL, transactions, pagination, and projection races at T1; a small number of real label-service/PDS/AppView pipeline contracts at T2. Extend the hermetic stack for a second AppView and label service as needed. Respect local PLC/PDS configuration throughout. No mocked federation E2E and no infrastructure skips. `make ci` remains the merge gate.

**Proposed operational targets:** local reads started after a successful mutation observe its enforcement; healthy connected peers apply a published label within 30 seconds at p95; private dashboards expose publication backlog, oldest pending age, consumer lag, rejected labels, and reconciliation failures. Validate targets under representative load before promising them publicly.

## 11. Delivery plan

| Phase | Deliverable | Exit condition |
|---|---|---|
| 0. Product and protocol decisions | Resolve section 12; inspect Ozone/reusable publisher support; map read/media paths and settle lexicons | Approved behavior, authority/storage boundary, wire contract, and test plan |
| 1a. Lexicon package (one change, published once) | All local-milestone schema work together: the new `moderation.defs` definitions and reason tokens (without `#publicationView`), the moderation endpoint files, the `commentView` correction and its runbook exception, `#moderatedPost` and the `post.get` union member, `postView.moderation`, the `blockedComment` description, fixtures and `cmd/validate-lexicon` entries, and the `LEXICON_PUBLISHING.md` rewrite (file-level holdback list, explicit-file publish command, publish order). | Section 14.6 gates pass locally. Publication happens once, after the package is reviewed: `moderation.defs`, then `comment.defs`/`post.defs`, then endpoints, by explicit file path so the held-back ban/tribunal schemas are not swept in |
| 1. Local moderation backend | DID authorization, durable decisions/history, remove/restore, NSFW apply/retract, safe public/private APIs, read-path enforcement and label hydration | Local backend acceptance cases pass, including retry/concurrency and leak tests |
| 2. User-facing launch | Web admin tools/modlog; web/mobile tombstones and moderator NSFW integration with existing concealment; route and cache changes | End-to-end local moderation is usable; client-first rollout verified |
| 3. Federated moderation | Signed publisher with durable retry and initial state export, trusted-source ingestion, reconciliation, inherited modlog entries | Two-AppView acceptance cases pass; operator runbook and backup/recovery demonstrated |
| 4. Community moderation | Separate PRD using the existing action/history/enforcement model | Community authority, membership, and label projection contracts approved first |
| 5. Bridge moderation (follow-up, mostly Tidepool) | Section 8.1: inbound comment moderation, inbound post extensions, and outbound Coves-to-Lemmy moderation delivery | Portable comment-removal signal and outbound authority model approved first; depends on phase 4's community authority contract |

Phases 1–3 are multiple PR-sized changes, not one endpoint-sized feature. The dominant effort is consistent serving-path enforcement and reliable label lifecycle, followed by cross-client presentation. Keep advanced reports, bans, automation, and community roles out of this first delivery. Estimate dates after the media scope and publisher choice are settled.

Roll out lexicons and client tombstone support before enabling admin actions. Enable publication only once retraction, identity keys, durable history, and delivery monitoring are operational. A rollback must preserve existing enforcement and audit data rather than return to a version that silently serves removed content.

## 12. Decisions still needed

| Decision | Recommendation | Why it matters |
|---|---|---|
| Label publisher | Build it in the Go AppView codebase on `indigo/atproto/labeling` (already a dependency; signs and verifies labels). Do not deploy Ozone | Identity is decided (instance DID); signing, streaming, and storage ownership are not |

The first-release action set and modlog visibility are already decided: **remove/restore posts and comments, apply/retract post NSFW labels using existing blur/reveal only, and public history with private notes**. Full AppView removal remains independent from NSFW; NSFW does not exclude posts from default feeds.

## 13. Protocol sources

Consulted 2026-09-17; pin dependency versions and recheck wire behavior during implementation.

- [atProto Labels specification](https://atproto.com/specs/label): signed labels, source/subject/value, CID scoping, negation, keys, service discovery, query/stream endpoints, and redaction headers.
- [atProto Moderation guide](https://atproto.com/guides/moderation): composable moderation and separation of content distribution from reach.
- [Core label lexicon](https://github.com/bluesky-social/atproto/blob/main/lexicons/com/atproto/label/defs.json): wire schema and open known-value vocabulary; compare deployed tooling with current prose specification.
- [Ozone](https://github.com/bluesky-social/ozone) and [Using Ozone](https://atproto.com/guides/using-ozone): existing moderation UI/service capabilities and deployment model; not evidence of drop-in Coves integration.
- [Draft Lexicon Style Guide, discussion 4245](https://github.com/bluesky-social/atproto/discussions/4245), its [published style guide](https://atproto.com/guides/lexicon-style-guide), and the [Lexicon specification](https://atproto.com/specs/lexicon): naming, reusable definitions, wire types, evolution, and pagination rules applied in section 14.

## 14. Lexicon change inventory

**Status:** Implementation inventory, checked against discussion 4245 and the published style guide on 2026-09-17. The local-milestone schema JSON in this inventory is implemented; publication follows `LEXICON_PUBLISHING.md`. Final wire definitions depend on the product decisions in section 12; neither this inventory nor schema publication grants admin authority.

Paths below are relative to `internal/atproto/lexicon/social/coves/`. A schema `object` used in API responses is not a repository `record`: the moderation service's action history does not need a new public repository collection just to have a typed XRPC API.

### 14.1 Style and evolution rules

| Guidance | Application to this inventory |
|---|---|
| `lowerCamelCase`; verb–noun endpoints | New endpoints use `removeContent`, `restoreContent`, `labelContent`, `retractContentLabel`, `listActions`, `listAdminActions`, and `getSubjectState`. Fields use `actionId`, `authorityDid`, `labelValue`, `createdAt`, etc. Errors use `UpperCamelCase`; simple status values use `kebab-case`. |
| Separate feature groups and reusable `.defs` | New operations live directly under `social.coves.moderation.*`; shared types live in `moderation.defs`. Do not introduce an `action` record plus an `action.*` endpoint hierarchy or put shared view types in an endpoint's `#main`. |
| Preserve published naming and types | Existing Coves noun/operation hierarchies are a documented legacy convention in `LEXICON_PUBLISHING.md`, not a reason to rename published APIs. Apply the linked guide to new moderation names; retain the existing group if a thread endpoint is versioned later. Document this limited naming decision in the publishing runbook during implementation. |
| Open vocabulary and unions | Use `knownValues`, not closed enums. Use tokens for extensible subjective moderation reasons. All new unions are open (`closed` omitted or `false`); unknown variants must not gain permissions or render arbitrary content. |
| Correct primitive formats | Record subjects use `at-uri`; reviewed versions use `com.atproto.repo.strongRef`; account inputs use `at-identifier`; stored/output identities use `did`; times use `datetime`; CIDs use string `cid`, not `cid-link`. The `community` filter is a documented application-identifier exception supporting bare names and `name@origin` as well as account identifiers. Do not combine a string format with redundant length limits. |
| Small, bounded values | Opaque IDs/state versions have byte limits; public/private text has both grapheme and byte limits. Arrays have documented maxima. Use response objects for lists, not parallel arrays of DIDs/IDs. No evidence payloads or content snapshots in modlog definitions. |
| Minimal required fields | Mark fields required only where an action, identity, concurrency check, or response would be unusable without them. Optional means omitted, not implicitly `null`; `unknown` does not permit `null` unless explicitly nullable. |
| Explicit endpoint descriptions | Every `main` describes purpose, authentication, authorization, and personalization. Every endpoint has an `output` with `encoding: application/json`; procedures have JSON input too. Explain every URI/CID/version's subject and semantics. |
| Protocol discriminators | Do not declare a property named `$type`. Emit protocol `$type` discriminators on union variants; plain object refs do not need them. Unions must be inline schema positions, not named top-level `defs` or refs to other unions. |
| Published compatibility | Add optional properties, new definition fragments, and variants to existing open unions where compatible. Do not change an existing ref into a union, make a required record optional/nullable, or change community-removal semantics in place. Use a new endpoint/type for those changes. The single approved exception is `commentView.record` (section 14.4), which corrects the schema to what the AppView has always served. |

The new names below are stable-design candidates, not permission to experiment under published stable NSIDs. If live-network experimentation is necessary before agreement, use a distinct `.temp.` namespace and separate fixtures. No blanket `.temp.` migration or unrelated schema cleanup is part of this feature.

### 14.2 New moderation endpoints

All seven endpoints belong to the local milestone. Instance authority and actor identity come from the authenticated service context, never request fields. The initially supported scope is the serving instance; community-write capabilities are not exposed by reserving types for future reads. NSFW procedures initially accept posts only; remove/restore also accept comments.

| New file / NSID suffix under `social.coves.moderation` | Type and access | Proposed contract |
|---|---|---|
| `moderation/removeContent.json` / `removeContent` | `procedure`; authentication and instance-admin authorization required | Required `subject` strongRef, `expectedVersion`, `idempotencyKey`, and `reason`. Optional `privateNote`; no public free-text field in this release. Return `outcome`, post-operation `state`, and the resulting `action` when an action exists. Removal applies to the subject URI; the strongRef CID is the version inspected and checked before acting. |
| `moderation/restoreContent.json` / `restoreContent` | `procedure`; authentication and instance-admin authorization required | Required local `actionId` identifying the removal to retract, `expectedVersion`, `idempotencyKey`, and `reason`; optional `reviewedSubject` strongRef and `privateNote`. Require `reviewedSubject` when a current record exists; allow retraction without one when the record is deleted/unavailable, without making it visible. Return the same mutation-result shape. A restore cannot target an imported source's decision. |
| `moderation/labelContent.json` / `labelContent` | `procedure`; authentication and instance-admin authorization required | Required post `subject` strongRef, `labelValue`, `expectedVersion`, and `idempotencyKey`; optional `reason`, and `privateNote`. The initial supported `labelValue` is `nsfw`, with URI-scoped effect and the CID used for inspection/concurrency. Return `#mutationResult`. This procedure cannot accept `!takedown` or arbitrary label values as a shortcut around the removal workflow. Decided 2026-09-30 (Q-LABEL-REASON): `doxing` and `illegal-content` are Remove-only reasons, so `labelContent` and `retractContentLabel` reject them with `UnsupportedReason`. |
| `moderation/retractContentLabel.json` / `retractContentLabel` | `procedure`; authentication and instance-admin authorization required | Required local `actionId` for an active label application, `expectedVersion`, and `idempotencyKey`; optional `reason`, `reviewedSubject`, and `privateNote`. Require `reviewedSubject` when the current post exists, as for restore. Derive subject/value/source/scope from the referenced action and retract only that local classification. Return `#mutationResult`; a removal action, imported decision, or author self-label is not a valid target. |
| `moderation/listActions.json` / `listActions` | `query`; public, no personalization or extra fields when authenticated | Optional `limit`, `cursor`, `subject`, `action`, `origin`, `authority`, `community`, `since`, `until`, and, if public actor naming is approved, `actor`. Return required `actions` of public `actionView` objects and optional `cursor`. Hidden subjects cannot be identified through filters, counts, cursors, or error differences. Decided 2026-09-30 (Q-LABEL-LOG): `listActions` never returns `label` or `retract-label` actions, under any filter or cursor page, and its `action` filter rejects those two values with `InvalidRequest`; `listAdminActions` keeps them. |
| `moderation/listAdminActions.json` / `listAdminActions` | `query`; authentication and instance-admin authorization required | Same pagination/filter conventions, plus optional local `actionId` for exact lookup. Return required `actions` of `adminActionView` objects and optional `cursor`. This is the explicit private history/notes surface; there is no `includePrivate` switch on the public endpoint. |
| `moderation/getSubjectState.json` / `getSubjectState` | `query`; authentication and instance-admin authorization required | Required `subject` AT URI. Return required `state` of `subjectState`: concurrency version, independent removal/classification state, source-record availability, optional current strongRef/local removal reference, and local label-action references. A syntactically valid URI in a supported content collection has a state even if never indexed: `recordState: unavailable`, initial `version`, and no removal or classifications unless stored decisions apply. Use `InvalidSubject` for malformed/unsupported subjects, not `SubjectNotFound` for an unindexed one. Supplies preconditions for all four mutations; returns no retained raw content. Public summaries are carried by content/tombstone views rather than exposing admin state tokens. |

For a successful state change, `outcome` is `applied` and `action` is required by the endpoint's semantic contract. An unchanged/no-op outcome is `unchanged`; omit `action` unless returning the existing matching action is appropriate and documented. An idempotency replay returns the original result, not a new action. `outcome` uses open `knownValues`; these four procedures cannot be turned into a generic arbitrary-action write surface. `restoreContent` accepts removal decisions only; `retractContentLabel` accepts classification decisions only.

The exact action lookup and replay response must remain available within the idempotency window even if later reads have a different effective state; document that a replay's returned state is the original operation result. Clients refresh `getSubjectState` before a subsequent mutation.

A state for an unindexed URI is not proof that a record exists or that its caller-supplied CID is genuine. `removeContent` and `labelContent` must independently verify the current inspected record and can return `SubjectNotFound` when they cannot establish that subject. Existing removals and classifications remain retractable when their source record disappears. Initial version tokens and subsequent state changes must participate in the same concurrency contract without requiring a content-row foreign key.

**Admin inspection is out of scope** (decided 2026-09-20). If a later release approves a retained-content inspection UI, inventory a separate `moderation/getSubject.json` / `social.coves.moderation.getSubject` query before implementing it. It must require admin authorization and use a bounded, typed available/unavailable result with a verbatim record only when inspection is permitted. Neither a public tombstone nor `getSubjectState` becomes a private-content escape hatch. Inspection is conditional scope; the seven required endpoints do not promise evidence retention.

### 14.3 Shared definitions, fields, and limits

Extend `moderation/defs.json` with the following **new** definitions. The existing `#banView` is unrelated and remains unchanged; adding these definitions does not authorize publishing the old ban/governance endpoint files.

| Proposed definition | Required core | Optional/conditional fields and semantics |
|---|---|---|
| `#actionRef` | `serviceDid` (`did`), `actionId` (opaque string) | Identifies an action in the applying service's log, not necessarily the label issuer's log. IDs are service-local, not fake AT URIs or content CIDs. Restore's local `actionId` is implicitly qualified by the called service. |
| `#scopeView` | `kind` (`instance` or future `community`, open string) | `communityDid` (`did`) for a public community scope; omit identifying scope detail on a restricted-subject public projection. The initial service accepts only instance-scoped mutations. |
| `#subjectRef` | `uri` (`at-uri`) | Optional `cid` (`cid`) means the observed record version, never the version of the moderation action. Allows URI-only references for inbound labels, deleted content, and safe history; use strongRef instead whenever an exact inspected version is required. |
| `#actorRef` | `did` (`did`) | Minimal identity for public actor attribution. No mandatory handle/avatar lookup. Future descriptive fields can be optional. |
| `#actionView` | `ref` (`#actionRef`), `action`, `authorityDid`, `scope`, `createdAt`, `origin` | `origin` uses open `local`/`inherited` values. Optional `subject`, `reason`, `labelValue` (present on disclosed classification applications/retractions), `actor` (`#actorRef`), and `reverses` (`#actionRef`). `createdAt` is this log's server-recorded time; optional `issuedAt` is the source label's `cts`, not a sort key controlled by a remote clock. Subject/actor/reason omission follows the public disclosure policy; absence of a remote human actor is legitimate. |
| `#adminActionView` | `action` (`#actionView`) | Optional `actorDid` (`did`), semantically required for local human actions and authoritative regardless of public `action.actor` disclosure; absent for inherited events without verified human attribution. `privateSubject` (`#subjectRef`) holds subject/version detail suppressed publicly. Separate `privateNote`, optional report-reference objects, and `publication` (`#publicationView`). No operational-log dump or credentials. Use a separate object, not inheritance that merges private fields into the public shape. |
| `#sourceView` | `authorityDid` (`did`), `scope` (`#scopeView`) | Optional public action reference/reason; optional `label` referencing the standard `com.atproto.label.defs#label` once federation ships. Labels on restricted subjects must not leak through this view. |
| `#moderationView` | `state` (open `clear`/`removed`) | `state` describes removal only; `clear` does not imply content exists, is readable, or has no NSFW label. Optional removal `sources` array of `#sourceView`, with `sourcesTruncated` (optional boolean, default false) if incomplete. Optional `contentLabels` array of `#contentLabelView` describes active moderator classifications independently of removal. |
| `#contentLabelView` | `value` (initially `nsfw`) | One aggregate entry per active classification value applied by the AppView. Optional `sources` array of `#sourceView` and `sourcesTruncated` describe provenance without changing the aggregate effect. Only active labels appear; retracted/expired labels remain in history, not this array. The complete set of active supported values must be present even if individual source previews are truncated. Author self-labels remain in the verbatim record, not reissued here as a moderator's assertion. |
| `#localLabelView` | `value`, `action` (`#actionRef`) | Admin-only reference to an active locally retractable classification in the requested subject state. Does not claim authority to retract remote or self-applied labels. |
| `#subjectState` | `subject` (`at-uri`), `version`, `moderation` (`#moderationView`), `recordState` | Optional `currentSubject` strongRef, `localRemoval` (`#actionRef`), and `localLabels` array of `#localLabelView`. `recordState` has open values `present`, `deleted`, `unavailable`; the strongRef is present only for a known current record. `version` is an opaque monotonic-state token, not a record CID, and protects local removal and classification changes against stale writes. |
| `#mutationResult` | `outcome`, `state` (`#subjectState`) | Optional `action` (`#adminActionView`) under the applied/unchanged rules above. Only admin procedures return this private result. |
| `#publicationView` | `status` (open `pending`/`published`/`failed`) | Federated milestone only; not authored or published with the local-milestone definitions. Optional `publishedAt`, sanitized error code. Absent when no publication obligation exists; never expose signing keys, endpoint credentials, or raw failure text. |

`action` starts with `remove`, `restore`, `apply-removal`, `retract-removal`, `label`, and `retract-label`. For the last two, `labelValue` identifies `nsfw` and `origin` distinguishes local from inherited activity. Decided 2026-09-30 (Q-LABEL-LOG): `label` and `retract-label` actions appear only in the admin log, not in the public modlog. Additional observed transitions such as expiry or trust-policy changes need distinct documented values when implemented. Unknown received action/status values are displayable as an unrecognized action, never authorization to mutate state. For source inputs, schema openness does not imply trusting unsupported labels or scopes.

For reasons, define `#reasonType` as a bounded string with fully qualified token references in `knownValues`, and token definitions such as `#reasonSpam`, `#reasonHarassment`, `#reasonDoxing`, `#reasonIllegalContent`, `#reasonRuleViolation`, and `#reasonModeratorDiscretion`, each with a precise description. These subjective classifications benefit from a namespaced extension mechanism. The short codes in section 5 are explanatory names; the new moderation API uses tokens. Do not convert already-published `community.removal.code` values or label `val` values to these tokens. A server may reject unsupported reason tokens on writes with `UnsupportedReason`; accepting a token syntactically does not approve its public disclosure.

Proposed first-publish limits, to be mirrored by runtime validation:

- `actionId`, `expectedVersion`/`version`, and `idempotencyKey`: nonempty opaque strings, maximum 128 UTF-8 bytes; none has `cid`, `tid`, or `at-uri` format unless it actually adopts that protocol syntax.
- Simple action/state/scope/origin/status/error-code strings: maximum 64 bytes. `reasonType` token-reference strings: maximum 640 bytes; not `format: nsid`, because token refs contain fragments.
- `labelValue` / content-label `value`: nonempty string, maximum 128 bytes, open `knownValues` initially containing `nsfw`. The local classification procedures reject other values with `UnsupportedLabel`; `!takedown` remains exclusively in the remove/restore workflow. Public classification reads tolerate future values without guessing their presentation.
- Private notes: maximum 1,000 graphemes and 10,000 bytes. Plain text initially; do not introduce HTML/Markdown interpretation or quote removed content automatically.
- Account-valued query filters `authority` and approved `actor`: `format: at-identifier`. `community` instead uses a nonempty plain string, maximum 320 bytes to match the published community parameters, resolved by the existing community resolver so DIDs, handles, bare names, and `name@origin` remain accepted. This is the established application-identifier exception in `LEXICON_PUBLISHING.md`, not an account-format change. Resolve through configured infrastructure and freeze resulting DIDs in cursor scope. Never use a mutable handle or community name as stored authority or authorization identity.
- `since`/`until`, `createdAt`, `issuedAt`, and `publishedAt`: `format: datetime`. Specify inclusive `since` and exclusive `until` in endpoint descriptions.
- New log pagination: `limit` default 50, minimum 1, maximum 100; `cursor` opaque string maximum 2,048 bytes; `actions` maximum 100 objects. Bind cursors to filters, service, and public/admin visibility context. Missing cursor ends pagination; fewer than `limit` results, including zero, does not. No public totals that count otherwise undisclosable matches.
- `sources`: at most 100 preview objects per removal/classification, with truthful `sourcesTruncated`; effects are evaluated over all applicable sources regardless of preview truncation. `contentLabels` and `localLabels`: maximum 100 value objects each, with only one supported value (`nsfw`) initially; enabling more values later must preserve complete aggregate effects within this bound. Report links, if implemented, are at most 100 objects with bounded opaque IDs, not report text.

Lexicon `params` allow primitives and arrays of primitives only. Put structured strongRefs/actionRefs in JSON procedure bodies or response objects, not GET query parameters. Repeat simple query filter declarations where needed rather than inventing a reusable named `params` definition.

### 14.4 Tombstones and affected content lexicons

The published `comment.defs#commentView` requires a verbatim `record`, but the AppView has served author-deleted comments as a content-free placeholder since soft deletion shipped: `record: null`, `isDeleted`, `deletionReason`, `deletedAt`, a DID-only author, zeroed vote stats, and preserved thread references and `replyCount` (`buildDeletedCommentView`). Web and mobile already render `deletionReason: moderator` as a removed-by-moderator placeholder. **Decided 2026-09-20:** moderator removal reuses this placeholder, and the published schema is corrected in place to describe it. Making `record` optional is an approved evolution-rule exception on the runbook's standing justification (the published schema described something the AppView never served). Unlike the 2026-09-07 exceptions it loosens a required output field, so the runbook entry must say so. No `#commentTombstone`, `#threadViewCommentV2`, or `getCommentsV2` is introduced.

| File / definition | Planned change | Compatibility and reader impact |
|---|---|---|
| `community/comment/defs.json#commentView` | **Approved exception:** remove `record` from `required`; document it as absent/null exactly when `isDeleted` is true. Add optional `isDeleted` (boolean), `deletionReason` (open `knownValues`: `author`, `moderator`), `deletedAt` (`datetime`), and `moderation` (`moderation.defs#moderationView`). Document that a placeholder's `cid` is the last indexed version, its vote stats are zero, `replyCount` is preserved, and `embed`/`viewer` are absent. | `goat lex breaking` flags the `required` change; add it to `LEXICON_PUBLISHING.md` accepted exceptions in the same change that edits the JSON, noting it is output-loosening. The other properties are additive. No reader that works today breaks: the wire shape is unchanged. Instance versus community removal comes from `moderation` source scope, not a new `deletionReason`. Restricted existence still returns an access-safe unavailable response, not an identifying placeholder. |
| Placeholder author (`buildDeletedCommentView`) | Serve `author.handle` as `handle.invalid` instead of the empty string, which fails the required `format: handle`. No handle/avatar/display-name enrichment on placeholders. | Runtime fix, no schema change. Verify web and mobile never display the placeholder handle before shipping. |
| `community/comment/defs.json#blockedComment` | Description-only: `blockedBy: moderator` is published but never emitted; mark it unused. Moderator removal is represented only by the `commentView` placeholder. | Do not remove the known value (breaking) and do not start emitting it as a second representation of removal. |
| `community/comment/getComments.json` | No schema change. A removed post returns the existing `NotFound` error (decided 2026-09-20). Removed comments appear as placeholders at any depth with their visible descendants intact. | One endpoint for old and new readers. Preserved discussion under a removed post is later scope and would need a versioned endpoint with a post-header union. |
| `internal/core/comments/view_models.go` | Correct the stale doc comments naming `getComments#commentView` and `social.coves.feed.getComments`; the NSIDs are `social.coves.community.comment.defs#commentView` and `social.coves.community.comment.getComments`. | Comment-only. |
| `community/post/defs.json` | Add `#moderatedPost`: required `uri` and `moderation` (`moderation.defs#moderationView`, semantically removed); optional `authorDid` (`did`) and safe community reference. No record/title/embed. Retain `#removedPost` for its existing community-removal meaning. Update `#blockedPost` and its `blockedBy` descriptions so their reference to `removedPost` means community removals; instance moderation uses `moderatedPost`. | New object instead of relabeling a community's decision as an instance decision. Source attribution distinguishes local/inherited restrictions without pretending to be a human actor. The blocked-view wording update is description-only and compatible. |
| `community/post/defs.json#postView` | Add optional `moderation` ref to `moderation.defs#moderationView`. Normal NSFW posts keep their verbatim record and ordinary view; active moderator `contentLabels` drive existing sensitivity presentation alongside record self-labels. | Additive optional property, no versioned endpoint required for this metadata. Do not rewrite `record.labels` or return a removal tombstone for a post that is only NSFW. Update client bindings and sensitivity checks before enabling the admin workflow. |
| `community/post/get.json` | Add `#moderatedPost` to the existing open `posts.items` union and serve it for instance removals (decided 2026-09-20), subject to the old-reader check below. | Schema-additive, so schema evolution alone does not require `post.getV2`; runtime client compatibility must still be demonstrated. Unknown variants must render unavailable rather than crash a batch or expose raw JSON. |
| `actor/getComments.json` | Keep its normal `commentView` response. Filter moderated comments, comments under a removed post, and any author-deleted placeholders from profile activity before hydration. | No schema change. Audit whether placeholders are currently returned here; profile tombstones have no use. |
| `actor/getPosts.json`, `feed/defs.json`, `feed/getCommunity.json`, `feed/getDiscover.json`, `feed/getTimeline.json`, `feed/getAll.json`, `feed/searchPosts.json` | Keep normal feed views and filter removed subjects; hydrate the new optional post metadata for NSFW without filtering/ranking changes. Test pagination, cached candidates, and hydration. | The referenced `postView` gains optional metadata; no feed-array type change is needed. Do not broaden all feed unions just because direct views need tombstones. |
| `embed/post.json#view.resolved` | Document/test the content-free unavailable result for a removed quoted subject; preserve its current `unknown` field type. Reuse a typed tombstone object when it fits the existing rendering contract, or define the concrete unavailable object before shipping. D-QUOTES (decided): Coves-quoted posts are not hydrated server-side. The served quote is the strongRef only, so it carries no copied content and no NSFW metadata; `post.get` on a removed quoted URI returns `#moderatedPost`. | Redact a removed quote's preview, not the containing post; this is not an embed-only moderation action. Do not narrow the published `unknown` to a union in place or turn the record's strongRef into a hydrated object. No removed record data may survive in cached `resolved` objects. |
| `community/post/getStatus.json` | Keep community admission status and its meaning. Audit serving/disclosure separately; do not overwrite `accepted` with an instance-removal status. | No moderation-driven schema change. `getSubjectState` and tombstone views describe the separate instance restriction. |

**Old-reader contract:** comments need no separate old-reader path, because removed comments use the wire shape installed clients already decode and a removed post's thread uses the already-declared `NotFound`. Before enabling actions, pin with tests that supported web and mobile builds render `deletionReason: moderator`, tolerate the optional `moderation` object, and handle thread `NotFound` without a crash loop. Every other still-served endpoint must redact using only a response its published schema allows.

For **every unversioned endpoint gaining a union variant**, test actual supported older mobile and web decoders, not just newly generated clients. For `post.get`, include normal posts plus an unknown variant in a 25-URI response and verify one removed post cannot fail the entire batch. Until supported client builds pass, leave `MODERATION_ADMINS` empty so no admin action can create a removal (`docs/LEXICON_PUBLISHING.md` release gate 5): `post.get` serves `#moderatedPost` for every instance removal and has no `#notFoundPost` fallback switch. Never substitute the community-only `#removedPost`. If incompatible readers must remain supported while new clients need source-attributed post tombstones, select and inventory a versioned post-read endpoint before rollout. Do not assume installed apps upgrade or infer decoder support from authentication. Never serve a normal `commentView` containing removed content.

### 14.5 Errors, privacy, and unknown values

Candidate endpoint errors are `AuthRequired`, `Forbidden`, `InvalidRequest`, `InvalidSubject`, `SubjectNotFound`, `DecisionNotFound`, `InvalidDecision`, `ContentChanged`, `StateConflict`, `IdempotencyConflict`, `UnsupportedReason`, `UnsupportedLabel`, `InvalidCursor`, and `ModerationUnavailable`. `InvalidDecision` covers a reversal targeting the wrong action kind; it must not mutate any state. Declare only those each endpoint can actually return, with precise retry/refetch guidance; reuse established transport-level rate-limit handling. Authorization and access checks precede sensitive subject/decision errors. A missing/restricted public subject must not produce different lookup/filter behavior that becomes an existence oracle.

Published public schemas must never declare `privateNote`, raw report details, credentials, or retained evidence as optional public fields. New public/admin result types stay distinct even when they share safe subobjects. Lexicon objects permit unknown extension fields, so the schema alone cannot enforce redaction: serializers must construct allowlisted tombstones/modlog projections, and tests must inspect the complete serialized object for leaked record extensions.

Clients tolerate unknown reason tokens, action values, and union variants with safe generic text. Servers validate recognized write operations and scope independently; “open union” is a schema-evolution mechanism, not authorization to act on arbitrary third-party types.

### 14.6 Reuse, deferred schemas, and publication gates

- **Reuse unchanged:** `com.atproto.repo.strongRef`, `com.atproto.label.defs#label`, `com.atproto.label.queryLabels`, and `com.atproto.label.subscribeLabels`. Do not fork these schemas into `social.coves.*` or alter upstream label values/fields to carry private actor/scope data. Implement the actual label endpoint's cursor/error rules, not a generic subscription example copied over them.
- **No initial record changes:** author post/comment records, community acceptance/removal, existing self-labels, and moderator appointments. No `moderation.action` repository record or custom moderation subscription is required for this service API. A labeler declaration for third-party ecosystem integration is conditional on the chosen publisher; it is not a new user-account modality declaration.
- **Deferred:** ban/tribunal/rule-proposal endpoints, community role mutations, user labeler preferences, log amendment APIs, evidence retention schemas, and any standalone admin-management API. They do not become publication dependencies merely because some drafts share the moderation directory.
- **OAuth/service routing:** inventory the exact new NSIDs in the existing authentication/proxy permission configuration, client bindings, route registration, and production Caddy allowlist. A permission-set lexicon is conditional on adopting granular OAuth permissions; neither a scope nor a client-supplied DID replaces instance-admin checks.

Before publishing implementation schemas:

1. Resolve section 12 choices that affect wire data and record a field-level required/optional and public/private review. Pin generated client handling for all tombstone/union variants.
2. Author JSON and fixtures for new definitions, requests, responses, errors, unknown values, and removed-content extensions. Validate required/non-null constraints, text byte/grapheme limits, safe pagination, and the distinction between strongRefs and URI-scoped decisions.
3. Run `go run ./cmd/validate-lexicon`, `goat lex lint internal/atproto/lexicon/social/coves`, and `goat lex breaking internal/atproto/lexicon/social/coves`; inspect `goat lex diff` against published schemas. The only new exception this work may add to `LEXICON_PUBLISHING.md` is `commentView.record` becoming optional (section 14.4); no ref-to-union change and no other nullability change is authorized. Run implementation tests and `make ci`; no public network in the hermetic gate.
4. Follow the runbook's separate live-record compatibility check with an explicitly selected real PDS before publication. Do not substitute a public identity/PDS dependency for the local development stack.
5. Update the runbook's moderation holdback policy for these specific reviewed schemas. Because the new content-view definitions reference `moderation.defs`, resolving its publication is a hard prerequisite for tombstones as well as the modlog. The existing `#banView` (a generic `uri`/`cid`/`record`/`indexedAt` record view referenced only by the held-back `banUser`, `listBans`, and `getBanStatus`) is published as it stands (2026-09-20); it cannot be removed afterwards. Then enable `_lexicon.moderation.coves.social` delegation, publish the reviewed shared definitions, and verify their full dependency closure resolves. Only then publish the additions to `community.comment.defs` and `community.post.defs`, and finally the endpoints that reference them. Use explicit file allowlists, **not the whole moderation directory**. Do not silently publish unreviewed governance contracts or remove an existing definition casually.
6. Resolve all published definitions through DNS/PDS and pass the older-decoder gate above before emitting new variants. The corrected `commentView` placeholder, removed-post thread `NotFound`, the selected old-reader behavior, and effective NSFW hydration/presentation must all be exercised against the running AppView before enabling actions. Ignoring optional moderator label metadata is schema-compatible but does not satisfy the supported client's NSFW behavior contract.

Inventory completion means each row has a selected wire contract and compatibility classification before coding; network publication remains a release step under `LEXICON_PUBLISHING.md`.
