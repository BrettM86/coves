# Communities

## Accounts

Each community is its own atProto account: its own `did:plc`, its own repo, and a PDS account that this instance provisions and holds credentials for. The community repo holds the profile and the records the community issues, such as post acceptances; posts themselves live in their authors' repos (see [POSTS.md](POSTS.md)).

The profile is `at://<community DID>/social.coves.community.profile/self`. The lexicon declares its key as `literal:self`, and the community consumer rejects any other rkey on create and update. Updates to the profile are written with the community's own credentials, never the instance's.

## Handles

A community provisioned here gets the handle `c-<name>.<instance domain>`, for example `c-gardening.coves.social`. The `c-` prefix keeps community actors apart from user actors on the same PDS while staying a single-level subdomain. A community name may not start with `c-`: clients strip one leading `c-` to get the handle they display, so `c-sharp` would be shown as the handle of a community named `sharp`. Bridged communities keep their source handle with no prefix.

Clients display and link the prefix-free handle, so `LookupByHandle` retries a bare handle with `c-` when the first lookup misses. Errors other than not-found are returned without a retry, so a database outage never looks like a missing community.

`!name@instance` is a display form that clients build from `name` and the instance. No record stores it, but the API accepts it as input. `ResolveCommunityIdentifier` takes a DID, a handle (with or without `c-` or a leading `@`), `name@origin` with or without `!`, or a bare name on this instance. A local origin rebuilds the `c-` handle. A remote origin resolves against the `(name, origin)` pair the consumer stored at ingest, which lets a bridged community be addressed by the instance it came from rather than by its DNS handle. Which handles a community DID may claim, and the `hostedBy` did:web check, are in [SECURITY_MODEL.md](SECURITY_MODEL.md).

## Credentials

The community's PDS password, access token and refresh token are sealed with AES-256-GCM before they reach Postgres ([CREDENTIAL_ENCRYPTION.md](CREDENTIAL_ENCRYPTION.md)). The password is encrypted rather than hashed because the session fallback below sends it in clear. Credential fields carry `json:"-"`.

`EnsureFreshToken` runs before every write to a community repo. It locks a mutex for that community DID, re-reads the row in case another goroutine already refreshed, and refreshes when the access token expires within 5 minutes. If the refresh fails with an expired-token error, it falls back to `createSession` with the stored email and password; any other refresh error is returned. Refresh tokens are single-use, so the new pair is written at once with three attempts. If all three fail, the call errors and the stored refresh token is already spent. The mutex lives in one process and does not serialize refreshes across several AppView instances.

Raw PDS calls made by `callPDSWithAuth` time out after 10 seconds for reads and 30 seconds for `createAccount`, `createRecord` and `putRecord`. Writes got the longer limit because community creation failed at 10 seconds on slow PDS writes (keypair generation and PLC registration). Indigo XRPC calls in the package, including token refresh, run under a 30-second ceiling that covers their retries.

## Hosted communities

A community is hosted by this instance when `pds_refresh_token_encrypted IS NOT NULL`, meaning the AppView holds credentials that can sign for it. `hosted_by_did` is never used for this, because it comes from a record anyone can write. The query is `NewHostedCommunityQuery` in `internal/db/postgres/community_hosted.go`, and the Discover hosted bonus uses the same predicate.

## Subscriptions

A subscription is a `social.coves.community.subscription` record in the user's repo whose `subject` is the community DID. The consumer indexes creates and deletes idempotently and ignores updates. `contentVisibility` runs from 1 (only the best content) to 5 (all content), is clamped to that range, and defaults to 3; it is stored, but no feed reads it yet.

`subscriberCount` comes only from indexed subscription rows. A trigger added in migration 045 maintains `communities.subscriber_count`, and the profile record cannot set it. Go code must not adjust the count itself: a binary that still did would double-count against the trigger, which is why the migration header warns against overlapping old and new binaries in a rolling deploy.

## Blocks

A `social.coves.community.block` record in the user's repo hides that community from the viewer's aggregate feeds only. The exact surfaces are in [FEEDS.md](FEEDS.md).

## Creation

`COMMUNITY_CREATORS`, a comma-separated list of DIDs, restricts `community.create` to those accounts; anyone else gets 403 `CommunityCreationRestricted`. When it is empty any signed-in user can create a community. The handler rejects a client-supplied `createdByDid` or `hostedByDid`; the service derives both from the session and the instance configuration.

## Community moderators

Not built. Only instance admins moderate today (see [MODERATION.md](MODERATION.md)). The held-back, unpublished `social.coves.community.moderator` lexicon puts each moderator record in the community's repo, not the moderator's. The community PDS can then revoke it at once by deleting it, and a malicious instance cannot forge or keep moderator status after revocation. The accepted trade-off is that moderators are appointed rather than invited, and "what do I moderate?" needs an AppView index.
