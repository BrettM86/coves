# Reddit Highlights Aggregator

Aggregates video highlights from Reddit subreddits (e.g., r/nba) and posts them to Coves communities.

## Features

- Fetches posts from Reddit via RSS (no API key required)
- Extracts streamable.com video links
- Posts to configured Coves communities with proper attribution
- Anti-detection jitter (randomized polling intervals)
- State tracking for deduplication
- Docker deployment with cron scheduler

## Quick Start

1. **Copy environment file:**
   ```bash
   cp .env.example .env
   ```

2. **Configure your Coves API key:**
   ```bash
   # Edit .env and set your API key
   COVES_API_KEY=ckapi_your_key_here
   ```

3. **Build and run:**
   ```bash
   docker-compose up -d
   ```

4. **View logs:**
   ```bash
   docker-compose logs -f
   ```

## Configuration

### config.yaml

```yaml
coves_api_url: "https://coves.social"

subreddits:
  - name: "nba"
    community_handle: "nba.coves.social"
    enabled: true

allowed_domains:
  - streamable.com
```

### Adding More Subreddits

1. Add entry to `config.yaml`:
   ```yaml
   - name: "soccer"
     community_handle: "soccer.coves.social"
     enabled: true
   ```

2. Authorize the aggregator for the new community in Coves

3. Restart the container:
   ```bash
   docker-compose restart
   ```

## Polling Schedule

- Cron runs every **10 minutes**
- Python script adds **0-10 minutes random jitter**
- Effective polling interval: **10-20 minutes** (varies each run)

This randomization helps avoid bot detection patterns.

## Development

### Setup

```bash
# Create virtual environment
python -m venv venv
source venv/bin/activate

# Install dependencies
pip install -r requirements.txt
```

### Run Tests

```bash
pytest
```

### Run Manually

```bash
# Set environment variables
export COVES_API_KEY=ckapi_your_key
export SKIP_JITTER=true  # Skip delay for testing

# Run
python -m src.main
```

## Architecture

```
src/
├── main.py           # Orchestration (CRON entry point)
├── rss_fetcher.py    # RSS feed fetching with retry
├── link_extractor.py # Streamable URL detection
├── coves_client.py   # Coves API client
├── state_manager.py  # Deduplication state tracking
├── config.py         # YAML config loader
└── models.py         # Data models
```

## Post Format

Posts are created with:
- **Title**: Reddit post title
- **Embed**: Streamable video link with metadata
- **Sources**: Link back to original Reddit post

Example embed:
```json
{
  "$type": "social.coves.embed.external",
  "external": {
    "uri": "https://streamable.com/abc123",
    "title": "LeBron with the chase-down block!",
    "description": "From r/nba",
    "sources": [
      {"uri": "https://reddit.com/...", "title": "r/nba", "domain": "reddit.com"}
    ]
  }
}
```
