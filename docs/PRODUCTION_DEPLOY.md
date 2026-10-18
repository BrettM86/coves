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

- The apex `/` splits on `Accept`. An explicit `text/html` goes to the frontend. Everything else, including a request with no `Accept` header, goes to Tidepool's instance actor, which must answer peers that send no `Accept`. A bare `curl https://coves.social/` therefore returns actor JSON.
- `/img/*` on `coves.social` is a 301 to `img.coves.social`. The AppView serves the image proxy on any host, and only the media hostname passes through the scanning edge.
- `/.well-known/*` is served from `./static` by Caddy and shadows the AppView's own handlers. WebFinger, NodeInfo and `/ap/*` go to Tidepool.
- The frontend catch-all caps request bodies at 1 MB, because the frontend's `/api/proxy` buffers bodies in memory with no limit of its own. Raise it if a media upload is ever routed through the frontend.
- The frontend upstream has no active health check on purpose. There is one upstream and nothing to fail over to, so a single slow probe would take the site down.

## Content-Security-Policy

The AppView's fallback CSP is set-if-absent inside the `@appview` handle only. `@frontend_static` (build assets, service worker, files from the frontend's `static/`) gets a minimal fallback because adapter-node serves those before the app's hooks run. Page responses get no Caddy CSP: the frontend emits its own nonce'd policy, and a missing one must stay visibly missing. The check after any routing change:

```sh
curl -sI -H 'Accept: text/html' https://coves.social/ | grep -io "'nonce-[^']*'"
```

The `Accept` header matters: without it the apex goes to Tidepool.

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

Deploy with Compose directly from `/opt/coves`. Compose reads `/opt/coves/.env` (not in the repository) for `${VAR}` interpolation, and a value there overrides the `${VAR:-default}` defaults in the compose file. After bumping a pin in the compose file, check that `.env` does not still set the variable, or the bump does nothing. Do not shell-source `.env`: some values (for example `OTEL_EXPORTER_OTLP_HEADERS`) are unquoted and contain spaces, which Compose accepts and bash does not.

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
    -e INSTANCE_DID=did:web:coves.social -e INSTANCE_DOMAIN=coves.social \
    -e APPVIEW_PUBLIC_URL=https://coves.social -e PDS_URL=https://coves.me \
    -e 'JETSTREAM_FEEDS=bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080' \
    -e AUTH_SKIP_VERIFY=false -e SKIP_DID_WEB_VERIFICATION=false \
    -e PORT=8080 -e ENV=production -e LOG_LEVEL=info \
    -e COMMUNITY_CREATORS=did:web:coves.social -e IMAGE_PROXY_CACHE_PATH=/var/cache/coves/images \
    -v "$PWD":/src:ro -w /src golang:1.26.8-alpine3.24 \
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
