# Production deploy

The production host runs several Compose projects. This repo's `docker-compose.prod.yml` in `/opt/coves` runs Postgres, the AppView, the PDS and Caddy. Each aggregator under `aggregators/` is its own project from the same checkout. The SvelteKit frontend runs as its own project from `/opt/coves-frontend`, and Tidepool from its own checkout. The `Caddyfile` at this repo's root is the production edge config for every hostname.

## Topology

The frontend and Tidepool join `coves-prod-network`, which this repo declares and their compose files mark `external`. Caddy addresses the frontend by container name, `coves-prod-frontend:3000`, never by the `frontend` service alias: aliases are not unique across Compose projects on one network. The frontend's server side reaches the AppView at `http://appview:8080` over the same network.

The frontend container publishes no port. Its `ADDRESS_HEADER=x-real-ip` makes adapter-node trust `X-Real-IP` from any peer, and Caddy overwrites that header on every frontend route (`header_up X-Real-IP {remote_host}`), so a client-supplied value never reaches it. The frontend stamps that address on its `/api/proxy` and `/api/me` calls to the AppView, so the AppView's rate limits key on the real user. The trust boundary is membership of `coves-prod-network`: any container on it (appview, pds, postgres, tidepool) could dial `:3000` and assert any address. That is accepted, not enforced. Never publish the frontend port and never join an untrusted container to that network.

## coves.social routing

The `coves.social` site block is an explicit allowlist for the AppView plus a frontend catch-all. The `@appview` matcher names every Go route meant to be reachable there: `/xrpc/*`, `/oauth/*`, the OAuth metadata and key files, `/api/me`, Go-served static and policy pages, and `/health*`. Everything else goes to the frontend.

A new non-XRPC Go route must be added to `@appview`, or the frontend answers it with its 404 page. `internal/api/routes/caddy_allowlist_test.go` (T0, runs in `make test`) walks the real router against the matcher in both directions and fails on drift. Keep `@appview` on one `path` line, because the test parses the block literally, and keep its paths disjoint from every other `handle` in the block: Caddy gives a multi-path named matcher its precedence from position only.

`/api/me` belongs to the AppView. Every other `/api/*` (`/api/auth/*`, `/api/proxy/*`) belongs to the frontend. A future Go `/api/*` route must join `@appview`, and a frontend `/api/me` would be shadowed.

Other rules in the block:

- The apex `/` splits on `Accept`. A request asking for `activity+json` or `ld+json`, or sending no `Accept` header at all, goes to Tidepool's instance actor, which must answer peers that send no `Accept`. Everything else, including `Accept: */*`, goes to the frontend. A bare `curl https://coves.social/` therefore returns the HTML home page; `curl -H 'Accept: application/activity+json' https://coves.social/` returns the actor JSON.
- `/img/*` on `coves.social` is a 301 to `img.coves.social`. The AppView serves the image proxy on any host, and only the media hostname passes through the scanning edge.
- `/.well-known/*` is served from `./static` by Caddy and shadows the AppView's own handlers. WebFinger, NodeInfo and `/ap/*` go to Tidepool.
- The frontend catch-all caps request bodies at 1 MB, because the frontend's `/api/proxy` buffers bodies in memory with no limit of its own. Raise it if a media upload is ever routed through the frontend.
- The frontend upstream has no active health check on purpose. There is one upstream and nothing to fail over to, so a single slow probe would take the site down.

## Content-Security-Policy

The AppView's fallback CSP is set-if-absent inside the `@appview` handle only. `@frontend_static` (build assets, service worker, files from the frontend's `static/`) gets a minimal fallback because adapter-node serves those before the app's hooks run. Page responses get no Caddy CSP: the frontend emits its own nonce'd policy, and a missing one must stay visibly missing. The check after any routing change:

```sh
curl -sI -H 'Accept: text/html' https://coves.social/ | grep -io "'nonce-[^']*'"
```

The `Accept: text/html` header keeps the check explicit; curl's default `*/*` also reaches the frontend. Only an `activity+json` or `ld+json` Accept, or no Accept header at all (`-H 'Accept:'`), reaches Tidepool.

