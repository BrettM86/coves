# Architecture

## Layering

Data flows client → PDS → Jetstream → AppView → PostgreSQL. The AppView only
indexes; it never writes CAR files or touches PDS internals.

Each domain has one package under `internal/core/<domain>` holding its models,
service and repository interfaces. Handlers in `internal/api/handlers/<domain>`
only handle XRPC; services coordinate repositories, which live in
`internal/db/postgres`. Consumers in `internal/atproto/jetstream` turn firehose
events into repository writes. `internal/atproto/lexicon` holds lexicon JSON,
not generated Go types; see [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md).

## Server startup

`cmd/server` is split by concern:

| File | Responsibility |
|---|---|
| `main.go` | `run()` orchestration, serve loop, graceful shutdown |
| `wiring.go` | Builds repositories, services and middleware in dependency order |
| `*_wiring.go` | CDN purge, image proxy and moderation admin wiring |
| `routes.go` | Router construction and route registration |
| `consumers.go` | Jetstream consumers, feed topology, dead letter redriver |
| `database.go` | Migrations, the application pool, the session coordination pool |
| `httpserver.go` | HTTP listener with every timeout set |
| `jobs.go`, `*_job.go` | Background ticker jobs |
| `health.go` | Liveness and consumer health handlers |
| `pds.go` | Instance PDS login at startup |

`database.go` opens two pools. The application pool carries a
`statement_timeout`; migrations run on their own connection without it,
because an index build or backfill may legitimately run long. The session
coordination pool is small and dedicated: each authenticated PDS write holds
one of its connections under a per-session advisory lock for the whole PDS
round trip, so slow PDS writes cannot starve feed queries. Before goose runs,
startup re-encrypts any legacy database-held credentials with the application
cipher (see [CREDENTIAL_ENCRYPTION.md](CREDENTIAL_ENCRYPTION.md)).

Background jobs go through `runTicker` in `jobs.go`: OAuth cleanup, bridged
vote poll, acceptance queue, aggregator token refresh, Discover Hot cleanup,
moderation idempotency sweep, CDN purge sweep, blocked-media purge and
notification retention. It runs one cycle immediately at startup, gives each
cycle a context deadline equal to its interval, and recovers panics per cycle,
so a panicking cycle loses a tick instead of killing the job. The deadline is
cooperative: `runGuarded` calls the work synchronously, so a callback that
ignores its context blocks later cycles and shutdown. Every job and its
dependencies must honour the context. Use it for any new job.

`/health` and `/xrpc/_health` are pure liveness checks and the container
healthcheck target, so a Jetstream outage does not restart-loop the AppView.
Indexing health is `/health/consumers`. A consumer's name keys its persisted
cursor and dead letter rows, so it must not change between releases.

## Configuration

Server wiring reads configuration through `config.Load()` in
`internal/config`. Malformed values fail on the first error. The cross-field
and production-only checks then run together and report every problem in one
error, so a misconfigured deployment fails at startup rather than at first use
(for example, `SKIP_DID_WEB_VERIFICATION` is refused in production). Every
variable `Load` reads must also be in `loadedEnvVars` in
`internal/config/testing.go`, or production-mode config tests fail only under
`make ci`, whose runner inherits `.env.ci`.

Some packages read the environment themselves: `internal/observability`
(`OTEL_*`), `internal/core/imageproxy` (`IMAGE_PROXY_*`, called from `Load`,
which re-reads `IMAGE_PROXY_ENABLED` strictly), `internal/api/handlers/wellknown`,
`internal/notify/telegram`, and `internal/atproto/identity`, whose
`DefaultConfig` reads `PLC_DIRECTORY_URL` and falls back to
`https://plc.directory`. `posts.TrustedAggregatorDIDs` reads
`TRUSTED_AGGREGATOR_DIDS` (falling back to `KAGI_AGGREGATOR_DID`) once at wiring
for the acceptance decider and again on every post create.

## Migrations and static assets

Migrations are goose SQL files in `internal/db/migrations`, numbered
sequentially (`NNN_name.sql`; create with
`goose -dir internal/db/migrations -s create <name> sql`). They are embedded
through `migrations.FS` and applied by the server at startup, so the binary
does not depend on its working directory for them. Write both `Up` and `Down`.
Never edit a migration that has been applied anywhere; add a new one.

`internal/api/routes/web.go` still serves `/static/` from the relative path
`static`, so the process must start in a directory containing `static/`: the
repository root in dev, `/app` in the Docker image.

## Tests

Test tiers are chosen by build tag. T0 and T1 tests live in-package; `tests/`
holds only cross-cutting suites. See [TEST_ARCHITECTURE.md](TEST_ARCHITECTURE.md).
