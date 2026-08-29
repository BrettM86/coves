# Frontend deployment: how the SvelteKit app is wired into production

The web frontend lives in the sibling repo **`coves-frontend`** and is deployed as its **own
Docker Compose project** from its own checkout on the production box. This document is the
backend-side view: what this repo contributes (Caddy routing), how the two stacks meet, and the
one-time cutover order. Day-to-day deploys use each repo's `/deploy` command.

## Topology

```
browser ──HTTPS──▶ caddy (coves-prod-caddy, this repo)
                     │  coves.social
                     ├─ /.well-known/*, /ap/*, webfinger, nodeinfo ─▶ tidepool / static
                     ├─ /img/* ─▶ 301 img.coves.social
                     ├─ @appview allowlist (/xrpc/*, /oauth/*, /api/me, …) ─▶ appview:8080
                     └─ everything else (pages, /_app/*, /api/auth/*, /api/proxy/*)
                                                     ─▶ coves-prod-frontend:3000
                                                           │ server-side (/api/me check,
                                                           │ /api/proxy upstream)
                                                           └─▶ http://appview:8080 (Docker network)
```

- Both stacks share `coves-prod-network` (declared here, `external:` in the frontend's compose).
- Caddy addresses the frontend by **container name** (`coves-prod-frontend`), never the bare
  `frontend` service alias — aliases are not unique across compose projects on one network.
- The frontend container publishes **no port**, so nothing outside Docker can reach it. Its
  `ADDRESS_HEADER=x-real-ip` makes adapter-node trust `X-Real-IP` from *any* peer; Caddy
  overwrites that header unconditionally (`header_up X-Real-IP {remote_host}`), which is what
  makes the value trustworthy. The boundary is **membership of `coves-prod-network`** — other
  containers on it (appview, pds, tidepool, aggregators) could dial `:3000` and assert any
  address. They are our own infrastructure; this is the accepted trust model, not an enforced
  one. Never publish the port and never join untrusted containers to that network.
- Note `/api/me` is the AppView's; every other `/api/*` (`/api/auth/*`, `/api/proxy/*`) is the
  frontend's. A future Go `/api/*` route must be added to `@appview`; a future frontend
  `/api/me` would be shadowed.
- The `coves.social` Caddy block is an **explicit allowlist** for the AppView plus a frontend
  catch-all. A new non-XRPC Go route must be added to the `@appview` matcher or the frontend
  will answer it with its 404 page. `internal/api/routes/caddy_allowlist_test.go` (T0, in
  `make test`) walks the real router against the Caddyfile in both directions and fails on
  drift.
- The AppView's fallback `Content-Security-Policy` is scoped to the `@appview` handle; the
  frontend's sirv-served assets (`/_app/*`, `/service-worker.js`, files from its `static/`) get a
  minimal fallback in `@frontend_static`; page responses get none, because the frontend emits its
  own nonce'd policy. The launch gate is a `'nonce-…'` in `script-src` on
  `curl -sI -H 'Accept: text/html' https://coves.social/` (the `Accept` header matters — the apex
  sends anything else to Tidepool).

## Environment contract (frontend `.env.prod`)

Authoritative reference: `coves-frontend/docs/ENVIRONMENT.md`; template:
`coves-frontend/.env.prod.example`. The values that must agree with this stack:

| Frontend variable              | Value                     | Must match                                        |
| ------------------------------ | ------------------------- | ------------------------------------------------- |
| `PUBLIC_INSTANCE_URL`, `ORIGIN`| `https://coves.social`    | `APPVIEW_PUBLIC_URL` in `docker-compose.prod.yml` |
| `PUBLIC_INTERNAL_INSTANCE`     | `http://appview:8080`     | the `appview` service name + `PORT` here          |
| `ALLOW_HTTP_INTERNAL_INSTANCE` | `true`                    | required by the plaintext `http://` scheme above — without it the frontend's proxy rejects the upstream with 400 |
| `ADDRESS_HEADER`               | `x-real-ip`               | `header_up X-Real-IP` in the frontend Caddy block |
| `CSP_VIDEO_ORIGINS`            | `https://pds.coves.me https://coves.me https://tdpl.io` | `media-src` in the `@appview` fallback CSP |

## First-time cutover (one-off, in this order)

**Prerequisite:** the `coves-frontend` `prod-deploy` branch (compose project, `scripts/deploy.sh`,
`.env.prod.example`, and the `/api/me` client-address stamping in `hooks.server.ts`) must be
merged to its `main` first. Without the last item every logged-in page view counts against one
rate-limit bucket at the AppView and users get silently logged out under load.

Order matters: Caddy has a single frontend upstream with no failover, so recreating it before
the frontend container exists 502s every page.

1. **Backend repo** — merge and `git pull` this change on the box, but **do not recreate caddy
   yet.** (The AppView needs no rebuild; nothing in Go changed.)
2. **Frontend checkout** — on the box:
   ```sh
   git clone <coves-frontend remote> /opt/coves-frontend
   cd /opt/coves-frontend && cp .env.prod.example .env.prod   # then review every value
   ./scripts/deploy.sh
   ```
   The script builds `coves/frontend:<sha>`, starts `coves-prod-frontend` on
   `coves-prod-network`, and waits for `/healthz` (a health timeout is a *warning*, not a
   failure — read the output). Its final curl of `https://coves.social/` will *warn* that no
   nonce'd CSP is served — expected, Caddy still points at the AppView.
3. **Preflight the new Caddyfile** in a fresh container (validating inside the running one
   would read the stale bind-mounted inode):
   ```sh
   cd /opt/coves && docker compose -f docker-compose.prod.yml run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile
   ```
4. **Recreate caddy** (coves `/deploy` Step 5b — the single-file bind mount does not pick up
   `git pull`):
   ```sh
   cd /opt/coves && docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate caddy
   ```
   Confirm host/container inodes match, then run the verification below. ~2–5 s of edge
   downtime for **every** hostname this Caddy terminates: coves.social, the PDS hostnames,
   img.coves.social, tdpl.io and the bridged-handle wildcards.
5. **Verify**
   ```sh
   curl -sI -H 'Accept: text/html' https://coves.social/ | grep -io "'nonce-[^']*'" | head -1   # nonce → frontend
   curl -sS -o /dev/null -w '%{http_code}\n' https://coves.social/oauth-client-metadata.json  # 200 → appview
   curl -sS -o /dev/null -w '%{http_code}\n' https://coves.social/xrpc/_health                 # 200 → appview
   curl -sS -o /dev/null -w '%{http_code}\n' https://coves.social/safety/child-safety          # 200 → appview page
   curl -sS -o /dev/null -w '%{http_code}\n' https://coves.social/healthz                      # 200 → frontend
   ```
   Then a real login round-trip in a browser (login → feed → logout), and confirm the frontend
   log has no boot-time `ADDRESS_HEADER` warning (`docker logs coves-prod-frontend | head`).

**Rollback** (without touching the frontend container): find the cutover commit with
`git log --oneline -1 -- Caddyfile`, then `git checkout <that-sha>^ -- Caddyfile` and
force-recreate caddy again; the catch-all goes back to the AppView. This leaves `/opt/coves`
dirty — before the next `/deploy`, either `git checkout -- Caddyfile` (re-applies the cutover)
or land a proper revert commit.

## Known follow-up: OAuth `redirect_uri` / `state` are ignored by the web login

The frontend's `POST /api/auth/login` starts OAuth at
`/oauth/login?handle=…&redirect_uri=<origin>/api/auth/callback&state=…`. The backend's web
`HandleLogin` (`internal/atproto/oauth/handlers.go`) reads only `?redirect=` (a local path) and
its callback lands the user on `APPVIEW_PUBLIC_URL/`. Login still works — the `coves_session`
cookie is set on the shared origin and the frontend's `hooks.server.ts` validates it via
`/api/me` — but the frontend's CSRF-state check in `/api/auth/callback` never runs and the
user always lands on `/` instead of the page they were on. Fix on either side (backend honours
`state` + a same-origin `redirect_uri`, or the frontend sends `?redirect=<local path>` and drops
its callback route); tracked as a post-launch item, not a blocker.