## Frontend environment contract

The authoritative reference is `coves-frontend/docs/ENVIRONMENT.md`. These values in the frontend's `.env.prod` must agree with this stack:

| Frontend variable | Value | Must match |
|---|---|---|
| `PUBLIC_INSTANCE_URL`, `ORIGIN` | `https://coves.social` | `APPVIEW_PUBLIC_URL` in `docker-compose.prod.yml` |
| `PUBLIC_INTERNAL_INSTANCE` | `http://appview:8080` | the `appview` service name and its `PORT` |
| `ALLOW_HTTP_INTERNAL_INSTANCE` | `true` | required by the `http://` upstream; without it the frontend proxy returns 400 |
| `ADDRESS_HEADER` | `x-real-ip` | `header_up X-Real-IP` on the frontend routes |
| `CSP_VIDEO_ORIGINS` | `https://pds.coves.me https://coves.me https://tdpl.io` | `media-src` in the `@appview` fallback CSP |

## Compose and Caddy changes

Deploy with Compose directly from `/opt/coves`. Compose reads `/opt/coves/.env` (not in the repository) for `${VAR}` interpolation, and a value there overrides the `${VAR:-default}` defaults in the compose file. After bumping a pin in the compose file, check that `.env` does not still set the variable, or the bump does nothing. The server identity variables were renamed: `SERVER_HANDLE`, `SERVER_REPO_PASSWORD`, `SERVER_DID`, `SERVER_DOMAIN` and `SERVER_ATPROTO_PUBLIC_KEY` replace `PDS_INSTANCE_HANDLE`, `PDS_INSTANCE_PASSWORD`, `INSTANCE_DID`, `INSTANCE_DOMAIN` and `INSTANCE_ATPROTO_PUBLIC_KEY`. The old names still work as a deprecated fallback. An old name set alone logs a warning naming it and its replacement; an old name set to the same value as its replacement is silent; an old name with a different value logs a warning and is ignored. The `INSTANCE_*` and `PDS_INSTANCE_*` lines in `docker-compose.prod.yml` exist only so a rollback to a pre-rename image still works, and can be dropped once that rollback is no longer needed. Do not shell-source `.env`: some values (for example `OTEL_EXPORTER_OTLP_HEADERS`) are unquoted and contain spaces, which Compose accepts and bash does not.

To deploy the AppView, tag the image with the commit so `docker ps` shows what is running (`.env` sets `VERSION=latest`, and an exported value beats it):

```sh
cd /opt/coves
export VERSION=$(git rev-parse --short HEAD)
docker compose -f docker-compose.prod.yml build appview
docker compose -f docker-compose.prod.yml up -d --no-deps appview
```

The AppView applies pending migrations at startup. Always name the service and pass `--no-deps`. Plain `up -d` recreates every service whose config changed, and without `--no-deps` Compose also recreates any dependency whose config changed; Caddy depends on the AppView and the PDS, so a Caddy deploy after an image pin bump once restarted the database and PDS as a side effect.

Caddy mounts the `Caddyfile` as a single-file bind mount. `git pull` replaces the file with a new inode and the running container keeps reading the old one, so a Caddyfile change needs the container recreated, not reloaded. Validate first in a fresh container, because validating inside the running one reads the stale file:

```sh
cd /opt/coves
docker compose -f docker-compose.prod.yml run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile
docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate caddy
```

Then confirm the host and container inodes match (`stat -c %i Caddyfile` against `docker exec coves-prod-caddy stat -c %i /etc/caddy/Caddyfile`) and run the CSP check above. Recreation drops every hostname this Caddy terminates for a few seconds: `coves.social`, the PDS hostnames, `img.coves.social`, `tdpl.io` and the handle wildcards (`*.coves.social`, `*.tdpl.io`).

After a routing or Caddy change, these must each return 200. The first three are served by the AppView, `/healthz` by the frontend. Then do a browser login round trip (login, feed, logout).

