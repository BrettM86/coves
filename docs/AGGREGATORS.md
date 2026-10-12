# Aggregators

An aggregator is an ordinary atProto actor with its own DID and PDS account. It posts through `social.coves.community.post.create` like any user. There is no separate posting API: the author comes from authentication, and `CreatePost` classifies the actor before admission. Aggregator posts are author-owned, so they live in the aggregator's own repo (see [POSTS.md](POSTS.md)).

Setup steps are in [scripts/aggregator-setup/README.md](../scripts/aggregator-setup/README.md). The first-party aggregators live in `aggregators/`.

## Records

`social.coves.aggregator.service` lives in the aggregator's repo with rkey `self`. The Jetstream consumer (`aggregator_consumer.go`) indexes it into the `aggregators` table. It rejects the event permanently when the rkey is not `self`, when `displayName` is missing, or when the record's `did` is not the repo DID.

`social.coves.aggregator.authorization` lives in the community's repo under any rkey. The consumer rejects it permanently when `communityDid` is not the repo DID, so a user repo cannot authorize on a community's behalf. When the aggregator or community row is missing, the event fails as `ErrUnresolvedReference` and goes to the redriver rather than being retried as transient.

Both record types are hard-deleted on a delete event. Both run under the rev gate, and the gate row is the tombstone that stops a stale create copy from resurrecting a deleted record.

Deleting the service record cascades. The `aggregators` row goes, and with it every `aggregator_authorizations` row, the `aggregator_posts` quota ledger, and the API key hash and OAuth credentials stored on the row. The key's `oauth_sessions` row (session ID `apikey`) is not deleted and stays until removed by hand. Jetstream never redelivers the communities' authorization records, so a delete followed by a re-create leaves the aggregator unauthorized everywhere. Change the declaration with `com.atproto.repo.putRecord`, never delete and re-create.

## Actor classes

| Class | Who | Checks | Quota |
|---|---|---|---|
| User | everyone else | visibility, ban, dedupe | per-author ledger limit (`SubmissionsConfig`) |
| Registered aggregator | any DID with an `aggregators` row | enabled authorization, dedupe | 10 posts per rolling hour per community, counted in `aggregator_posts` |
| Trusted aggregator | DIDs in `TRUSTED_AGGREGATOR_DIDS` | dedupe only | none |

`TRUSTED_AGGREGATOR_DIDS` is comma-separated. When it is empty the legacy single-DID `KAGI_AGGREGATOR_DID` is used instead. The write path and the acceptance engine both read it through `posts.TrustedAggregatorDIDs`, so they cannot disagree about who is trusted. The engine resolves it once at wiring, so a change needs an AppView restart.

Dedupe applies to every class. RSS re-polling is the case it exists for. The fingerprint is the canonical record plus the top-level `thumbnailUrl`, and an identical submission in the same dedupe bucket is refused with 409 `DuplicateSubmission`. Buckets are `POST_SUBMISSIONS_DEDUPE_WINDOW` long (default one hour) and aligned to the epoch, so protection lasts anywhere up to one window. Aggregators still need their own posted-item state. A missing authorization is 403 `NotAuthorized`. A spent aggregator quota is 429 `RateLimitExceeded`. A refusal consumes no quota.

If the `IsAggregator` lookup fails during `CreatePost`, the author is treated as a user for that request.

## Registration

`social.coves.aggregator.register` takes `{did, domain}` and needs no authentication. It is rate limited to 10 requests per 10 minutes per IP. The handler normalizes the domain to a bare hostname before building any URL, then fetches `https://<domain>/.well-known/atproto-did` through the SSRF-guarded client with a 4 KB body cap (see [SSRF_SECURITY.md](SSRF_SECURITY.md)). Under `IS_DEV_ENV` the guard allows private hosts so the local stack works.

The DID's resolved handle must equal the domain, compared case-insensitively. A mismatch is 403 `HandleMismatch`. An empty handle or `handle.invalid` is also `HandleMismatch`, because the one value that means "no handle" is a domain an attacker can own. An erased account is refused with `AccountErased`. On success the handler writes a `users` row and nothing else.

Registration gates nothing today. Every aggregator check keys off the `aggregators` row, which comes from the indexed service record, so an aggregator that skips `register` works the same. The published `register.json` error list also omits `HandleMismatch` and `AccountErased`.

## API keys

Aggregators authenticate with `Authorization: Bearer ckapi_<64 hex>`. The auth middleware tries the `ckapi_` prefix before sealed session tokens and service JWTs, and accepts API keys only from aggregators.

`createApiKey` requires an OAuth session (browser login as the aggregator account) and an `aggregators` row, so the service record must be indexed first. It returns the key once. Only the SHA-256 hash and the 12-character prefix are stored. Each aggregator has one key, and minting a new one invalidates the old one immediately. `getApiKey` and `revokeApiKey` complete the set.

The key is bound to a copy of the minting OAuth session: access token, refresh token and DPoP key, encrypted at rest (see [CREDENTIAL_ENCRYPTION.md](CREDENTIAL_ENCRYPTION.md)). The AppView uses that session to write the aggregator's posts to its PDS. A background job runs every 30 minutes and refreshes tokens that expire within the hour, and `APIKeyService` also refreshes on demand.

The browser session and the key share one refresh-token lineage. Logging out of the browser session that minted the key revokes those tokens at the PDS, and the key starts failing with 401 at its next refresh. Close the tab or clear the cookie instead of logging out. If it happens, log in again and mint a new key.

## Thumbnails

A trusted aggregator may send a top-level `thumbnailUrl` on `post.create`. It is not a lexicon field. The AppView downloads the image and uploads it as a blob to the author's repo. A failed upload leaves the post without a thumb. When a trusted aggregator sends no `thumbnailUrl`, the link is unfurled for a thumbnail only and the aggregator's own title and description stand.

Every other actor, registered aggregators included, has `thumbnailUrl` ignored and gets the full unfurl that users get.

## Authorization in practice

The first-party aggregators (Kagi News, Reddit Highlights, Bluesky Topics) run as trusted DIDs and map feeds to communities in their own `config.yaml`. They never read authorization records, and no code reads `authorization.config`. The `configSchema` in a service record is validated with JSON Schema only in the enable and updateConfig paths, which are not built.

`social.coves.aggregator.enable`, `disable` and `updateConfig` have published lexicons but no routes, and their service methods return `ErrNotImplemented`. Nothing in the product writes an authorization record. Authorizing a third-party registered aggregator today means writing the record into the community's repo directly with the community's own credentials.

## Operations

The first-party aggregators run cron inside their containers. Cron does not inherit the container environment, so each `docker-entrypoint.sh` writes a whitelist of variables to `/etc/environment`, and the crontab sources it. A variable missing from that whitelist is present under `docker exec` and absent in the cron run. Prove cron authentication from a log line of a real cron run, not a manual `docker exec`.

`/var/log/cron.log` is not on a volume. Recreating a container erases it along with the old container's Docker logs, so capture both first.
