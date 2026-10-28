# Kagi News aggregator

Posts Kagi News stories to Coves communities. Each run is a single `python -m src.main` that processes every enabled feed and exits. Cron runs it every two hours (`0 */2 * * *`) with no jitter.

The aggregator runs as a trusted DID on the AppView, so it needs no community authorization and may send `thumbnailUrl`. The server-side rules are in [docs/AGGREGATORS.md](../../docs/AGGREGATORS.md).

## What a run does

Each configured feed URL must end in `.xml`. The run fetches it and its `.json` sibling (same URL, `.json` suffix). RSS supplies the link, guid, pubDate, categories and title. JSON supplies the content: summary, highlights, perspectives, quote, sources and image.

`Aggregator._resolve_json_cluster` joins an RSS entry to its JSON cluster. The normalized title is the real join key, because it is the only value Kagi carries identically in both feeds. When the entry link ends in a bare number, cluster `number + 1` is tried first and accepted only if its title matches. Otherwise the run scans all clusters for a single title match. Kagi changed the link shape on 2026-07-30 (date prefix plus slug), which is why the number is never required. If no unposted entry in a feed resolves, the run logs an error naming possible Kagi title-format drift. That log line is the only alarm for a format change.

Feeds are isolated. An exception in one feed is logged and the run moves to the next.

Posts are built by `RichTextFormatter` with byte-offset facets. `embed.external.sources` is trimmed to the lexicon's 50-entry cap, keeping Kagi's leading (most relevant) articles; an over-long list would make the AppView reject the whole post. The story image goes in the top-level `thumbnailUrl`.

## Dedup

State lives in `data/state.json` (override with `STATE_FILE`), keyed by the configured feed URL. Per feed it keeps up to 100 posted GUIDs and drops any older than 30 days. Changing a feed's URL starts it with empty state, so every story in the feed looks new.

Semantic dedup runs after the GUID check when `dedup.semantic_enabled` is true, which is the default even with no `dedup` block. Claude Haiku compares the new candidates against stories this same feed posted in the last `lookback_days` (default 4) and flags a duplicate at `similarity_threshold` (default 0.8) or above. It never compares across feeds.

It fails closed. If the model call fails or returns something unusable (`SemanticDedupUnavailable`), the whole feed is deferred: nothing is posted, nothing is suppressed, and the same candidates come back next run. Suppressions are written to state before any post is attempted, so a failure while posting cannot lose them. Suppressed GUIDs sit in their own list with the same 100-entry and 30-day limits, and are not offered to the model again while they are kept.

A story whose post fails is not marked posted and is retried next run.

## Configuration

Copy `config.example.yaml` to `config.yaml`. It holds `coves_api_url`, the `feeds` list (`name`, `url`, `community_handle`, `enabled`), the `dedup` block and `log_level`. Use `news.kagi.com` feed URLs. Kagi moved its feeds there from `kite.kagi.com` in October 2025. Story permalinks inside the feed still point at `kite.kagi.com`, and that is expected.

| Variable | Required | Purpose |
|---|---|---|
| `COVES_API_KEY` | yes | `ckapi_` key from setup step 5 |
| `ANTHROPIC_API_KEY` | unless `dedup.semantic_enabled: false` | semantic dedup; each run fails without it (the container still starts) |
| `COVES_API_URL` | no | overrides `coves_api_url` in `config.yaml` |
| `CONFIG_PATH` | no | defaults to `config.yaml` |
| `STATE_FILE` | no | defaults to `data/state.json` |
| `RUN_ON_STARTUP` | no | `true` runs once when the container starts |

Under Docker Compose, `COVES_API_URL` is always set: `docker-compose.yml` defaults it to `https://api.coves.social`, which overrides `config.yaml`. Set it explicitly in `.env`.

`docker-entrypoint.sh` copies only `COVES_*`, `ANTHROPIC_*` and `PATH` into `/etc/environment`, which is all the cron run sees. `CONFIG_PATH`, `STATE_FILE` and any new variable apply to manual runs only until the whitelist is changed.

## License

Kagi News data is CC BY-NC: free for non-commercial use with attribution. Every post must credit Kagi News and link the story, and `RichTextFormatter.format_full` ends each post with a "Kagi News" link to the story permalink. Do not remove it. Commercial use needs a license from Kagi (support@kagi.com).

## Setup

Register the aggregator account and mint an API key with [scripts/aggregator-setup/README.md](../../scripts/aggregator-setup/README.md). Use the numbered scripts, not `scripts/setup.sh` here. That script creates the account with a generated `*.bsky.social` handle, so its registration call fails with `HandleMismatch` and it exits before the service declaration. A re-run cannot recover: it loads `kagi-aggregator-config.env`, whose `AGGREGATOR_`-prefixed names its later steps do not read. It also has no API key step.

The Dockerfile copies `config.yaml` into the image, so create it before building.

```bash
cp config.example.yaml config.yaml   # edit feeds and communities
cp .env.example .env                 # set COVES_API_KEY, ANTHROPIC_API_KEY, COVES_API_URL
docker compose up -d --build
docker compose run --rm kagi-aggregator python -m src.main   # one run now
```

For tests: `python3 -m venv venv && . venv/bin/activate && pip install -r requirements-dev.txt && pytest`.

## Operations

`/var/log/cron.log` is not on a volume, but the container tails it to stdout and stdout goes to the host journal, which survives recreation. `docker logs kagi-news-aggregator` shows only the current container; for runs before the last recreate use `journalctl CONTAINER_NAME=kagi-news-aggregator --since <time>`.

A manual `docker exec ... python -m src.main` proves the container environment, not cron's. Confirm authentication from a log line written by a real cron run. To reproduce cron's environment in a shell, use `set -a; . /etc/environment; set +a`.
