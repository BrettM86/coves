# Kagi News RSS Aggregator

A Python-based RSS aggregator that posts Kagi News stories to Coves communities using rich text formatting.

## Overview

This aggregator:
- Fetches RSS feeds from Kagi News daily via CRON
- Parses HTML descriptions to extract structured content (highlights, perspectives, sources)
- Formats posts using Coves rich text with facets (bold, italic, links)
- Thumbnails are automatically extracted by the server's unfurl service
- Posts to configured communities via XRPC

## Project Structure

```
aggregators/kagi-news/
├── src/
│   ├── models.py              # Data models (KagiStory, Perspective, etc.)
│   ├── rss_fetcher.py         # RSS feed fetching with retry logic
│   ├── html_parser.py         # Parse Kagi HTML to structured data
│   ├── richtext_formatter.py  # Format content with rich text facets (TODO)
│   ├── atproto_client.py      # ATProto authentication and operations (TODO)
│   ├── state_manager.py       # Deduplication state tracking (TODO)
│   ├── config.py              # Configuration loading (TODO)
│   └── main.py                # Entry point (TODO)
├── tests/
│   ├── test_rss_fetcher.py    # RSS fetcher tests ✓
│   ├── test_html_parser.py    # HTML parser tests ✓
│   └── fixtures/
│       ├── sample_rss_item.xml
│       └── world.xml
├── scripts/
│   └── setup.sh               # Automated Coves registration script
├── Dockerfile                 # Docker image definition
├── docker-compose.yml         # Docker Compose configuration
├── docker-entrypoint.sh       # Container entrypoint script
├── .dockerignore              # Docker build exclusions
├── requirements.txt           # Python dependencies
├── config.example.yaml        # Example configuration
├── .env.example               # Environment variables template
├── crontab                    # CRON schedule
└── README.md
```

## Registration with Coves

Before running the aggregator, you must register it with a Coves instance. This creates a DID for your aggregator and registers it with Coves.

### Quick Setup (Automated)

The automated setup script handles the entire registration process:

```bash
cd scripts
chmod +x setup.sh
./setup.sh
```

This will:
1. **Create a PDS account** for your aggregator (generates a DID)
2. **Generate `.well-known/atproto-did`** file for domain verification
3. **Pause for manual upload** - you'll upload the file to your web server
4. **Register with Coves** instance via XRPC
5. **Create service declaration** record (indexed by Jetstream)

**Manual step required:** During the process, you'll need to upload the `.well-known/atproto-did` file to your domain so it's accessible at `https://yourdomain.com/.well-known/atproto-did`.

After completion, you'll have a `kagi-aggregator-config.env` file with:
- Aggregator DID and credentials
- Access/refresh JWTs
- Service declaration URI

**Keep this file secure!** It contains your aggregator's credentials.

### Manual Setup (Step-by-step)

Alternatively, use the generic setup scripts from the main Coves repo for more control:

```bash
# From the Coves project root
cd scripts/aggregator-setup

# Follow the 4-step process
./1-create-pds-account.sh
./2-setup-wellknown.sh
./3-register-with-coves.sh
./4-create-service-declaration.sh
```

See [scripts/aggregator-setup/README.md](../../scripts/aggregator-setup/README.md) for detailed documentation on each step.

### What Happens During Registration?

1. **PDS Account Creation**: Your aggregator gets a `did:plc:...` identifier
2. **Domain Verification**: Proves you control your aggregator's domain
3. **Coves Registration**: Inserts your DID into the Coves instance's `users` table
4. **Service Declaration**: Creates a record that gets indexed into the `aggregators` table
5. **Ready for Authorization**: Community moderators can now authorize your aggregator

Once registered and authorized by a community, your aggregator can post content.

## Setup

### Prerequisites

- Python 3.11+
- python3-venv package (`apt install python3.12-venv`)
- **Completed registration** (see above)

### Installation

1. Create virtual environment:
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   ```

2. Install dependencies:
   ```bash
   pip install -r requirements.txt
   ```

3. Copy configuration templates:
   ```bash
   cp config.example.yaml config.yaml
   cp .env.example .env
   ```

4. Edit `config.yaml` to map RSS feeds to communities
5. Set environment variables in `.env` (aggregator DID and private key)

## Running Tests

```bash
# Activate virtual environment
source venv/bin/activate

# Run all tests
pytest -v

# Run specific test file
pytest tests/test_html_parser.py -v

