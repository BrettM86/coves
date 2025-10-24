# Kagi News RSS Aggregator

A Python-based RSS aggregator that posts Kagi News stories to Coves communities using rich text formatting.

## Overview

This aggregator:
- Fetches RSS feeds from Kagi News daily via CRON
- Parses HTML descriptions to extract structured content (highlights, perspectives, sources)
- Formats posts using Coves rich text with facets (bold, italic, links)
- Hot-links images from Kagi's proxy (no blob upload)
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
│   └── generate_did.py        # Helper to generate aggregator DID (TODO)
├── requirements.txt           # Python dependencies
├── config.example.yaml        # Example configuration
├── .env.example               # Environment variables template
├── crontab                    # CRON schedule
└── README.md
```

## Setup

### Prerequisites

- Python 3.11+
- python3-venv package (`apt install python3.12-venv`)

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
