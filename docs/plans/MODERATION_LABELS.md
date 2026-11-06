# Moderation labels

Plan for federated moderation: another AppView can choose to follow Coves.social's removals and NSFW labels, and Coves can follow theirs. Delete this file once the federated milestone ships.

The first milestone is the publisher with Tidepool as its first consumer, so Coves admin removals reach Tidepool and Lemmy. The Coves side of it is built (§ Built today); the community-record work is described in [MODERATION.md](../MODERATION.md). Ingesting labels from outside sources comes after it.

## Built today

Local moderation is built and described in [MODERATION.md](../MODERATION.md): instance admins remove and restore posts and comments and label posts NSFW, with a public modlog and an admin log.

The publisher is built:

- Config: `LABEL_SIGNING_KEY` (private multibase, K-256 or P-256) enables publication; unset, nothing is published, and boot fails with `ErrSigningKeyRequired` once a label is stored or the one-time export has completed, so a decision is never made while publication is off. `LABEL_SIGNING_KEY_RETIRING` acknowledges a rotation. `INSTANCE_ATPROTO_PUBLIC_KEY` is the `#atproto` key the DID document lists; it is required with `LABEL_SIGNING_KEY` and must differ from the label key. `DID_WEB_HOSTS` (dev and CI only) maps a did:web host to a local `host:port` so the hermetic stack resolves `INSTANCE_DID`. Malformed values fail boot.
- DID document: the AppView serves `GET /.well-known/did.json` built from that config, with `#atproto_label` and an `#atproto_labeler` service pointing at `APPVIEW_PUBLIC_URL`. That document lists no `#atproto_pds` service, so it does not replace production's. Production keeps serving the static `did.json` built from `static/.well-known/did.json.template`, which also lists the instance DID's `#atproto_pds`, and the operator updates it by hand in the deploy: add `#atproto_label` with the label key's public multibase when publication is enabled, and change it in the same deploy as any key rotation. Until then, external verifiers cannot verify the labels.
- Issuance: removal, restore, NSFW label and NSFW retraction each write one signed, URI-scoped label row (`!takedown`, `!takedown` neg, `nsfw`, `nsfw` neg) in the decision's transaction (migration 071, table `labels`). Each insert takes a transaction-scoped advisory lock held to commit, so commit order equals `seq` order and a reader never later finds a committed row below a `seq` it has seen; `cts` strictly increases. Replays write nothing; restricted subjects (indexed row gone) are not published; doxing and illegal-content removals are published.
- One-time export: the first boot with a key publishes one current label for each active instance removal and NSFW label whose subject is still indexed, with a new `cts`, under the same lock. `label_publication_state` records completion in the same transaction, so later boots export nothing.
- `com.atproto.label.queryLabels` is served publicly (`uriPatterns` with trailing `*` prefix, `sources`, `limit` 1 to 250, `cursor` = last `seq`).
- `com.atproto.label.subscribeLabels` is served publicly when publication is enabled. Each `#labels` frame carries one stored row, its `seq` the row's `seq`, with the stored signature as `queryLabels` serves it. No cursor starts at the live tail: rows committed after the connection opens. A cursor replays every row after it in `seq` order, then tails live with no gap or duplicate. A cursor ahead of the latest `seq` gets a `FutureCursor` error frame and the connection closes. Live delivery is one shared poller with a bounded in-memory window; a subscriber that falls behind reads from the table again, so it stalls only itself and never a decision. Every frame write has a 10-second deadline, the peer is pinged every 30 seconds and dropped 60 seconds after its last pong, and at most 256 subscriptions are open at once; past that a new one gets 503 `Unavailable`. That cap is global across all clients; a per-IP limit belongs in Caddy. Shutdown closes every open subscription, so a subscriber reconnecting after a rotation restart receives later rows once, verifying under the new key.
- Key-change policy and rotation: see § Wire contract. A rotation deploy also updates `#atproto_label` in the production `did.json` to the new key.

Nothing consumes labels yet. The schema already reserves what this plan needs: `moderation_actions.origin` accepts `inherited`, the action vocabulary includes `apply-removal` and `retract-removal`, and every decision carries an authority DID and a scope.

## Model

Labels distribute decisions; they do not delete content, grant moderation permissions, or provide an audit log. Three responsibilities stay separate:

