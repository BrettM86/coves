# Bridged identity: handle refresh and origin fields

Tidepool, the ActivityPub bridge, renamed every bridged actor from `<user>.<instance-with-dashes>.tdpl.io` to `<user>.<instance host>.ap.tdpl.io` (for example `alice.lemmy.world.ap.tdpl.io`). The DID stays the same; the handle changes in PLC and Tidepool announces it with a firehose `#identity` event. The old scheme needed one DNS delegation per instance, could not be turned back into the fediverse address (`my-lemmy.org` and `my.lemmy.org` collided), and lost case and underscores. The design document lives in the Tidepool repository and is not public.

Coves needed five changes for this, and this doc covers them:

1. Store a fresh, verified handle on every identity event, for users and communities.
2. Serialize handle refreshes per DID, so that two feeds or the redriver cannot revert a handle.
3. A one-off sweep, `cmd/reconcile-handles`, to fix rows that missed their identity event.
4. Origin fields (`origin`, `originUsername`, `originUrl`, `originSoftware`) on bridged profiles, admitted only from a trusted bridge, and served on profile views, author views and notifications.
5. Resolving a bridged user by `name@host`.

Bridged-account detection is unchanged: it uses `TRUSTED_BRIDGE_PDS_HOSTS` against the PDS, never the handle. The `hostedBy` registrable domain is still `tdpl.io`, so community `hostedBy` checks pass after the rename.

## Identity events

`#identity` events are no longer trusted as delivered. The user consumer (`internal/atproto/jetstream/user_consumer.go`) and the community consumer (`community_consumer.go`) both:

- Look up the DID in `users` or `communities` first. An unknown DID costs one indexed read and no resolver call. Jetstream delivers identity events for the whole network, so this ordering matters. A known DID is refreshed on every identity event, including events with no handle.
- Ignore the event's handle and call `RefreshIdentityHandle` (`identity_refresh.go`). In order, it purges the DID from the caching resolver, checks the timeout debounce, resolves the DID through the uncached resolver (`identity.NewUncachedResolver`) under the 5-second `identityResolveTimeout`, runs `identity.VerifiedHandle`, re-reads the stored handle inside the per-DID lock, purges the old handle's cache row, and writes the new handle. The old handle is purged before the write, so a failed purge is retried on redrive instead of being lost.
- Return `ErrUnresolvedReference` for resolution or purge failures, so the event is redriven.

The community path also validates the new handle (`communities.ValidateHandle`, permanent on failure) and re-runs `verifyHostedByClaim` against the stored `hosted_by_did`. A mismatch is `ErrPermanentEvent` and keeps the old handle. A fetch failure is `ErrUnresolvedReference`. A unique-handle conflict (`ErrHandleTaken`) is `ErrUnresolvedReference`. Users also get their OAuth session handles updated.

Timeout debounce: a refresh whose resolve hit the deadline records the DID for `IDENTITY_NEGATIVE_CACHE_TTL` (4096 entries at most). Within that window the next identity event still purges the cached DID but skips resolution and defers without spending a redrive attempt. Fast failures are not recorded. This is the one accepted exception to "every identity event triggers a fresh resolution"; `docs/SECURITY_MODEL.md` has the reasoning.

## Per-DID refresh lock

`NewPostgresIdentityRefreshLock` (`identity_refresh_lock.go`) takes a session-scoped `pg_advisory_lock` on a dedicated connection. The key is the first 8 bytes of `sha256("coves/identity-refresh/" + did)`, so it works across every feed's consumer, the redriver and the separate sweep process.

