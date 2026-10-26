# Local development

The dev stack runs in Docker from `docker-compose.dev.yml` with settings from `.env.dev`. The AppView itself runs on the host. `make help` lists every target.

## Ports

| Service | Host port | Started by |
|---|---|---|
| Postgres (dev, `coves_dev`) | 5435 | `make dev-up` |
| Postgres (PLC) | 5436 | `make dev-up` |
| Postgres (test, `coves_test`) | 5434 | `make test-integration` |
| PDS | 3001 | `make dev-up` |
| PLC directory | 3002 | `make dev-up` |
| Jetstream / metrics | 6008 / 6009 | `make dev-up` |
| AppView | 8081 | `make run` or `make run-web` |
| Web proxy (Caddy) | 8080 | `make web-proxy` |
| Jaeger UI / OTLP gRPC | 16686 / 4317 | `make dev-up-otel` |
| Relay | 2471 | not started by any dev target (`relay` profile) |

## Running

`make dev-up` starts Postgres, the PLC directory and its Postgres, the PDS and Jetstream. `make run` runs `make db-migrate` and then `scripts/dev-run.sh`, which sources `.env.dev` and runs `go run -tags dev ./cmd/server`. The server also applies migrations itself at startup.

The `dev` build tag compiles in the localhost OAuth resolvers (`internal/atproto/oauth/dev_resolver.go`, `dev_auth_resolver.go`). Without it the server falls back to stubs and OAuth against the local HTTP PDS does not work. Do not start the dev AppView with plain `go run ./cmd/server`.

The AppView ingests through Jetstream, not the PDS firehose. `.env.dev` sets `JETSTREAM_FEEDS=self=ws://localhost:6008`, and the dev Jetstream reads the local PDS's `subscribeRepos`.

## Web frontend

`make run-web` runs migrations, starts the frontend's Vite server from `../coves-frontend` (override with `COVES_FRONTEND_DIR`) on 5173, and runs the AppView on 8081 with `APPVIEW_PUBLIC_URL=http://127.0.0.1:8080`. Start the proxy separately with `make web-proxy` (or `web-proxy-bg`). `Caddyfile.dev` sends `/oauth/*`, `/xrpc/*`, `/oauth-client-metadata.json`, `/img/*` and `/api/me` to the AppView and everything else to Vite, so the browser sees one origin.

Open the app at `http://127.0.0.1:8080`, not `localhost`. The PDS rejects `localhost` in an OAuth `redirect_uri` (RFC 8252), and the session and OAuth binding cookies are host-only, so mixing the two names splits them and login fails. The same rule applies to `make run`, where `APPVIEW_PUBLIC_URL` is `http://127.0.0.1:8081`.

## Identity

Identity, handle checks and OAuth resolve through the local PLC (`PLC_DIRECTORY_URL=http://localhost:3002`) and the local PDS. Never point dev at public Bluesky services: a lookup that succeeds there does not prove local login works.

`HANDLE_WELL_KNOWN_HOSTS` sends handle verification for `.local.coves.dev` and `.coves.social` to the local PDS, because the dev PDS mints handles under both (`PDS_SERVICE_HANDLE_DOMAINS`). Config refuses it unless `IS_DEV_ENV=true`. A side effect: a dev AppView that consumes a feed carrying real `coves.social` communities verifies them against the local PDS, gets a 404 and indexes nothing for them.

## Secrets that must stay stable

Keep `OAUTH_SEAL_SECRET` and `ENCRYPTION_KEY` set in `.env.dev`. If `OAUTH_SEAL_SECRET` is unset, the server generates one per boot and every restart signs out every user, mobile sessions included. If `ENCRYPTION_KEY` is unset, it generates a random key per boot and every stored community and aggregator credential becomes unreadable at the next restart. The server logs a warning at startup for either case.

## Migrations

Migrations are goose SQL and Go files in `internal/db/migrations/`, named with a sequential three-digit prefix (`053_name.sql`), and embedded into the binary through `migrations.FS`. Create one with `goose -s -dir internal/db/migrations create name sql`; without `-s` goose uses a timestamp prefix, which breaks the ordering. Write both `Up` and `Down`. Never edit a migration that has been applied anywhere; add a new one.

`make db-migrate` and `make db-migrate-down` run `go run ./cmd/migrate up|down`, which applies the embedded migrations the same way the server does at startup. Set `MIGRATE_DSN=...` to target another database. Go backfills register with `goose.AddMigrationContext` in `init()`. The stock `goose` CLI cannot run them; apply migrations through `cmd/migrate` or the server.

## Tests

The test tiers, targets and the merge gate are in [TEST_ARCHITECTURE.md](TEST_ARCHITECTURE.md).

The test Postgres (`coves-test-postgres`) keeps its data in memory with durability off, like the CI one. Recreating the container discards everything, including the template; the next `make test-integration` rebuilds it.
