# Bluesky Topics Aggregator

Cross-posts the top Bluesky posts about a topic (NBA, tennis, a TV show) into a
Coves community every hour. Candidates come from named Bluesky feeds and
lists, a Claude call decides which candidates are actually about the topic,
and the top few on-topic posts are posted as links to their `bsky.app`
permalinks with author attribution.

Same scaffold as `../reddit-highlights`: a Python cron job in a container with
a JSON state file for deduplication.

## How a run works

Every hour, for each enabled topic in `config.yaml`:

1. Fetch every configured feed and list from the public Bluesky AppView
   (`app.bsky.feed.getFeed`, `app.bsky.feed.getListFeed`), up to 5 pages each.
   A failing source is logged and skipped; the topic continues with the rest.
2. Drop reposts, pins, replies, posts outside the last `window_hours`, posts
   with adult or hidden moderation labels, authors who opted out of logged-out
   visibility (`!no-unauthenticated`), excluded handles, and posts under
   `min_likes`.
3. Rank by likes + reposts + quotes and keep the top `classify_candidates`.
4. Fetch each remaining author's bio once and ask the classifier
   (`classifier_model`, low effort, structured output) whether each post is
   primarily about the topic, and what story it belongs to. A post by an NBA
   reporter about the NFL is off-topic; that is the whole reason this step
   exists. Posts the aggregator already cross-posted in the window are sent
   along too, so their stories are keyed consistently.
5. Group posts by story. A story is represented by its highest-scored member
   that is on-topic with confidence at or above `min_confidence`; a story with
   no such member is dropped before the top-N cut and takes no slot. Take the
   top `max_posts_per_run` remaining stories. A story is skipped when any of
   its posts was already posted, by the aggregator or by a community member
   linking the same Bluesky post. Skipped stories are not backfilled by the
   next story down, so a quiet hour posts nothing.
6. Post the remaining representatives to the community, then record them in
   the state file and the story log.

Each topic is independent: a classifier or source failure in one topic never
blocks another. A classifier failure posts nothing for that topic in that run,
including when a story-log entry comes back without a verdict.

Story keys are only consistent within a single classifier call, so every
candidate plus the recent story-log entries go out in one request. When the
two together exceed the 200-post batch, the lowest-ranked candidates are
dropped so the call still fits and the story keys stay consistent.

The community check reads the community's recent posts through the public
`social.coves.communityFeed.getCommunity` endpoint and matches `bsky.app`
links by author (current handle or DID) and record key; a link that uses an
author's previous handle is not recognized. A lookup failure is logged at
ERROR and the run proceeds as if nothing had been posted there, except for a
4xx — a misconfigured `community_handle` — which skips the topic.

## Setup

1. Copy `config.example.yaml` to `config.yaml` and edit the topics. The
   Dockerfile copies `config.yaml` into the image, so it must exist at build
   time (it is gitignored).
2. Copy `.env.example` to `.env` and set `COVES_API_KEY` (the aggregator's
   Coves API key; authorize it for each community) and `ANTHROPIC_API_KEY`.
3. Dry-run first:

   ```bash
   DRY_RUN=1 RUN_ON_STARTUP=true docker compose up --build
   ```

   Dry run fetches and classifies, logs a ranked table of candidates with
   verdicts, and posts nothing. It ignores the state file and the story log,
   so it shows what a fresh run would pick; links already posted by community
   members are still checked, so `[posted]` markers can appear. Use it to
   hand-check the classifier's precision on a real day of candidates before
   the first live run.
4. Run for real:

   ```bash
   docker compose up -d --build
   docker compose logs -f
   ```

Cron runs every hour (`crontab`). `RUN_ON_STARTUP=true` also runs once at
container start.

## Tuning volume

Volume is set by how often the top `max_posts_per_run` stories in the window
change, not by the cadence. Measured on the two NBA feeds over one off-season
day (hourly runs, simulated with final engagement counts):

| window | top N | posts/day |
|--------|-------|-----------|
| 6h     | 3     | 13        |
| 6h     | 1     | 6         |
| 12h    | 2     | 12        |
| 24h    | 3     | 7         |