- Pool: `openIdentityRefreshLockDatabase` in `cmd/server/database.go`, capped at 8 connections and separate from the application pool. Lock waiters must never share a capped pool with the writes that release them, or the pool deadlocks. The sweep opens its own lock pool sized to `-concurrency`.
- Timeouts: each acquire sets `lock_timeout` to `IdentityRefreshLockWaitTimeout` (25 seconds: the resolve timeout, plus the 15-second well-known verification timeout, plus 5 seconds) and `statement_timeout` to 5 seconds more, so contention always surfaces as SQLSTATE 55P03. A same-DID contender can stall its serial lane for up to 25 seconds.
- A busy lock returns `jetstream.ErrIdentityRefreshBusy`, wrapped in `ErrUnresolvedReference` as a deferred redrive (no in-line retry, no redrive attempt spent). The sweep reports it as `lock-busy`.
- A nil lock (nil database or a wait of zero or less) makes `RefreshIdentityHandle` fail closed before any purge. The callback runs synchronously and is not re-entrant.

## Reconciliation sweep

`cmd/reconcile-handles` selects every user and community row whose `pds_url` is a trusted bridge host (`-trusted-hosts`, replacing `TRUSTED_BRIDGE_PDS_HOSTS`), resolves each DID fresh, and writes changes through the same locked refresh as the consumers, using the exported entry points `(*UserEventConsumer).RefreshUserHandle` and `(*CommunityEventConsumer).RefreshCommunityHandle`. `-dry-run` resolves and verifies only. `-clear-identity-cache` empties `identity_cache` first and cannot be combined with `-dry-run`. Exit status 0 means every bridged row matches its verified handle. The runbook, with the production invocation, reason codes and re-run rules, is "Bridged handle reconciliation" in `docs/PRODUCTION_DEPLOY.md`.

## Origin fields on profiles

Columns: `users.origin`, `origin_username`, `origin_url`, `origin_software` (migration 057) and `communities.origin_url`, `origin_software` (migration 058; community `origin` already existed). A cleared column holds `''` and an untouched one is `NULL`, so filter with `COALESCE(column, '') <> ''`, never `IS NOT NULL`.

Admission (`resolveFreshUserProfileIdentity`, `resolveFreshCommunityIdentity`) requires all of the following for the event being processed. A stored `pds_url` or an `identity_cache` row never admits the fields, because a cache row lives 24 hours and can still name the bridge after a repo moves.

1. The DID document, read uncached, names a PDS in `TRUSTED_BRIDGE_PDS_HOSTS`.
2. The declared handle's registrable domain (eTLD+1) equals the trusted PDS host's registrable domain. For Tidepool that means handles under `tdpl.io`. This rule was decided on 2026-10-12. Without it, an account could point its PLC PDS at the bridge host during consumer lag and be admitted.
3. A fresh uncached resolution verifies the handle both ways, names the same DID, and still names a trusted PDS whose registrable domain matches the verified handle's.

If admission fails, the fields are not admitted: a known user or community has its origin fields cleared and the rest of the edit applied, and an unknown user's profile event is dropped without creating a row. This is never redriven. A transient resolution failure or an unverified handle on a record that asserts the fields returns `ErrUnresolvedReference` without writing, so Tidepool's one-time profile rewrite survives by redrive. Community admission logs one line per event when an asserted field is not stored or a stored one is cleared (DID and reason only).

Sanitizing (`user_profile_origin.go`): `origin` is normalized as a domain and kept only together with a non-empty `originUsername` of at most 256 bytes. `originUrl` must be `https`, have a host equal to `origin`, pass atproto `uri` syntax and carry no userinfo. `originSoftware` is trimmed and lowercased, then must be 1 to 64 bytes. All lexicon `maxLength` limits count UTF-8 bytes, because indigo compares `len(value)`.

## Origin fields on views

- `actor.defs#profileViewDetailed` (`getProfile`) and `actor.defs#profileView` (notification reply authors and `recentUpvoters`) carry all four fields.
- `community.defs#communityView` and `#communityViewDetailed` (`community.get`, `list`, `search`) carry `originUrl` and `originSoftware` next to the existing `origin`.
- `community.post.defs#authorView` carries `origin`, `originUsername` and `originSoftware` on posts and comments. Posts have no software field of their own; the author's profile supplies it. Every post-view query selects these three columns through `postViewSelectColumns`; a new post-view query must do the same or the scan misaligns. Deleted-comment placeholders carry only the DID and `handle.invalid`.