- Decision and history: who acted, under whose authority, on which content, why, and which earlier action it reversed. This is the existing action log.
- Signed signal: an atProto label naming the issuing authority, the subject and the removal or NSFW value, with independent retractions.
- Enforcement: each AppView decides which authorities it trusts and redacts accordingly.

A label proves that an authority issued a signal. It does not prove which human clicked Remove. Local actor attribution comes from the authenticated audit record; public actor attribution is the authority's assertion.

A standard label cannot carry the full action or a community scope. Do not add unsigned fields or pack DIDs, reasons or structured data into `val`. Reasons stay in the action log and are not separate enforcement labels.

## Wire contract

- The labeler is the instance DID (`INSTANCE_DID`, a `did:web`) with a separate `#atproto_label` verification key, distinct from its `#atproto` key, and an `#atproto_labeler` service endpoint. No dedicated labeler account. The hermetic stack must serve a resolvable DID document for it through the configured local resolver.
- Removal is a URI-scoped label: `cid` omitted, so editing the record cannot evade it. The reviewed CID stays in the action history.
- The removal value is `!takedown`, for compatibility with the hard-redaction signal in the labeler-header spec. Enforcement comes from the receiver's mandatory trust policy on every read, not from a client header. The label does not claim the author's PDS deleted anything. `!hide` and custom hide definitions are not equivalent.
- Account takedown is `!takedown` on the account DID (the bare `did:…`, not `at://<did>`), with `neg: true` on unban. A temporary ban's label carries `exp` equal to the ban's `expiresAt` (signed, millisecond precision); the negation has no `exp`. No label is written when a temporary ban lapses: `exp` already ends it. A replacing ban writes a new `!takedown` whose later `cts` supersedes the earlier one.
- NSFW is `val: nsfw`, also URI-scoped, published with a definition as a revealable content warning. The `!takedown` and `nsfw` lifecycles are independent.
- Restore emits the same `src`, `uri` and `val` with `neg: true`. A negation retracts that source's assertion; it is not a global "safe" verdict.
- Services exchange full signed objects (`ver` and `sig` required), verify against the source DID's label key, and keep provenance. Keys resolve through the configured local or production identity infrastructure, never a hardcoded public host.
- The endpoints are `com.atproto.label.queryLabels` and `com.atproto.label.subscribeLabels`, reused unchanged, with defined cursor retention, recovery and key-rotation behavior. Do not fork the label schemas into `social.coves.*`. A Bluesky labeler declaration record does not make Bluesky's AppView understand Coves collections.
- Key rotation: every stored label records the public multibase that signed it, and `label_signing_keys` records each key's start and retirement. At boot, before the HTTP listener and the export, the AppView compares the configured key with the signers of the stored labels. To rotate, the operator sets `LABEL_SIGNING_KEY` to the new key and `LABEL_SIGNING_KEY_RETIRING` to the old key's public multibase, then restarts. In the same deploy the operator updates `#atproto_label` in the production static `did.json` to the new key's public multibase; Caddy serves that file, not the AppView's document, so until it is updated every label is signed by a key the published document does not list, external verification fails, and Coves logs nothing. Boot re-signs every label not signed by the new key in one transaction holding the label allocation lock, and records the old key as retired. `seq` and `cts` never change, and no label is re-emitted or given a new `seq`. A failure rolls the whole rotation back and fails boot; the next boot retries. Without the acknowledgement, or with one naming a key that did not sign the stored labels, boot refuses with `ErrSigningKeyMismatch` and changes nothing. A subscriber mid-replay is disconnected by the restart and resumes from its cursor, receiving later labels once, signed by the new key. Labels it received earlier carry the old signature; to re-verify them it resubscribes from cursor 0 or calls `queryLabels`.
- Labels are protocol objects, not repo records, so the Jetstream wanted-collection subscription does not deliver them. Ingestion needs its own consumer.

## Publishing

Labels for public subjects are published by default (decided 2026-09-20). Restricted subjects are excluded until a sharing policy exists; publishing a label reveals its subject, so modlog filtering alone is not enough.

Local enforcement must never wait on an outbound call. Publication intent is written in the same transaction as the decision, delivery is durable and retried without duplicating actions, and admins can see pending or failed publication (`#publicationView`, not yet in `moderation.defs`). Local-only decisions never show as pending.

