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

`scripts/deploy.sh` refuses to run without `.env.prod` and sources it with `set -a`. Compose prefers exported variables over `.env`, so a value in `.env.prod` beats `/opt/coves/.env`, and either overrides the `${VAR:-default}` defaults in the compose file. After bumping a pin in the compose file, check that neither `.env.prod` nor `.env` still sets the variable, or the bump does nothing.

The script runs `build --no-cache` before `up`. `scripts/deploy.sh <service>` runs `up -d --no-deps <service>`. With no service argument it runs plain `up -d`, which recreates every service whose config changed. Without `--no-deps`, Compose also recreates any dependency whose config changed; Caddy depends on the AppView and the PDS, so a Caddy deploy after an image pin bump once restarted the database and PDS as a side effect.

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