The code default is a 24-hour window with top 3 (omit `window_hours` and you
get 24h): the day's three biggest stories, refreshed hourly. A breaking post
is picked up once it outscores the day's #3 story. A short window with top 1
gives similar volume but can starve the #2 story of a busy hour, since it ages
out together with #1.

## Choosing the classifier model

The classifier uses the Anthropic Messages API with structured output. OpenRouter
serves that API for many vendors' models, so `ANTHROPIC_BASE_URL=https://openrouter.ai/api`
plus an OpenRouter key lets `classifier_model` be any OpenRouter model id.

Measured 2026-09-19 on 60 hand-labelled candidates from the Surf NBA feed (28 on-topic,
32 off-topic: WNBA, personal news, jokes), threshold 0.7, one topic per run:

| model                          | F1 (2 runs) | seconds/run | $/run    |
|--------------------------------|-------------|-------------|----------|
| openai/gpt-5.6-luna            | 1.00, 0.98  | 15          | ~0.005   |
| deepseek/deepseek-v4-flash     | 0.96, 0.98  | 47-65       | ~0.0006  |
| google/gemini-3.5-flash-lite   | 0.97, 0.98  | 10          | ~0.008   |
| anthropic/claude-haiku-4.5     | 0.96, 0.95  | 9           | ~0.016   |
| anthropic/claude-sonnet-5      | 0.96        | 14          | ~0.04    |
| openai/gpt-5.4-nano            | 0.82        | 8           | ~0.003   |

`z-ai/glm-5.3-flash`, `deepseek/deepseek-v4-pro` and `google/gemini-3.8-flash` did not
honour the JSON schema through OpenRouter's translation layer and fail with a
classification error. The default is `openai/gpt-5.6-luna`; `deepseek/deepseek-v4-flash`
is the budget choice if a slow run is acceptable. Re-run the comparison for a new topic
before trusting it there: 60 posts is a small sample.

## Finding feeds and lists

Any Bluesky feed generator or list works. Surf-published feeds are ordinary
feed generators (service `did:web:bsky.surf.social`); for example Surf's
"NBA Essentials" is
`at://did:plc:ygvuw3tg5gsy73wpggfz3oj4/app.bsky.feed.generator/nba-essential`.
Look feeds up with the public API:

```bash
curl -s 'https://public.api.bsky.app/xrpc/app.bsky.unspecced.getPopularFeedGenerators?query=nba&limit=10'
```

Feeds curated by account (starter packs, reporter lists) drift off-topic in
the off-season. That is expected; the classifier handles it.

## Environment

| Variable            | Required | Purpose                                              |
|---------------------|----------|------------------------------------------------------|
| `ANTHROPIC_API_KEY` | yes      | Classifier calls (an OpenRouter key when `ANTHROPIC_BASE_URL` is set) |
| `ANTHROPIC_BASE_URL`| no       | `https://openrouter.ai/api` to run the classifier through OpenRouter |
| `COVES_API_KEY`     | unless `DRY_RUN=1` | Posting to Coves                          |
| `COVES_API_URL`     | no       | Overrides `coves_api_url`                            |
| `BLUESKY_API_URL`   | no       | Overrides `bluesky_api_url`                          |
| `DRY_RUN`           | no       | `1` to classify and log without posting or state     |
| `CONFIG_PATH`       | no       | Default `config.yaml`                                |
| `STATE_FILE`        | no       | Default `data/state.json`; the story log lives beside it as `stories.json` |
| `RUN_ON_STARTUP`    | no       | `true` to run once when the container starts         |

## Development

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements-dev.txt
.venv/bin/python -m pytest -q
```

Tests are hermetic: HTTP through `responses`, the Anthropic client through a
fake (plus one test through the real SDK with a mock transport), Coves through
a fake client. `coves_client.py`, `state_manager.py`, and `uri_sanitizer.py`
are verbatim copies from `reddit-highlights`; `kagi-news` carries its own
variants of `coves_client.py` and `state_manager.py`.

## Not in scope

- `app.bsky.feed.searchPosts`: the public AppView returns 403 without an
  authenticated session, so keyword search is not a source yet.
- A Bluesky oEmbed provider in the AppView's unfurl service
  (`embed.bsky.app/oembed` exists); the aggregator supplies title, description
  and source attribution itself.
- Firehose ingestion or hosting a Bluesky feed generator.