Serialize application, retraction and reapplication per source, subject and value, for removal and NSFW separately. Locally issued `cts` values must strictly increase, and a retry must not give an old decision a new timestamp that overtakes a later retraction.

When federation is first enabled, export the currently active, sharing-eligible decisions once with new issuance times, then hand off to live publication without a gap. Do not replay historical remove/restore pairs, and do not lose a restore that lands between the export and live publication.

## Trust rules

- Subscribe by DID, not endpoint hostname. Discovering an endpoint or key grants nothing.
- A trust policy names accepted values, supported subject collections and optional community restrictions. Accepting `nsfw` enables the warning treatment only; accepting `!takedown` enables removal. Unrecognized values grant nothing. NSFW is posts only.
- Trust configuration is operator-managed (decided 2026-09-20). Changes are attributed in audit history and recompute affected state. There is no per-user opt-out of mandatory removals and no per-subject "ignore trusted removal" override; an operator can stop trusting a source.
- Under a community-restricted subscription, an unknown or forged subject-to-community relationship cannot authorize removal. Under instance-wide policy, a valid trusted exact-URI label can be held before the subject is hydrated.
- Any active applicable removal blocks ordinary visibility. Neither a local restore nor a remote negation overrides another source's restriction. NSFW from several sources combines without becoming a removal. Retracting `nsfw` cannot negate `!takedown`, and the reverse holds.
- Never reissue inherited labels under the local identity. That creates trust loops and turns one source into apparent consensus. Subscriptions are not transitive.
- Mandatory policy is applied independently of label hydration a user selects. The `atproto-accept-labelers` header never widens or narrows mandatory removals; an empty optional preference cannot disable server policy.

## Ingestion and ordering

This section applies when Coves starts trusting outside label sources, which is deferred (2026-10-19). Tidepool, as a consumer of Coves' labels, follows the same verification and deduplication rules. For the first milestone (one source, one consumer), ordering and retention are settled: the publisher keeps every label row, so a cursor never expires; a consumer whose cursor is ahead of the latest `seq` (`FutureCursor`, meaning the publisher was reset) resubscribes from the start; `queryLabels` carries no sequence and is not used for recovery; labels apply in `seq` order, so `cts` never orders them; and a label for a subject the consumer has not seen yet is kept and applies when the subject appears.

Labels for subjects not yet indexed go into a pending store with per-source record and byte limits; a label may arrive before its subject. Persisted decisions apply before the content's first eligible read once its scope is known. When pending capacity runs out, pause that source without advancing its cursor past unpersisted events and raise a health failure. Do not evict decisions or report the source as caught up. For a stalled source trusted to remove content, reconcile newly indexed subjects' labels or hold them unavailable before serving. A classification-only source cannot withhold posts through an outage: keep known NSFW state, report staleness, reconcile.

Deduplicate incoming events and keep negation state, so a replayed older positive label cannot resurrect a removal. Within one source, subject and value, the label's `cts` decides currency, consistently across query and stream. Stream sequence controls delivery and checkpoints, not currency: a later sequence carrying an older `cts` does not replace newer state. Sources need no ordering relative to each other.

Bootstrap by backfill plus stream handoff with no gap. An expired cursor triggers reconciliation, never a silent reset. During a labeler outage, keep known active decisions until a verified retraction, an applicable expiry or a trust change, and surface staleness. Verify signatures on ingest, refresh DID documents for key rotation, reject invalid labels, and never resolve a remote labeler synchronously on a content read.

## Inherited modlog entries

Inherited changes appear as "Applied removal from [authority]" or "Removal retracted by [authority]", with origin `inherited`, attributed to the source and the local trust policy, never to a fabricated local moderator. Inherited NSFW entries name the source and value and change presentation only.

A label carries no remote actor, explanation, community scope or action ID. The receiving log works from source, subject, value, timestamps and ingestion metadata. `createdAt` is the receiver's server time; the source `cts` goes in `issuedAt` and is never a sort key a remote clock controls. Links to a source's public history are optional, and their absence never blocks enforcement.

## Cross-instance example

