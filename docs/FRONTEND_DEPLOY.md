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

## Web OAuth return pages and errors

The frontend starts web login at `/oauth/login?handle=…&redirect=<local path>`.
Go owns the OAuth transaction: it saves the validated return path and an expiring
browser binding with the provider's OAuth request. The callback claims that
binding once before exchanging the code or issuing `coves_session`. Success
returns to the saved path, including its query and fragment. Failed login returns
to `/login?error=<known-code>&redirect=<saved-path>` so the frontend can explain
the failure and retain the destination for retry. Invalid or unrelated callbacks
use `/` as the retry destination and do not clear another pending login cookie.
A new login replaces the browser's binding cookie, so only the newest attempt
can complete from that browser; the earlier attempt's server-side binding stays
claimable by its original cookie value until it expires (10 minutes). The
binding cookie is scoped to `/oauth`, so browsers send it only to Go's OAuth
routes and never to the SvelteKit server.
The login page consumes the error query parameter and clears the message before
retry, so reload/back navigation does not restore an old failure. Invalid or
expired browser bindings use `invalid_request`; storage and configuration
failures use `server_error` and produce sanitized operational diagnostics.

Apply migration `047_web_oauth_binding.sql` before deploying the backend change.
Deploy the matching frontend and backend together; there is no frontend OAuth
callback route or additional frontend callback URI to register. The provider's
registered callback remains `/oauth/callback` on `APPVIEW_PUBLIC_URL`.
`PUBLIC_INSTANCE_URL` must name that same browser origin; production uses HTTPS
and secure cookies. Keep `/oauth/*` routed to Go and `/api/auth/*` to SvelteKit.

Local development uses the configured local PDS and PLC through the same proxy
arrangement. Keep the browser origin on `127.0.0.1` to match the local OAuth
callback; mixing it with `localhost` would separate their host-only cookies.
No public Bluesky resolver or production configuration is required for this flow.

Mobile continues to use `/oauth/mobile/login` and the existing callback allowlist.
Valid mobile completion and error handoffs are separate from the web transaction;
a rejected mobile callback cannot fall back to creating a web session. This does
not add callback registrations for third-party mobile clients.