```sh
for path in /oauth-client-metadata.json /xrpc/_health /safety/child-safety /healthz; do
  curl -sS -o /dev/null -w "%{http_code} $path\n" "https://coves.social$path"
done
```

To roll back a Caddyfile change, find its commit with `git log --oneline -1 -- Caddyfile`, run `git checkout <sha>^ -- Caddyfile`, validate, and force-recreate Caddy as above. That leaves `/opt/coves` dirty; before the next deploy, either `git checkout -- Caddyfile` or land a revert commit.

## Image proxy render version r3

The deploy that bumps the image proxy's `renderVersion` to `r3` serves WebP (unless a side exceeds 16383 px) and 192px `avatar_small`. The disk cache is not keyed by render version, so already-cached renders keep their old JPEG or PNG bytes and 360px avatars until the cache is cleared. Clear it while no AppView is running: an r3 container serving an old entry would give it an r3 ETag, and a browser holding that ETag gets 304 before the cache is read, so it keeps the old bytes. Build the new image, stop the AppView, clear the `coves-prod-imageproxy-cache` volume (mounted at `IMAGE_PROXY_CACHE_PATH`) with a one-off container, then start the new AppView:

```sh
cd /opt/coves
export VERSION=$(git rev-parse --short HEAD)
docker compose -f docker-compose.prod.yml build appview
docker compose -f docker-compose.prod.yml stop appview
docker run --rm -v coves-prod-imageproxy-cache:/cache alpine find /cache -mindepth 1 -delete
docker compose -f docker-compose.prod.yml up -d --no-deps appview
```