Community X is hosted on instance Y. Comment C in X is indexed by both Y and Coves.social. A Coves admin removes C: Coves redacts it, logs the action and publishes a signed URI-scoped `!takedown`. Y's operator has configured Coves' DID as a trusted removal source for all Coves content or a subset of communities. Y verifies the label, establishes C's community from validated indexed content, applies its policy, shows "[Removed by server admin]" sourced to Coves.social, and records an inherited event. Y does not delete C from its author's PDS or write a removal record for X. When Coves restores C and publishes the negation, Y clears its inherited restriction unless another applies. An instance that never trusted Coves is unaffected.

## Community scope

Decided 2026-10-19: community decisions are records in the community's repo, not labels, and there are no per-community labelers. Every community already has a repo whose keys the Coves PDS or Tidepool holds, its records are signed by the community's identity, and the firehose Coves consumes already carries them. Records are authoritative and the `moderation_*` tables index them; labels never project community decisions. Lemmy moderators' removals and bans arrive this way through Tidepool, and Coves community moderators will write the same records. The indexed records are described in [MODERATION.md](../MODERATION.md) § Community comment removals and [POSTS.md](../POSTS.md) § 4.1 (community bans).

## Decisions made

- Labeling identity is the instance DID with a separate `#atproto_label` key (2026-09-20).
- Public-subject labels are published by default; restricted subjects are excluded (2026-09-20).
- Trust is operator-managed, attributed, with no per-user opt-out of removals (2026-09-20).
- Removal and NSFW are separate label values with independent lifecycles; reasons are not labels.
- Coves admin moderation is the one instance moderation service for Coves and Tidepool. Tidepool consumes Coves' labels and publishes none (2026-10-19).
- Community decisions are records in the community repo, never labels (2026-10-19).
- Tidepool pushes Coves removals of native content to Lemmy as `Delete` of the `Page` or `Note` from the author's persona, and restores as `Undo{Delete}` (2026-10-19).
- Ingesting outside label sources is deferred until another AppView exists (2026-10-19).
- Build the publisher in Go on indigo's `atproto/labeling` package, which the pinned indigo module already ships (`Label.Sign`, `VerifySignature`, lexicon conversion) rather than deploy Ozone. Ozone would add a Node service with its own database and a second action store, and it does not consume labels for a third-party AppView, so ingestion must be built either way. What remains for publication is a sequenced label table, `queryLabels` and the `subscribeLabels` stream. Confirm this before starting.

## Acceptance

The first milestone is met when Tidepool, consuming the Coves publisher, deletes and restores native content on a real Lemmy. The full federation requirement is met only when two separately configured AppViews in the hermetic stack demonstrate it. No mocked federation E2E and no infrastructure skips.

- Y redacts Coves' removal of a real comment only after explicitly trusting the source. An untrusted labeler, wrong key, unsupported collection or value, forged community relationship or out-of-scope subscription cannot impose removal.
- Restore and negation propagate and old replays cannot undo them. Two trusted sources remove one subject independently; retracting one leaves the other.
- Labels before subjects, pending-capacity exhaustion, restart, duplicate and out-of-order delivery, and expired cursors recover without losing removals or retractions.
- Y's modlog distinguishes inherited from local actions, and Y does not republish Coves' decisions.
- Rebuilding indexes preserves enforcement. Stopping trust is audited and recomputes only that policy's effects.
- A publication failure leaves local removal in force, its retry durable and its status visible. The first-enable export happens once logically, with a concurrent restore or re-remove handled without a gap.
- A source trusted only for `nsfw` cannot remove, and its outage does not drop posts from feeds.

Enable publication only once retraction, identity keys, durable history and delivery monitoring work. A rollback must keep existing enforcement and audit data.

## Open questions

For the first milestone, § Ingestion and ordering settles ordering, cursor retention and labels that arrive before their subjects. The items below remain open for outside sources.

- Whether `!takedown` works end to end in the chosen publisher and consumer; older tooling and the core lexicon's known values may lag the spec.
- Deterministic handling of equal-`cts` conflicts and implausibly future-dated labels. Quarantine ambiguous input rather than let arrival order decide.
- Pending-label limits, cursor retention, and the expired-cursor recovery procedure.
- A sharing policy for restricted subjects.
- Proposed operational targets: a healthy peer applies a published label within 30 seconds at p95, and private dashboards show publication backlog, oldest pending age, consumer lag, rejected labels and reconciliation failures. Validate under load before promising them.