# Run with coverage
pytest --cov=src --cov-report=html
```

## Deployment

### Docker Deployment (Recommended for Production)

The easiest way to deploy the Kagi aggregator is using Docker. The cron job runs inside the container automatically.

#### Prerequisites

- Docker and Docker Compose installed
- Completed registration (you have `.env` with credentials)
- `config.yaml` configured with your feed mappings

#### Quick Start

1. **Configure your environment:**
   ```bash
   # Copy and edit configuration
   cp config.example.yaml config.yaml
   cp .env.example .env

   # Edit .env with your aggregator credentials
   nano .env
   ```

2. **Start the aggregator:**
   ```bash
   docker compose up -d
   ```

3. **View logs:**
   ```bash
   docker compose logs -f
   ```

4. **Stop the aggregator:**
   ```bash
   docker compose down
   ```

#### Configuration

The `docker-compose.yml` file supports these environment variables:

- **`AGGREGATOR_HANDLE`** (required): Your aggregator's handle
- **`AGGREGATOR_PASSWORD`** (required): Your aggregator's password
- **`COVES_API_URL`** (optional): Override Coves API endpoint (defaults to `https://api.coves.social`)
- **`RUN_ON_STARTUP`** (optional): Set to `true` to run immediately on container start (useful for testing)

#### Testing the Setup

Run the aggregator immediately without waiting for cron:

```bash
# Run once and exit
docker compose run --rm kagi-aggregator python -m src.main

# Or set RUN_ON_STARTUP=true in .env and restart
docker compose restart
```

#### Production Deployment

For production, consider:

1. **Using Docker Secrets** for credentials:
   ```yaml
   secrets:
     aggregator_credentials:
       file: ./secrets/aggregator.env
   ```

2. **Setting up log rotation** (already configured in docker-compose.yml):
   - Max size: 10MB per file
   - Max files: 3

3. **Monitoring health checks:**
   ```bash
   docker inspect --format='{{.State.Health.Status}}' kagi-news-aggregator
   ```

4. **Auto-restart on failure** (already enabled with `restart: unless-stopped`)

#### Viewing Cron Logs

```bash
# Follow cron execution logs
docker compose logs -f kagi-aggregator

# View last 100 lines
docker compose logs --tail=100 kagi-aggregator
```

#### Updating the Aggregator

```bash
# Pull latest code
git pull

# Rebuild and restart
docker compose up -d --build
```

### Manual Deployment (Alternative)

If you prefer running without Docker, use the traditional approach:

1. **Install dependencies:**
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   pip install -r requirements.txt
   ```

2. **Configure crontab:**
   ```bash
   # Edit the crontab file with your paths
   # Then install it:
   crontab crontab
   ```

3. **Verify cron is running:**
   ```bash
   crontab -l
   ```

## Development Status

### ✅ Phase 1-2 Complete (Oct 24, 2025)
- [x] Project structure created
- [x] Data models defined (KagiStory, Perspective, Quote, Source)
- [x] RSS fetcher with retry logic and tests
- [x] HTML parser extracting all sections (summary, highlights, perspectives, sources, quote, image)
- [x] Test fixtures from real Kagi News feed

### 🚧 Next Steps (Phase 3-4)
- [ ] Rich text formatter (convert to Coves format with facets)
- [ ] State manager for deduplication
- [ ] Configuration loader
- [ ] ATProto client for post creation
- [ ] Main orchestration script
- [ ] End-to-end tests

## Configuration

Edit `config.yaml` to define feed-to-community mappings:

```yaml
coves_api_url: "https://api.coves.social"

feeds:
  - name: "World News"
    url: "https://news.kagi.com/world.xml"
    community_handle: "world-news.coves.social"
    enabled: true

  - name: "Tech News"
    url: "https://news.kagi.com/tech.xml"
    community_handle: "tech.coves.social"
    enabled: true
```

## Architecture

### Data Flow

```
Kagi RSS Feed
    ↓ (HTTP GET)
RSS Fetcher
    ↓ (feedparser)
Parsed RSS Items
    ↓ (for each item)
HTML Parser
    ↓ (BeautifulSoup)
Structured KagiStory
    ↓
Rich Text Formatter
    ↓ (with facets)
Post Record
    ↓ (XRPC)
Coves Community
```

### Rich Text Format

Posts use Coves rich text with UTF-8 byte-positioned facets:

```python
{
  "content": "Summary text...\n\nHighlights:\n• Point 1\n...",
  "facets": [
    {
      "index": {"byteStart": 20, "byteEnd": 31},
      "features": [{"$type": "social.coves.richtext.facet#bold"}]
    },
    {
      "index": {"byteStart": 50, "byteEnd": 75},
      "features": [{"$type": "social.coves.richtext.facet#link", "uri": "https://..."}]
    }
  ]
}
```

## License

See parent Coves project license.

## Related Documentation

- [PRD: Kagi News Aggregator](../../docs/aggregators/PRD_KAGI_NEWS_RSS.md)
- [PRD: Aggregator System](../../docs/aggregators/PRD_AGGREGATORS.md)
- [Coves Rich Text Lexicon](../../internal/atproto/lexicon/social/coves/richtext/README.md)