No field marks an account as bridged; a client can only tell from the presence of `origin`. This was left out on purpose.

## Resolving `name@host`

`users.Service.ResolveActorIdentifierToDID` accepts a DID, a handle, `name@host` or `@name@host`, splitting at the last `@` and keeping the name's case. A name over 256 bytes, with invalid UTF-8 or with control characters is a 400 before any query. The repository lookup `GetDIDByOriginUsername` tries an exact match on (`origin_username`, `origin`) first, then a unique case-insensitive match. Two case-insensitive matches return `users.ErrAmbiguousUser`, which `xrpc/mapper.go` maps to 409 `AmbiguousActor`, mirroring `AmbiguousCommunity`. This way, shared links (exact addresses) always resolve, typed or lowercased addresses resolve in the common case, and no account becomes unreachable. Migration 059 adds non-unique exact and `lower()` indexes; the pair is copied from the bridge and is not unique. `actor.getProfile`, `getPosts` and `getComments` share `resolveActorDID`; an outage is a logged 500, never a 404.

There is no fallback that builds `<label>.<host>.ap.tdpl.io` from the address. Until Tidepool writes the origin fields, `name@host` returns 404 for every bridged user.

## Lexicon changes

All schema changes are additive and optional, except one. The `actor` parameter on `actor.getProfile`, `getPosts` and `getComments` dropped `format: at-identifier` for a plain string with `maxLength: 2048`. This is an input-loosening exception recorded under "Accepted evolution-rule exceptions" in `docs/LEXICON_PUBLISHING.md`. No lexicon file was added. None of these changes is published yet; publishing follows `docs/LEXICON_PUBLISHING.md`.

## Rollout with Tidepool

- The identity-event refresh and the per-DID lock must be deployed before Tidepool renames anything.
- After Tidepool's rename job finishes, run the sweep with `-clear-identity-cache`, then re-run it until it exits 0. That exit is the Coves half of the gate before the old handles' TLS and DNS are removed.
- Coves must serve the origin lexicon fields before Tidepool writes them. Tidepool re-syncs its vendored lexicons from Coves main (`scripts/sync-lexicons.sh` in Tidepool) after this work merges, and then rewrites every bridged profile once.
- Tidepool must emit bridged handles under the bridge PDS's registrable domain, an `originUrl` that passes the rules above, and byte-counted `originUsername` and `originSoftware` values, or Coves drops the fields.

## Known open items

- Origin fields are cleared only by a profile event. An identity event that moves a DID off the bridge, or removing a host from `TRUSTED_BRIDGE_PDS_HOSTS`, leaves stored values in place, and they keep showing on views and notifications until the next profile event.
- `originUsername` is stored as written apart from the length and empty checks. Bidirectional-override, zero-width and other control characters are not stripped, which allows display spoofing.
- `AmbiguousActor` is not declared in any lexicon `errors` list, and `getPosts` and `getComments` declare `NotFound` but return `ActorNotFound` (pre-existing).
- Users' OAuth session handles are updated after the per-DID lock is released, outside the serialized section. A row deleted between the first lookup and the in-lock re-read is treated as transient and redriven. The new handle's own cache row is not purged on refresh.
- Sweep: a cached `hostedBy` fetch failure reports `hostedby-pending` for the rest of the run, so re-run the sweep. A community row that carries sealed PDS credentials reports `error`, because the tool decrypts with a throwaway key.
- Discovering an unknown user whose DID is an untrusted `did:web` can stall the serial user lane for up to 5 seconds.
- Not built here: Caddyfile cleanup of the per-instance handle certificates (a production step after the gate), frontend and mobile display of `@name@host`, the community handle fallbacks in those clients, and community `name` truncation at 63 characters.
