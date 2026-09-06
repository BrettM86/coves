# Documentation

See the [project README](../README.md) for an overview and
[CONTRIBUTING.md](../CONTRIBUTING.md) to work on the backend.

## Development and reference

- [Test architecture](TEST_ARCHITECTURE.md) — test tiers, fixtures, and CI.
- [Project structure](../PROJECT_STRUCTURE.md) — packages and server startup.
- [Lexicon schemas](../internal/atproto/lexicon/social/coves/) — API and record definitions.
- [Schema fixtures](../tests/lexicon-test-data/README.md) — validation examples.
- [SSRF security model](SSRF_SECURITY.md) — rules for outbound requests.
- [Credential encryption](CREDENTIAL_ENCRYPTION.md) — storage, keys, and recovery.
- [Security policy](../SECURITY.md) — how to report a vulnerability.

## Local development

The [setup guide](LOCAL_DEVELOPMENT.md) needs updating. Check the
[Makefile](../Makefile), [Compose file](../docker-compose.dev.yml), and
[example environment](../.env.dev.example) for current configuration.

Development Postgres uses port **5435**, and the AppView subscribes through
`JETSTREAM_FEEDS`. Replace the environment template's placeholder values before
using it. Keep `OAUTH_SEAL_SECRET` and `ENCRYPTION_KEY` stable across restarts.
`make dev-up` starts the supporting services; `make run` starts the AppView
and requires `goose` for migrations.

## Aggregators

- [Kagi News](../aggregators/kagi-news/README.md)
- [Reddit Highlights](../aggregators/reddit-highlights/README.md)
- [Account and API-key setup scripts](../scripts/aggregator-setup/README.md)

## Maintainer guides

- [Lexicon publishing](LEXICON_PUBLISHING.md)
- [Frontend routing and deployment](FRONTEND_DEPLOY.md)

## Design background

[Author-owned posts](PRD_AUTHOR_OWNED_POSTS.md) explains why post content and
community acceptance live in separate repositories. It also includes migration
notes and proposed federation work.

Other PRDs, implementation reports, and dated audits remain in this directory
for historical context. They may describe replaced code or unimplemented plans;
use the source and tests to check current behavior.
