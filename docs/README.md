# Documentation

These are implementation notes for the Coves AppView. Each doc holds what the code cannot tell you: invariants, the reasons behind decisions, contracts between components, and procedures. If the code and a doc disagree, the code is right and the doc needs fixing.

Start with the [project README](../README.md) and [CONTRIBUTING.md](../CONTRIBUTING.md).

## Working on the code

- [ARCHITECTURE.md](ARCHITECTURE.md): package layering, server startup, config loading, migrations.
- [LOCAL_DEVELOPMENT.md](LOCAL_DEVELOPMENT.md): the dev stack, ports, and local PLC/PDS identity.
- [TEST_ARCHITECTURE.md](TEST_ARCHITECTURE.md): test tiers, the testkit, and what `make ci` enforces.
- [LEXICON_PUBLISHING.md](LEXICON_PUBLISHING.md): publishing `social.coves.*` schemas. A test checks this runbook.
- [Lexicon schemas](../internal/atproto/lexicon/social/coves/) and [schema fixtures](../tests/lexicon-test-data/README.md).

## Subsystems

- [POSTS.md](POSTS.md): author-owned posts, community admission, visibility, comments, and the legacy post cutover.
- [FEEDS.md](FEEDS.md): timeline, Discover, community feeds, search, and Hot ranking.
- [COMMUNITIES.md](COMMUNITIES.md): community accounts, handles, and credentials.
- [AGGREGATORS.md](AGGREGATORS.md): how aggregators register, get authorized, and post.
- [MODERATION.md](MODERATION.md): server-admin removal, NSFW labels, and the modlog.
- [NOTIFICATIONS.md](NOTIFICATIONS.md): in-app notifications, upvote groups, seen state, and retention.
- [MEDIA.md](MEDIA.md): the image proxy, CSAM scanning, caching, and purges.

## Security

- [SECURITY_MODEL.md](SECURITY_MODEL.md): auth, write-path, and firehose trust rules.
- [SSRF_SECURITY.md](SSRF_SECURITY.md): rules for outbound requests.
- [CREDENTIAL_ENCRYPTION.md](CREDENTIAL_ENCRYPTION.md): how stored credentials are sealed, and key recovery.
- [SECURITY.md](../SECURITY.md): reporting a vulnerability.

## Operations

- [PRODUCTION_DEPLOY.md](PRODUCTION_DEPLOY.md): Compose projects, Caddy routing, and the frontend deploy.
- [PRODUCTION_BACKUPS.md](PRODUCTION_BACKUPS.md): backup timers and restore.
- Aggregators: [Kagi News](../aggregators/kagi-news/README.md), [Reddit Highlights](../aggregators/reddit-highlights/README.md), [Bluesky Topics](../aggregators/bluesky-topics/README.md), [account and API-key setup](../scripts/aggregator-setup/README.md).

## Plans

[plans/](plans/) holds designs for work that is not built yet. Delete a plan when it ships, after moving its lasting rules into the subsystem doc.

- [IMAGE_UPLOADS.md](plans/IMAGE_UPLOADS.md): user image uploads in posts and comments.
- [NOTIFICATIONS_CLIENTS.md](plans/NOTIFICATIONS_CLIENTS.md): web and mobile notification clients.
- [MODERATION_LABELS.md](plans/MODERATION_LABELS.md): federated moderation through signed labels.

Deferred bugs and feature requests go in the central backlog, not in this folder.