Then run Purge Everything on the `coves.social` zone in Cloudflare; only `img.coves.social` is proxied, so this drops only cached images (see [MEDIA.md](MEDIA.md#edge-purge)). Images are served with `public, max-age=86400` (and `s-maxage=31536000` on purgeable URLs when CDN purge is configured), so without the purge edges keep the old renders for up to a day, or a year for those URLs. Browsers holding an `r2` ETag fetch the new render on their next revalidation.

## Bridged handle reconciliation

`cmd/reconcile-handles` is rollout steps 6 and 6a of the Tidepool handle rename (Tidepool `docs/PRD_HANDLE_RENAME.md` §6). It selects every `users` and `communities` row whose `pds_url` is a trusted bridge host, resolves each DID fresh against PLC, and writes changed handles through the same per-DID locked refresh the identity-event consumers run: users' OAuth session handles are updated too, and a community handle must pass handle validation and the hostedBy check first. It loads the AppView's configuration, so it needs the environment the AppView container gets.

The image built from `Dockerfile` contains only the server. Build the tool from the `/opt/coves` checkout in the Dockerfile's builder image and run it on `coves-prod-network`, because Postgres publishes no port:

```sh
cd /opt/coves
(
  set -a; . <(grep -E '^(POSTGRES_USER|POSTGRES_PASSWORD|POSTGRES_DB|ENCRYPTION_KEY|OAUTH_SEAL_SECRET|CURSOR_SECRET|IMAGE_PROXY_BASE_URL)=' .env); set +a
  export DATABASE_URL="postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@coves-prod-postgres:5432/${POSTGRES_DB}?sslmode=disable"
  docker run --rm --network coves-prod-network \
    -e DATABASE_URL -e ENCRYPTION_KEY -e OAUTH_SEAL_SECRET -e CURSOR_SECRET \
    -e IMAGE_PROXY_BASE_URL="${IMAGE_PROXY_BASE_URL:-https://img.coves.social}" \
    -e SERVER_DID=did:web:coves.social -e SERVER_DOMAIN=coves.social \
    -e APPVIEW_PUBLIC_URL=https://coves.social -e PDS_URL=https://coves.me \
    -e 'JETSTREAM_FEEDS=bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080' \
    -e AUTH_SKIP_VERIFY=false -e SKIP_DID_WEB_VERIFICATION=false \
    -e PORT=8080 -e ENV=production -e LOG_LEVEL=info \
    -e IMAGE_PROXY_CACHE_PATH=/var/cache/coves/images \
    -v "$PWD":/src:ro -w /src golang:1.27.4-alpine3.24 \
    sh -c 'CGO_ENABLED=0 go build -buildvcs=false -o /tmp/reconcile-handles ./cmd/reconcile-handles && exec /tmp/reconcile-handles -trusted-hosts https://tdpl.io -dry-run'
)
```

- The subshell exports only the seven `.env` keys the tool needs, so the shell strips quotes from their values the way Compose does for the AppView, and nothing stays in the operator's shell afterwards. Do not source the whole file: bash fails on its unquoted values with spaces and prints part of the value. Do not use `docker run --env-file .env`: it passes values verbatim, so a quoted value such as `ENCRYPTION_KEY="<base64>"` reaches the tool with its quotes and configuration loading refuses it.
- Bare `-e NAME` copies the value from that environment without putting it on the command line. Only the `.env` values that configuration loading requires in production are passed: `ENCRYPTION_KEY`, `OAUTH_SEAL_SECRET`, `CURSOR_SECRET`, and `IMAGE_PROXY_BASE_URL` with the compose default `https://img.coves.social`. The AppView's other `.env` settings are optional and unused by the tool, so their defaults load.
- The `-e NAME=value` flags are the literals in the `appview` service's `environment:` block of `docker-compose.prod.yml`. `.env` may lack them or hold a different value. Keep them in step with that block.
- `DATABASE_URL` is built from `.env`'s `POSTGRES_*` values as compose builds it, with host `coves-prod-postgres` instead of `postgres`, because Tidepool's stack shares `coves-prod-network` and its `postgres` alias can resolve to Tidepool's database.
- At startup the tool prints `reconcile-handles: plc_url=<url>` on stderr. It must be `https://plc.directory`, the AppView's PLC directory: the compose file passes the AppView neither `PLC_DIRECTORY_URL` nor `IDENTITY_PLC_URL`, and this command passes neither. Any other value means the command was changed to pass one; stop and remove it.

Flags: `-trusted-hosts https://tdpl.io` (comma-separated scheme and host; replaces `TRUSTED_BRIDGE_PDS_HOSTS`, and with neither set the tool exits before opening anything), `-clear-identity-cache` (deletes every `identity_cache` row before the sweep and prints `identity_cache_deleted=<n>`), `-dry-run` (resolves and verifies only: no lock, no purge, no writes; a row that would change is listed with `reason=-`), and `-concurrency N` (default 4). `-dry-run` with `-clear-identity-cache` is refused, because a dry run must not change the cache.

Exit status: 0 means every bridged row matches its PLC-verified handle. 1 with `differs` lines means rows still differ. 1 with no `differs` lines means the run failed; see stderr. 2 means a flag or setting was refused (for example `-dry-run` with `-clear-identity-cache`, or no trusted hosts).

- Step 6, once the rename job has finished: rehearse with `-dry-run`, then run with `-clear-identity-cache` in place of `-dry-run`. The dry run lists every row it would update with `reason=-` and exits 1; that is expected before the real run. Clearing the cache alone does not fix rows already stored with an old handle.
- Step 6a, the gate: re-run without `-clear-identity-cache` until it exits 0. Each remaining row is printed as `differs kind=<user|community> did=<did> stored=<handle> verified=<handle|-> reason=<code>` before the totals line, and stderr has a `reconcile-handles: cause kind=<kind> did=<did> reason=<reason>: <error>` line for it. `lock-busy` (a live refresh held the DID's lock) and `hostedby-pending` (the hostedBy DID document could not be fetched in this run) mean re-run. The other reasons (`resolve-error`, `unverified`, `subject-mismatch`, `handle-conflict`, `invalid-handle`, `hostedby-mismatch`, `error`) need a look at the identity or the conflicting row; the row's `cause` line says what failed.

## Enabling or re-enabling the content classifier

The consumers queue a classification job only while `CLASSIFIER_ENABLED=true`. Posts, comments and communities indexed while the classifier is off are never queued, and there is no initial back-fill. After turning the classifier back on after a period off, queue the content indexed while it was off with `social.coves.moderation.enqueueClassification`, called as an admin listed in `MODERATION_ADMINS` (`RequireInstanceAdmin`: an OAuth session or a PDS service JWT for that DID):

1. Set `CLASSIFIER_ENABLED=true`, `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_WORKERS_AI_API_TOKEN` in `/opt/coves/.env`, and deploy the AppView as above.
2. Re-enabling only (D13: no initial back-fill); a first enable runs neither this step nor step 3. Queue posts and comments indexed while the classifier was off: `POST /xrpc/social.coves.moderation.enqueueClassification` with `{"target": "postsAndComments", "since": "<start>", "until": "<now>"}`. `<start>` is the time the classifier was turned off, minus a margin at least as long as the worst consumer lag in that period (an hour is a safe default). `<now>` is the time you make the call, not the moment the AppView came back with the classifier on. `since` is inclusive and `until` exclusive. The range is on each post's and comment's `indexed_at`, which is the Jetstream event time of the last applied create or update, not the time the AppView processed it. An event delivered late (consumer lag, cursor replay) carries an earlier event time, which is why `<start>` needs the margin. A later update moves `indexed_at` forward even when it changes no classified field, such as a bridged vote-count update; that update queues nothing, so a post indexed while the classifier was off and updated after it came back has an `indexed_at` after the re-enable moment, and only an `until` of the call time selects it. An overlap costs nothing: a subject whose job already exists under the current evaluation is counted, not queued twice.
3. Re-enabling only: queue every indexed community with the same call with `{"target": "allCommunities"}` and no range.

Each call answers `{created, revived, alreadyPresent}`: jobs inserted; failed jobs under the same evaluation (same record CID, policy version and models), queued again due now with their attempt count, last error code and deferral backoff reset; and in-scope subjects whose identical job already existed in any other state. A closed job is not revived: its subject was superseded or not visible. Repeating a call is safe: it creates nothing new and revives only jobs that failed again since the last call, so re-running a range is how to retry jobs that failed, for example during a provider outage. The worker drains the queue under `CLASSIFIER_DAILY_REQUEST_CAP`, so a large range can take several days to classify. With the classifier off the endpoint answers `ClassifierNotConfigured`.

A policy version or model change is a new evaluation: the same calls over already-scored content queue new jobs under it, and earlier results are kept.

### Classifier configuration

Set these in `/opt/coves/.env`; `docker-compose.prod.yml` forwards each one to the AppView with the default below. With `CLASSIFIER_ENABLED=true` the AppView refuses to start when a required value is missing, an interval, limit or delay is not greater than 0, or `CLASSIFIER_SCORE_ALERTS` does not parse.

| Key | Default | Meaning |
|---|---|---|
| `CLASSIFIER_ENABLED` | `false` | Queue and classify posts, comments and communities. |
| `CLOUDFLARE_ACCOUNT_ID` | none, required when enabled | Cloudflare account that runs Workers AI: the account holding the `coves.social` zone (D12). |
| `CLOUDFLARE_WORKERS_AI_API_TOKEN` | none, required when enabled | Workers AI API token (scope below). |
| `CLASSIFIER_TEXT_MODEL_ID` | `@cf/cloudflare/clef-flash` | Model for text-only requests. |
| `CLASSIFIER_IMAGE_MODEL_ID` | `@cf/cloudflare/clef` | Model for requests carrying images. |
| `CLASSIFIER_DAILY_REQUEST_CAP` | `5000` | Workers AI requests per UTC day across both models; once reached, jobs wait in the queue until the next UTC day. |
| `CLASSIFIER_WORKER_INTERVAL` | `30s` | How often the worker runs. |
| `CLASSIFIER_CONCURRENCY` | `2` | Requests in flight per run; a run claims 4 jobs per slot. |
| `CLASSIFIER_TEXT_CHAR_LIMIT` | `8000` | Characters of text sent per request; longer text is truncated and the result says so. |
| `CLASSIFIER_ATTEMPT_LIMIT` | `5` | Attempts before a job is marked failed. |
| `CLASSIFIER_RETRY_BASE_DELAY` | `1m` | First retry delay; each later retry doubles it. |
| `CLASSIFIER_PENDING_DEFERRAL` | `10m` | First wait before re-checking a subject that is not yet visible (for example a post awaiting community acceptance); each later deferral of the same job doubles it, capped at 6 hours, and the count resets when the subject is accepted. |
| `CLASSIFIER_SCORE_ALERTS` | `sexual_minors:0.3` | Score alerts: comma-separated `question:threshold` pairs, threshold in (0, 1]. |
| `CLASSIFIER_SCORE_ALERT_ATTEMPT_LIMIT` | `8` | Send attempts per score alert before it stays undelivered. |
| `CLASSIFIER_SCORE_ALERT_RETRY_BASE_DELAY` | `1m` | First score alert retry delay; each later retry doubles it, capped at 6 hours. |
| `CLASSIFIER_SCORE_ALERT_SWEEP_INTERVAL` | `5m` | How often failed score alerts are retried. |

Score alerts go to the Telegram chat configured by `TELEGRAM_ALERTS_ENABLED`, `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`, the same chat as admin report alerts. With Telegram off, alerts are still recorded but nobody is notified; the AppView logs a warning at startup.

### Cloudflare token

Create the token in the Cloudflare dashboard on the account that holds the `coves.social` zone, with one permission: Account, Workers AI, Read and Edit (Edit is what allows running a model). Give it no zone permissions and no other account permissions.

Production uses its own long-lived token, created at deploy and stored only in `/opt/coves/.env`. The one-week token used during development (D12) is a different token; do not copy it to production, and let it expire. To rotate the production token, create the new one, replace `CLOUDFLARE_WORKERS_AI_API_TOKEN` in `.env`, deploy the AppView, then delete the old token in the dashboard.

#### When Cloudflare refuses the token

A 401 or 403 from Workers AI (a revoked, expired, mistyped or under-scoped token) is not counted against any job. The worker gives back every job it claimed in that run with its attempt count unchanged, stops the run, and pauses claiming: 5 minutes the first time, doubling on each refusal after a pause (10, 20, 40 minutes) up to 1 hour, and back to 5 minutes after the next accepted request. Each pause logs one Error line, `classification paused: provider refused the API token`, with `code=provider_auth`, `http_status` (401 or 403), `paused_for` and `paused_until`; the token is never logged. While paused, `getClassificationMetrics` answers `authPausedUntil`. Jobs wait in `queued` or `retrying` and none fails, so no re-enqueue is needed afterwards.

To fix it, check `CLOUDFLARE_WORKERS_AI_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` in `/opt/coves/.env` against the dashboard (the token must be active and carry Account, Workers AI, Read and Edit on that account), replace the token if needed, and deploy the AppView. The pause is held in memory, so the restart clears it and the first request tests the new token; a token that is still refused starts a new 5-minute pause and logs again.

### Partial results

A subject with more than four images is sent as several requests, one per batch of four. When a later batch's request fails at `CLASSIFIER_ATTEMPT_LIMIT` after earlier batches were answered, the job still ends `failed` (its attempt count and `last_error_code` are kept), and the answered batches are stored as a result with `partial: true` in `listClassificationResults`: each question's highest answer across those batches, with the images never sent counted in `imagesSkipped`. The log line has `code=partial_result_stored`. A partial result raises score alerts like any result, so a minors answer from the first batch is not lost to a failure on the second. It accepts a verdict and counts toward precision like any result. A job that failed before any batch was answered, including every single-request subject, stores nothing.

Re-enqueueing a range revives a failed job that holds a partial result. When the revived job completes, its result replaces the partial one in place: same result ID (verdicts stay attached), `partial: false`, `createdAt` set to the replacement time, and an alert already raised for a question is not raised again.

### Score alerts

After a result is stored, each `CLASSIFIER_SCORE_ALERTS` pair whose question scores at or above the threshold sends one Telegram message: `Classifier score alert: result <id>, question <key>, subject <post|comment|community>`, then a `URI: <subject AT-URI or community DID>` line and an `Open: <link>` line pointing at the subject on the web app under `APPVIEW_PUBLIC_URL` (left out when no link can be built, such as a comment whose row is gone). Link previews are disabled, so Telegram never fetches the linked page. Question keys are those of the policy (`internal/core/classification/policy/v3.json`): `sexual_explicit`, `sexual_suggestive`, `sexual_minors` (asked only in requests that carry an image, so a text-only result never raises a `sexual_minors` alert), `graphic_violence`, `hate_harassment`, `spam_scam`. A failed send is retried by the sweep; an alert that reaches `CLASSIFIER_SCORE_ALERT_ATTEMPT_LIMIT` stays undelivered and is counted in the metrics below.

The URI and Open lines name the subject. To see the result's scores, or when an alert has no Open line, list results with `GET /xrpc/social.coves.moderation.listClassificationResults?question=<key>&minProbability=<threshold in parts per million>&subjectKind=<kind>` as an admin and pick the entry whose `id` is the alert's result ID; its `subjectUri` and `subject` name the content.

On a `sexual_minors` alert:

1. Look at the subject through the result's `subject` view and decide whether it is CSAM. The score is not a confirmation.
2. If it is, follow the [CSAM matches](MEDIA.md#csam-matches) steps: preserve, then remove the subject with `removeContent` and reason `illegal-content` before anything deletes the record or the account, then take down the record and blob on the owning PDS and act on the account. The [CSAM response PRD](PRD_CSAM_RESPONSE.md) describes the planned tooling. Coves' own NCMEC report is not automated yet (D5).
3. Record a verdict on the result with `recordClassificationVerdict` either way, so the precision report reflects it.

### Classifier metrics

`GET /xrpc/social.coves.moderation.getClassificationMetrics`, called as an admin listed in `MODERATION_ADMINS` (an OAuth session or a PDS service JWT for that DID), answers the classifier's state read from its tables. It is served whether or not the classifier is enabled.

- `jobs`: current job counts: `queued`, `deferred` (awaiting community acceptance, re-checked after `CLASSIFIER_PENDING_DEFERRAL`), `retrying`, `sent` (classified with a stored result), `failed` (attempts exhausted; a failed job may hold a partial result, see below), `closedSuperseded` (an edit replaced the job) and `closedNotVisible` (the subject was deleted or hidden). With the classifier enabled, `queued`, `deferred` and `retrying` count only jobs under the current policy version and models, the ones the worker claims; jobs left pending under an earlier policy or model are never claimed and are not counted. With the classifier disabled they count every pending job. The other counts cover every policy version and model.
- `usage`: Workers AI requests and input tokens per model per UTC day, newest day first.
- `workerLagSeconds`: seconds since the oldest due pending job became due; 0 when nothing is due. With the classifier enabled it covers only jobs under the current policy version and models, so a policy or model change does not leave it growing on jobs the worker no longer claims. A lag that keeps growing then means the worker is stopped, failing, or held by the daily request cap. With the classifier disabled it covers every pending job and grows while any is due, because no worker runs.
- `authPausedUntil`: present only while the worker is paused because Cloudflare refused the API token; when the pause ends. See "When Cloudflare refuses the token".
- `undeliveredFlagEvents` and `undeliveredScoreAlerts`: community NSFW flag events and score alerts not yet delivered, including those that exhausted their attempts. Flag events are recorded but never sent while `COMMUNITY_SIGNAL_ALERTS_ENABLED` is false (the default) or Telegram is not enabled, so with either setting off `undeliveredFlagEvents` grows on every community NSFW flag change and no per-event line is logged. Score alerts are never sent while Telegram is not enabled. With sending on, a count that stays above 0 means sends are failing: a failed flag event send logs `send_failed`, a failed score alert send logs `send_failed`, and a score alert's last allowed failed send logs `attempts_exhausted`.
