# Coves Project Structure

This document provides an overview of the Coves project directory structure, following atProto architecture patterns.

**Legend:**
- 🔒 = Security-sensitive files

```
Coves/
├── CLAUDE.md                    # Project guidelines and architecture decisions
├── ATPROTO_GUIDE.md            # Comprehensive AT Protocol implementation guide
├── PROJECT_STRUCTURE.md        # This file - project structure overview
├── LICENSE                     # Project license
├── go.mod                      # Go module definition
├── go.sum                      # Go module checksums
│
├── cmd/                        # Application entrypoints
│   ├── server/                 # The AppView binary (see "Server startup" below)
│   ├── backfill-profiles/      # One-off maintenance: backfill actor profiles
│   ├── reindex-votes/          # One-off maintenance: rebuild vote counts
│   ├── tools/                  # generate-oauth-key
│   ├── ci-report/              # CI gate: a skip is a failure unless allowlisted
│   ├── contract-manifest/      # CI gate: every consumed collection has an e2e contract
│   ├── validate-lexicon/       # Lexicon schema validation
│   └── validate-live/          # Validation against a live instance
│
├── internal/                   # Private application code
│   ├── api/                    # HTTP layer
│   │   ├── handlers/           # XRPC request handlers, one package per domain
│   │   ├── middleware/ 🔒      # Auth, API keys, rate limiting
│   │   ├── routes/             # Route registration per domain
│   │   └── xrpc/               # Shared XRPC error response shape
│   ├── config/ 🔒              # Environment loading + production validation
│   ├── core/                   # Business logic and domain models
│   │   └── errors/             # Error types shared by every domain package
│   ├── atproto/                # atProto implementations
│   │   ├── identity/           # DID + handle resolution, with cache
│   │   ├── jetstream/          # Firehose consumers, cursors, dead letters
│   │   ├── lexicon/            # Generated lexicon types
│   │   ├── oauth/ 🔒           # atProto OAuth client, sealed session tokens
│   │   └── pds/                # PDS client (write-forward)
│   ├── db/
│   │   ├── postgres/           # AppView repositories (parameterized queries)
│   │   └── migrations/         # goose migrations, embedded into the binary
│   ├── observability/          # Optional OpenTelemetry tracing
│   ├── validation/             # Lexicon-level input validation
│   └── web/                    # Server-rendered pages (landing, delete account)
│
├── static/                     # Static web assets
├── scripts/                    # Development and deployment scripts
├── tests/                      # Only the cross-cutting tiers (see "Tests" below)
│   ├── testkit/                # The shared harness every tier imports
│   ├── e2e/                    # T2 pipeline contracts (//go:build e2e)
│   ├── live/                   # T3 opt-in tests against the public internet
│   ├── fixtures/               # Shared record/blob builders
│   ├── ci/                     # Skip allowlist and contract-manifest inputs
│   └── lexicon-test-data/      # Example records for lexicon validation
├── docs/                       # Additional documentation
└── aggregators/                # Aggregator bot examples
```

## Tests

Tiers are selected by build tag, never by directory or filename. T0 unit tests
(no tag) and T1 integration tests (`//go:build integration`) live **in-package**,
next to the code they test, so `internal/…` and `cmd/…` hold the bulk of the
suite. `tests/` keeps only what is genuinely cross-cutting.

`docs/TEST_ARCHITECTURE.md` is the canonical reference for the tiers, the make
targets, and the gates `make ci` enforces.

## Server startup

`cmd/server` is split by concern rather than being one long `main`:

| File | Responsibility |
|------|----------------|
| `main.go` | `run()` orchestration, serve loop, graceful shutdown |
| `wiring.go` | Builds every repository, service, and middleware in dependency order |
| `routes.go` | Router construction and route registration |
| `consumers.go` | Jetstream consumer wiring, feed topology, dead letter redriver |
| `database.go` | Migrations and the application connection pool |
| `httpserver.go` | HTTP listener with all timeouts set |
| `jobs.go` | Background jobs (OAuth cleanup, aggregator token refresh) |
| `health.go` | `/health`, `/xrpc/_health`, `/health/consumers` |
| `pds.go` | Instance PDS authentication |

Server wiring reads configuration through `internal/config`. `config.Load()`
applies defaults, rejects malformed values, and enforces the requirements that
differ between dev and production — so a misconfigured deployment fails at
startup with every problem listed at once, rather than at first use.

A few self-contained subsystems keep their own loaders rather than routing
through it: `internal/observability`, `internal/core/imageproxy`,
`internal/api/handlers/wellknown`, and `internal/core/posts` (the latter reads
`TRUSTED_AGGREGATOR_DIDS` / `KAGI_AGGREGATOR_DID` per call, which is worth
folding into `InstanceConfig` at some point).

Migrations are embedded into the binary (`internal/db/migrations/embed.go`), so
the container image no longer has to reproduce the repository layout around it.
Note the working directory still matters for static assets, which
`internal/api/routes/web.go` serves from a relative path.


## Development Guidelines

For detailed implementation guidelines, see [CLAUDE.md](./CLAUDE.md) and [ATPROTO_GUIDE.md](./ATPROTO_GUIDE.md).

1. **Start with Lexicons**: Define data schemas first
2. **Implement Core Domain**: Create models and interfaces
3. **Build Services**: Implement business logic
4. **Wire XRPC**: Connect handlers last