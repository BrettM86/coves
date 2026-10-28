# Reddit Highlights aggregator

Posts video highlights from subreddits (r/nba by default) to Coves communities. It reads each subreddit's public RSS feed (`https://www.reddit.com/r/<name>/.rss`), so it needs no Reddit API key. Only links on `allowed_domains` (default `streamable.com`) are posted. Server-side rules are in [docs/AGGREGATORS.md](../../docs/AGGREGATORS.md).

## Schedule

Cron starts a run every hour (`0 * * * *`). `src/main.py` first sleeps a random 0 to 600 seconds, so requests land 0 to 10 minutes after each hourly cron start and consecutive runs are 50 to 70 minutes apart. The jitter is there to avoid a fixed polling pattern against Reddit.

`SKIP_JITTER` set to `true`, `1` or `yes` skips the sleep. `RUN_ON_STARTUP=true` runs once at container start, always without jitter.

A run without `SKIP_JITTER` can sit silent for up to ten minutes and looks hung. Over SSH, run it from `/app` like this so the jitter is skipped and output is not buffered:

```bash
docker exec -it reddit-highlights-aggregator sh -c 'cd /app && SKIP_JITTER=1 python -u -m src.main'
```

A process showing 0s CPU and sleeping in `hrtimer_nanosleep` is in the jitter, not hung.

## What a run does

For each enabled subreddit, the run walks the feed in order and stops after `max_posts_per_run` (default 3) video entries. Entries without an allowed video link, and entries whose post fails, do not count. Already-posted entries do count, so a run looks only at the top N video entries and posts the ones it has not posted before. The limit is per subreddit.

State lives in `data/state.json` (override with `STATE_FILE`), keyed by subreddit name, holding up to 100 entry IDs per subreddit for up to 30 days. The entry ID is Reddit's `t3_` ID, falling back to the link. A failed post is not recorded and is retried next run. One failing subreddit does not stop the others.

## Post format

Each post has the Reddit title as its title, no body, and a `social.coves.embed.external` embed:

```json
{
  "$type": "social.coves.embed.external",
  "external": {
    "uri": "https://streamable.com/abc123",
    "title": "LeBron with the chase-down block!",
    "description": "From r/nba",
    "embedType": "video",
    "provider": "streamable",
    "domain": "streamable.com",
    "sources": [{"uri": "https://www.reddit.com/r/nba/comments/...", "title": "r/nba", "domain": "reddit.com"}]
  }
}
```

The aggregator sends no `thumbnailUrl`. The AppView fetches the thumbnail by unfurling the Streamable link.

## Configuration

Copy `config.example.yaml` to `config.yaml`. It holds `coves_api_url`, the `subreddits` list (`name`, `community_handle`, `enabled`), `allowed_domains`, `max_posts_per_run` and `log_level`. Subreddit names are restricted to letters, digits, `_` and `-`.

| Variable | Required | Purpose |
|---|---|---|
| `COVES_API_KEY` | yes | `ckapi_` key from [setup step 5](../../scripts/aggregator-setup/README.md) |
| `COVES_API_URL` | no | overrides `coves_api_url`; Compose defaults it to `https://coves.social` |
| `SKIP_JITTER` | no | skip the startup sleep |
| `RUN_ON_STARTUP` | no | run once at container start |
| `CONFIG_PATH`, `STATE_FILE` | no | default `config.yaml`, `data/state.json` |

`docker-entrypoint.sh` copies only `COVES_*`, `SKIP_*` and `PATH` into `/etc/environment`, which is all the cron run sees. `CONFIG_PATH` and `STATE_FILE` reach manual runs only.

Adding a subreddit means a new `subreddits` entry and a container restart. If the aggregator's DID is not trusted on the instance, the target community also needs an authorization record.

## Running

The Dockerfile copies `config.yaml` into the image, so create it before building.

```bash
cp config.example.yaml config.yaml
cp .env.example .env           # set COVES_API_KEY
docker compose up -d --build
docker compose logs -f
```

`/var/log/cron.log` is not on a volume, but the container tails it to stdout and stdout goes to the host journal, which survives recreation. `docker logs` shows only the current container; for runs before the last recreate use `journalctl CONTAINER_NAME=reddit-highlights-aggregator --since <time>`. The `Using API key authentication` log line appears after the jitter and shows the run reached authentication, not that it finished.

For tests: `python -m venv venv && . venv/bin/activate && pip install -r requirements-dev.txt && pytest`.
