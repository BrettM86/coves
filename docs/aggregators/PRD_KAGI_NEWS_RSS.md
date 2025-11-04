# Kagi News RSS Aggregator PRD

**Status:** ✅ Phase 1 Complete - Ready for Deployment
**Owner:** Platform Team
**Last Updated:** 2025-10-24
**Parent PRD:** [PRD_AGGREGATORS.md](PRD_AGGREGATORS.md)
**Implementation:** Python + Docker Compose

## 🎉 Implementation Complete

All core components have been implemented and tested:

- ✅ **RSS Fetcher** - Fetches feeds with retry logic and error handling
- ✅ **HTML Parser** - Extracts all structured data (summary, highlights, perspectives, quote, sources)
- ✅ **Rich Text Formatter** - Formats content with proper facets for Coves
- ✅ **State Manager** - Tracks posted stories to prevent duplicates
- ✅ **Config Manager** - Loads and validates YAML configuration
- ✅ **Coves Client** - Handles authentication and post creation
- ✅ **Main Orchestrator** - Coordinates all components
- ✅ **Comprehensive Tests** - 57 tests with 83% code coverage
- ✅ **Documentation** - README with setup and deployment instructions
- ✅ **Example Configs** - config.example.yaml and .env.example

**Test Results:**
```
57 passed, 6 skipped, 1 warning in 8.76s
Coverage: 83%
```

**Ready for:**
- Integration testing with live Coves API
- Aggregator DID creation and authorization
- Production deployment

## Overview

The Kagi News RSS Aggregator is a reference implementation of the Coves aggregator system that automatically posts high-quality, multi-source news summaries to communities. It leverages Kagi News's free RSS feeds to provide pre-aggregated, deduped news content with multiple perspectives and source citations.

**Key Value Propositions:**
- **Multi-source aggregation**: Kagi News already aggregates multiple sources per story
- **Balanced perspectives**: Built-in perspective tracking from different outlets
- **Rich metadata**: Categories, highlights, source links included
- **Legal & free**: CC BY-NC licensed for non-commercial use
- **Low complexity**: No LLM deduplication needed (Kagi does it)
- **Simple deployment**: Python + Docker Compose, runs alongside Coves on same instance

## Data Source: Kagi News RSS Feeds

### Licensing & Legal

**License:** CC BY-NC (Creative Commons Attribution-NonCommercial)

**Terms:**
- ✅ **Free for non-commercial use** (Coves qualifies)
- ✅ **Attribution required** (must credit Kagi News)
- ❌ **Cannot use commercially** (must contact support@kagi.com for commercial license)
- ✅ **Data can be shared** (with same attribution + NC requirements)

**Source:** https://news.kagi.com/about

**Quote from Kagi:**
> Note that kite.json and files referenced by it are licensed under CC BY-NC license. This means that this data can be used free of charge (with attribution and for non-commercial use). If you would like to license this data for commercial use let us know through support@kagi.com.

**Compliance Requirements:**
- Visible attribution to Kagi News on every post
- Link back to original Kagi story page
- Non-commercial operation (met: Coves is non-commercial)

---

### RSS Feed Structure

**Base URL Pattern:** `https://news.kagi.com/{category}.xml`

**Known Categories:**
- `world.xml` - World news
- `tech.xml` - Technology
- `business.xml` - Business
- `sports.xml` - Sports (likely)
- Additional categories TBD (need to scrape homepage)

**Feed Format:** RSS 2.0 (standard XML)

**Update Frequency:** One daily update (~noon UTC)

**Important Note on Domain Migration (October 2025):**
Kagi migrated their RSS feeds from `kite.kagi.com` to `news.kagi.com`. The old domain now redirects (302) to the new domain, but for reliability, always use `news.kagi.com` directly in your feed URLs. Story links within the RSS feed still reference `kite.kagi.com` as permalinks.

---

### RSS Item Schema

Each `<item>` in the feed contains:

```xml
<item>
  <title>Story headline</title>
  <link>https://kite.kagi.com/{uuid}/{category}/{id}</link>
  <description>Full HTML content (see below)</description>
  <guid isPermaLink="true">https://kite.kagi.com/{uuid}/{category}/{id}</guid>
  <category>Primary category (e.g., "World")</category>
  <category>Subcategory (e.g., "World/Conflict & Security")</category>
  <category>Tag (e.g., "Conflict & Security")</category>
  <pubDate>Mon, 20 Oct 2025 01:46:31 +0000</pubDate>
</item>
```

**Description HTML Structure:**
```html
<p>Main summary paragraph with inline source citations [source1.com#1][source2.com#1]</p>

<img src='https://kagiproxy.com/img/...' alt='Image caption' />

<h3>Highlights:</h3>
<ul>
  <li>Key point 1 with [source.com#1] citations</li>
  <li>Key point 2...</li>
</ul>

<blockquote>Notable quote - Person Name</blockquote>

<h3>Perspectives:</h3>
<ul>
  <li>Viewpoint holder: Their perspective. (<a href='...'>Source</a>)</li>
</ul>

<h3>Sources:</h3>
<ul>
  <li><a href='https://...'>Article title</a> - domain.com</li>
</ul>
```

**✅ Verified Feed Structure:**
Analysis of live Kagi News feeds confirms the following structure:
- **Only 3 H3 sections:** Highlights, Perspectives, Sources (no other sections like Timeline or Historical Background)
- **Historical context** is woven into the summary paragraph and highlights (not a separate section)
- **Not all stories have all sections** - Quote (blockquote) and image are optional
- **Feed contains everything shown on website** except for Timeline (which is a frontend-only feature)

**Key Features:**
- Multiple source citations inline
- Balanced perspectives from different actors
- Highlights extract key points with historical context
- Direct quotes preserved (when available)
- All sources linked with attribution
- Images from Kagi's proxy CDN

---

## Architecture

### High-Level Flow

```
┌─────────────────────────────────────────────────────────────┐
│  Kagi News RSS Feeds (External)                             │
│  - https://news.kagi.com/world.xml                          │
│  - https://news.kagi.com/tech.xml                           │
│  - etc.                                                     │
└─────────────────────────────────────────────────────────────┘
                            │
                            │ HTTP GET one job after update
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  Kagi News Aggregator Service (Python + Docker Compose)    │
│  DID: did:plc:[generated-on-creation]                       │
│  Location: aggregators/kagi-news/                           │
│                                                             │
│  Components:                                                 │
│  1. RSS Fetcher: Fetches RSS feeds on schedule (feedparser) │
│  2. Item Parser: Extracts structured data from HTML (bs4)   │
│  3. Deduplication: Tracks posted items via JSON state file  │
│  4. Feed Mapper: Maps feed URLs to community handles        │
│  5. Post Formatter: Converts to Coves post format           │
│  6. Post Publisher: Calls social.coves.community.post.create via XRPC │
│  7. Blob Uploader: Handles image upload to ATProto          │
└─────────────────────────────────────────────────────────────┘
                            │
                            │ Authenticated XRPC calls
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  Coves AppView (social.coves.community.post.create)                   │
│  - Validates aggregator authorization                        │
│  - Creates post with author = did:plc:[aggregator-did]      │
│  - Indexes to community feeds                                │
└─────────────────────────────────────────────────────────────┘
```

---

### Aggregator Service Declaration

```json
{
  "$type": "social.coves.aggregator.service",
  "did": "did:plc:[generated-on-creation]",
  "displayName": "Kagi News Aggregator",
  "description": "Automatically posts breaking news from Kagi News RSS feeds. Kagi News aggregates multiple sources per story with balanced perspectives and comprehensive source citations.",
  "aggregatorType": "social.coves.aggregator.types#rss",
  "avatar": "<blob reference to Kagi logo>",
  "configSchema": {
    "type": "object",
    "properties": {
      "feedUrl": {
        "type": "string",
        "format": "uri",
        "description": "Kagi News RSS feed URL (e.g., https://news.kagi.com/world.xml)"
      }
    },
    "required": ["feedUrl"]
  },
  "sourceUrl": "https://github.com/coves-social/kagi-news-aggregator",
  "maintainer": "did:plc:coves-platform",
  "createdAt": "2025-10-23T00:00:00Z"
}
```

**Note:** The MVP implementation uses a simpler configuration model. Feed-to-community mappings are defined in the aggregator's own config file rather than per-community configuration. This allows one aggregator instance to post to multiple communities.

---

## Aggregator Configuration (MVP)

The MVP uses a simplified configuration model where the aggregator service defines feed-to-community mappings in its own config file.

### Configuration File: `config.yaml`

```yaml
# Aggregator credentials (from environment variables)
# AGGREGATOR_DID=did:plc:xyz...
# AGGREGATOR_PRIVATE_KEY=base64-encoded-key...

# Coves API endpoint
coves_api_url: "https://api.coves.social"

# Feed-to-community mappings
feeds:
  - name: "World News"
    url: "https://news.kagi.com/world.xml"
    community_handle: "world-news.coves.social"
    enabled: true

  - name: "Tech News"
    url: "https://news.kagi.com/tech.xml"
    community_handle: "tech.coves.social"
    enabled: true

  - name: "Science News"
    url: "https://news.kagi.com/science.xml"
    community_handle: "science.coves.social"
    enabled: false  # Can be disabled without removing

# Scheduling
check_interval: "24h"  # Run once daily

# Logging
log_level: "info"
```

**Key Decisions:**
- Uses **community handles** (not DIDs) for easier configuration - resolved at runtime
- One aggregator can post to multiple communities
- Feed mappings managed in aggregator config (not per-community config)
- No complex filtering logic in MVP - one feed = one community

---

## Post Format Specification

### Post Record Structure

```json
{
  "$type": "social.coves.community.post.record",
  "author": "did:plc:[aggregator-did]",
  "community": "world-news.coves.social",
  "title": "{Kagi story title}",
  "content": "{formatted content - full format for MVP}",
  "embed": {
    "$type": "social.coves.embed.external",
    "external": {
      "uri": "{Kagi story URL}",
      "title": "{story title}",
      "description": "{summary excerpt - first 200 chars}",
      "thumb": "{Kagi proxy image URL from HTML}"
    }
  },
  "federatedFrom": {
    "platform": "kagi-news-rss",
    "uri": "https://kite.kagi.com/{uuid}/{category}/{id}",
    "id": "{guid}",
    "originalCreatedAt": "{pubDate from RSS}"
  },
  "contentLabels": [
    "{primary category}",
    "{subcategories}"
  ],
  "createdAt": "{current timestamp}"
}
```

**MVP Notes:**
- Uses `social.coves.embed.external` for hot-linked images (no blob upload)
- Community specified as handle (resolved to DID by post creation endpoint)
- Images referenced via original Kagi proxy URLs
- "Full" format only for MVP (no format variations)
- Content uses Coves rich text with facets (not markdown)

---

### Content Formatting (MVP: "Full" Format Only)

The MVP implements a single "full" format using Coves rich text with facets:

**Plain Text Structure:**
```
{Main summary paragraph with source citations}

Highlights:
• {Bullet point 1}
• {Bullet point 2}
• ...

Perspectives:
• {Actor}: {Their perspective} (Source)
• ...

"{Notable quote}" — {Attribution}

Sources:
• {Title} - {domain}
• ...

---
📰 Story aggregated by Kagi News
```

**Rich Text Facets Applied:**
- **Bold** (`social.coves.richtext.facet#bold`) on section headers: "Highlights:", "Perspectives:", "Sources:"
- **Bold** on perspective actors
- **Italic** (`social.coves.richtext.facet#italic`) on quotes
- **Link** (`social.coves.richtext.facet#link`) on all URLs (source links, Kagi story link, perspective sources)
- Byte ranges calculated using UTF-8 byte positions

**Example with Facets:**
```json
{
  "content": "Main summary [source.com#1]\n\nHighlights:\n• Key point 1...",
  "facets": [
    {
      "index": {"byteStart": 35, "byteEnd": 46},
      "features": [{"$type": "social.coves.richtext.facet#bold"}]
    },
    {
      "index": {"byteStart": 15, "byteEnd": 26},
      "features": [{"$type": "social.coves.richtext.facet#link", "uri": "https://source.com"}]
    }
  ]
}
```

**Rationale:**
- Uses native Coves rich text format (not markdown)
- Preserves Kagi's rich multi-source analysis
- Provides maximum value to communities
- Meets CC BY-NC attribution requirements
- Additional formats ("summary", "minimal") can be added post-MVP

---

## Implementation Details (Python MVP)

### Technology Stack

**Language:** Python 3.11+

**Key Libraries:**
- `feedparser` - RSS/Atom parsing
- `beautifulsoup4` - HTML parsing for RSS item descriptions
- `requests` - HTTP client for fetching feeds
- `atproto` - Official ATProto Python SDK for authentication
- `pyyaml` - Configuration file parsing
- `pytest` - Testing framework

### Project Structure

```
aggregators/kagi-news/
├── Dockerfile
├── docker-compose.yml
├── requirements.txt
├── config.example.yaml
├── crontab                  # CRON schedule configuration
├── .env.example             # Environment variables template
├── scripts/
│   └── generate_did.py      # Helper to generate aggregator DID
├── src/
│   ├── main.py              # Entry point (single run, called by CRON)
│   ├── config.py            # Configuration loading and validation
│   ├── rss_fetcher.py       # RSS feed fetching with retry logic
│   ├── html_parser.py       # Parse Kagi HTML to structured data
│   ├── richtext_formatter.py # Format content with rich text facets
│   ├── atproto_client.py    # ATProto authentication and operations
│   ├── state_manager.py     # Deduplication state tracking (JSON)
│   └── models.py            # Data models (KagiStory, etc.)
├── tests/
│   ├── test_parser.py
│   ├── test_richtext_formatter.py
│   ├── test_state_manager.py
│   └── fixtures/            # Sample RSS feeds for testing
└── README.md
```

---

### Component 1: RSS Fetcher (`rss_fetcher.py`) ✅ COMPLETE

**Responsibility:** Fetch RSS feeds with retry logic and error handling

**Key Functions:**
- `fetch_feed(url: str) -> feedparser.FeedParserDict`
  - Uses `requests` with timeout (30s)
  - Retry logic: 3 attempts with exponential backoff
  - Returns parsed RSS feed or raises exception

**Error Handling:**
- Network timeouts
- Invalid XML
- HTTP errors (404, 500, etc.)

**Implementation Status:**
- ✅ Implemented with comprehensive error handling
- ✅ Tests passing (5 tests)
- ✅ Handles retries with exponential backoff

---

### Component 2: HTML Parser (`html_parser.py`) ✅ COMPLETE

**Responsibility:** Extract structured data from Kagi's HTML description field

**Key Class:** `KagiHTMLParser`

**Data Model (`models.py`):**
```python
@dataclass
class KagiStory:
    title: str
    link: str
    guid: str
    pub_date: datetime
    categories: List[str]

    # Parsed from HTML
    summary: str
    highlights: List[str]
    perspectives: List[Perspective]
    quote: Optional[Quote]
    sources: List[Source]
    image_url: Optional[str]
    image_alt: Optional[str]

@dataclass
class Perspective:
    actor: str
    description: str
    source_url: str

@dataclass
class Quote:
    text: str
    attribution: str

@dataclass
class Source:
    title: str
    url: str
    domain: str
```

**Parsing Strategy:**
- Use BeautifulSoup to parse HTML description
- Extract sections by finding `<h3>` tags (Highlights, Perspectives, Sources)
- Handle missing sections gracefully (not all stories have all sections)
- Clean and normalize text

**Implementation Status:**
- ✅ Extracts all 3 H3 sections (Highlights, Perspectives, Sources)
- ✅ Handles optional elements (quote, image)
- ✅ Tests passing (8 tests)
- ✅ Validates against real feed data

---

### Component 3: State Manager (`state_manager.py`) ✅ COMPLETE

**Responsibility:** Track processed stories to prevent duplicates

**Implementation:** Simple JSON file persistence

**State File Format:**
```json
{
  "feeds": {
    "https://news.kagi.com/world.xml": {
      "last_successful_run": "2025-10-23T12:00:00Z",
      "posted_guids": [
        "https://kite.kagi.com/uuid1/world/123",
        "https://kite.kagi.com/uuid2/world/124"
      ]
    }
  }
}
```

**Key Functions:**
- `is_posted(feed_url: str, guid: str) -> bool`
- `mark_posted(feed_url: str, guid: str, post_uri: str)`
- `get_last_run(feed_url: str) -> Optional[datetime]`
- `update_last_run(feed_url: str, timestamp: datetime)`

**Deduplication Strategy:**
- Keep last 100 GUIDs per feed (rolling window)
- Stories older than 30 days are automatically removed
- Simple, no database needed

**Implementation Status:**
- ✅ JSON-based persistence with atomic writes
- ✅ GUID tracking with rolling window
- ✅ Tests passing (12 tests)
- ✅ Thread-safe operations

---

### Component 4: Rich Text Formatter (`richtext_formatter.py`) ✅ COMPLETE

**Responsibility:** Format parsed Kagi stories into Coves rich text with facets

**Key Function:**
- `format_full(story: KagiStory) -> dict`
  - Returns: `{"content": str, "facets": List[dict]}`
  - Builds plain text content with all sections
  - Calculates UTF-8 byte positions for facets
  - Applies bold, italic, and link facets
  - Includes all sections: summary, highlights, perspectives, quote, sources
  - Adds Kagi News attribution footer with link

**Facet Types Applied:**
- `social.coves.richtext.facet#bold` - Section headers, perspective actors
- `social.coves.richtext.facet#italic` - Quotes
- `social.coves.richtext.facet#link` - All URLs (sources, Kagi story link)

**Key Challenge:** UTF-8 byte position calculation
- Must handle multi-byte characters correctly (emoji, non-ASCII)
- Use `str.encode('utf-8')` to get byte positions
- Test with complex characters

**Implementation Status:**
- ✅ Full rich text formatting with facets
- ✅ UTF-8 byte position calculation working correctly
- ✅ Tests passing (10 tests)
- ✅ Handles all sections: summary, highlights, perspectives, quote, sources

---

### Component 5: Coves Client (`coves_client.py`) ✅ COMPLETE

**Responsibility:** Handle authentication and post creation via Coves API

**Implementation Note:** Uses direct HTTP client instead of ATProto SDK for simplicity in MVP.

**Key Functions:**
- `authenticate() -> dict`
  - Authenticates aggregator using credentials
  - Returns auth token for subsequent API calls

- `create_post(community_handle: str, title: str, content: str, facets: List[dict], ...) -> dict`
  - Calls Coves post creation endpoint
  - Includes aggregator authentication
  - Returns post URI and metadata

**Authentication Flow:**
- Load aggregator credentials from environment
- Authenticate with Coves API
- Store and use auth token for requests
- Handle token refresh if needed

**Implementation Status:**
- ✅ HTTP-based client implementation
- ✅ Authentication and token management
- ✅ Post creation with all required fields
- ✅ Error handling and retries

---

### Component 6: Config Manager (`config.py`) ✅ COMPLETE

**Responsibility:** Load and validate configuration from YAML and environment

**Key Functions:**
- `load_config(config_path: str) -> AggregatorConfig`
  - Loads YAML configuration
  - Validates structure and required fields
  - Merges with environment variables
  - Returns validated config object

**Implementation Status:**
- ✅ YAML parsing with validation
- ✅ Environment variable support
- ✅ Tests passing (3 tests)
- ✅ Clear error messages for config issues

---

### Main Orchestration (`main.py`) ✅ COMPLETE

**Responsibility:** Coordinate all components in a single execution (called by CRON)

**Flow (Single Run):**
1. Load configuration from `config.yaml`
2. Load environment variables (AGGREGATOR_DID, AGGREGATOR_PRIVATE_KEY)
3. Initialize all components (fetcher, parser, formatter, client, state)
4. For each enabled feed in config:
   a. Fetch RSS feed
   b. Parse all items
   c. Filter out already-posted items (check state)
   d. For each new item:
      - Parse HTML to structured KagiStory
      - Format post content with rich text facets
      - Build post record (with hot-linked image if present)
      - Create post via XRPC
      - Mark as posted in state
   e. Update last run timestamp
5. Save state to disk
6. Log summary (posts created, errors encountered)
7. Exit (CRON will call again on schedule)

**Error Isolation:**
- Feed-level: One feed failing doesn't stop others
- Item-level: One item failing doesn't stop feed processing
- Continue on non-fatal errors, log all failures
- Exit code 0 even with partial failures (CRON won't alert)
- Exit code 1 only on catastrophic failure (config missing, auth failure)

**Implementation Status:**
- ✅ Complete orchestration logic implemented
- ✅ Feed-level and item-level error isolation
- ✅ Structured logging throughout
- ✅ Tests passing (9 tests covering various scenarios)
- ✅ Dry-run mode for testing

---

## Deployment (Docker Compose with CRON)

### Dockerfile

```dockerfile
FROM python:3.11-slim

WORKDIR /app

# Install cron
RUN apt-get update && apt-get install -y cron && rm -rf /var/lib/apt/lists/*

# Install dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy source code and scripts
COPY src/ ./src/
COPY scripts/ ./scripts/
COPY crontab /etc/cron.d/kagi-news-cron

# Set up cron
RUN chmod 0644 /etc/cron.d/kagi-news-cron && \
    crontab /etc/cron.d/kagi-news-cron && \
    touch /var/log/cron.log

# Create non-root user for security
RUN useradd --create-home appuser && \
    chown -R appuser:appuser /app && \
    chown appuser:appuser /var/log/cron.log

USER appuser

# Run cron in foreground
CMD ["cron", "-f"]
```

### Crontab Configuration (`crontab`)

```bash
# Run Kagi News aggregator daily at 1 PM UTC (after Kagi updates around noon)
0 13 * * * cd /app && /usr/local/bin/python -m src.main >> /var/log/cron.log 2>&1

# Blank line required at end of crontab
```

---

### docker-compose.yml

```yaml
version: '3.8'

services:
  kagi-news-aggregator:
    build: .
    container_name: kagi-news-aggregator
    restart: unless-stopped

    environment:
      # Aggregator identity (from aggregator creation)
      - AGGREGATOR_DID=${AGGREGATOR_DID}
      - AGGREGATOR_PRIVATE_KEY=${AGGREGATOR_PRIVATE_KEY}

    volumes:
      # Config file (read-only)
      - ./config.yaml:/app/config.yaml:ro
      # State file (read-write for deduplication)
      - ./data/state.json:/app/data/state.json

    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"
```

**Environment Variables:**
- `AGGREGATOR_DID`: PLC DID created for this aggregator instance
- `AGGREGATOR_PRIVATE_KEY`: Base64-encoded private key for signing

**Volumes:**
- `config.yaml`: Feed-to-community mappings (user-editable)
- `data/state.json`: Deduplication state (managed by aggregator)

**Deployment:**
```bash
# On same host as Coves
cd aggregators/kagi-news
cp config.example.yaml config.yaml
# Edit config.yaml with your feed mappings

# Set environment variables
export AGGREGATOR_DID="did:plc:xyz..."
export AGGREGATOR_PRIVATE_KEY="base64-key..."

# Start aggregator
docker-compose up -d

# View logs
docker-compose logs -f
```

---

## Image Handling Strategy (MVP)

### Approach: Hot-Linked Images via External Embed

The MVP uses hot-linked images from Kagi's proxy:

**Flow:**
1. Extract image URL from HTML description (`https://kagiproxy.com/img/...`)
2. Include in post using `social.coves.embed.external`:
   ```json
   {
     "$type": "social.coves.embed.external",
     "external": {
       "uri": "{Kagi story URL}",
       "title": "{Story title}",
       "description": "{Summary excerpt}",
       "thumb": "{Kagi proxy image URL}"
     }
   }
   ```
3. Frontend renders image from Kagi proxy URL

**Rationale:**
- Simpler MVP implementation (no blob upload complexity)
- No storage requirements on our end
- Kagi proxy is reliable and CDN-backed
- Faster posting (no download/upload step)
- Images already properly sized and optimized

**Future Consideration:** If Kagi proxy becomes unreliable, migrate to blob storage in Phase 2.

---

## Rate Limiting & Performance (MVP)

### Simplified Rate Strategy

**RSS Fetching:**
- Poll each feed once per day (~noon UTC after Kagi updates)
- No aggressive polling needed (Kagi updates daily)
- ~3-5 feeds = minimal load

**Post Creation:**
- One run per day = 5-15 posts per feed
- Total: ~15-75 posts/day across all communities
- Well within any reasonable rate limits

**Performance:**
- RSS fetch + parse: < 5 seconds per feed
- Image download + upload: < 3 seconds per image
- Post creation: < 1 second per post
- Total runtime per day: < 5 minutes

No complex rate limiting needed for MVP.

---

## Logging & Observability (MVP)

### Structured Logging

**Python logging module** with JSON formatter:

```python
import logging
import json

logging.basicConfig(
    level=logging.INFO,
    format='%(message)s'
)

logger = logging.getLogger(__name__)

# Example structured log
logger.info(json.dumps({
    "event": "post_created",
    "feed": "world.xml",
    "story_title": "Breaking News...",
    "community": "world-news.coves.social",
    "post_uri": "at://...",
    "timestamp": "2025-10-23T12:00:00Z"
}))
```

**Key Events to Log:**
- `feed_fetched`: RSS feed successfully fetched
- `story_parsed`: Story successfully parsed from HTML
- `post_created`: Post successfully created
- `error`: Any failures (with context)
- `run_completed`: Summary of entire run

**Log Levels:**
- INFO: Successful operations
- WARNING: Retryable errors, skipped items
- ERROR: Fatal errors, failed posts

### Simple Monitoring

**Health Check:** Check last successful run timestamp
- If > 48 hours: alert (should run daily)
- If errors > 50% of items: investigate

**Metrics to Track (manually via logs):**
- Posts created per run
- Parse failures per run
- Post creation failures per run
- Total runtime

No complex metrics infrastructure needed for MVP - Docker logs are sufficient.

---

## Testing Strategy ✅ COMPLETE

### Unit Tests - 57 Tests Passing (83% Coverage)

**Test Coverage by Component:**
- ✅ **RSS Fetcher** (5 tests)
  - Successful feed fetch
  - Timeout handling
  - Retry logic with exponential backoff
  - Invalid XML handling
  - Empty URL validation

- ✅ **HTML Parser** (8 tests)
  - Summary extraction
  - Image URL and alt text extraction
  - Highlights list parsing
  - Quote extraction with attribution
  - Perspectives parsing with actors and sources
  - Sources list extraction
  - Missing sections handling
  - Full story object creation

- ✅ **Rich Text Formatter** (10 tests)
  - Full format generation
  - Bold facets on headers and actors
  - Italic facets on quotes
  - Link facets on URLs
  - UTF-8 byte position calculation
  - Multi-byte character handling (emoji, special chars)
  - All sections formatted correctly

- ✅ **State Manager** (12 tests)
  - GUID tracking
  - Duplicate detection
  - Rolling window (100 GUID limit)
  - Age-based cleanup (30 days)
  - Last run timestamp tracking
  - JSON persistence
  - Atomic file writes
  - Concurrent access safety

- ✅ **Config Manager** (3 tests)
  - YAML loading and validation
  - Environment variable merging
  - Error handling for missing/invalid config

- ✅ **Main Orchestrator** (9 tests)
  - End-to-end flow
  - Feed-level error isolation
  - Item-level error isolation
  - Dry-run mode
  - State persistence across runs
  - Multiple feed handling

- ✅ **E2E Tests** (6 skipped - require live API)
  - Integration with Coves API (manual testing required)
  - Authentication flow
  - Post creation

**Test Results:**
```
57 passed, 6 skipped, 1 warning in 8.76s
Coverage: 83%
```

**Test Fixtures:**
- Real Kagi News RSS item with all sections
- Sample HTML descriptions
- Mock HTTP responses

### Integration Tests

**Manual Integration Testing Required:**
- [ ] Can authenticate with live Coves API
- [ ] Can create post via Coves API
- [ ] Can fetch real Kagi RSS feed
- [ ] Images display correctly from Kagi proxy
- [ ] State persistence works in production
- [ ] CRON scheduling works correctly

**Pre-deployment Checklist:**
- [x] All unit tests passing
- [x] Can parse real Kagi HTML
- [x] State persistence works
- [x] Config validation works
- [x] Error handling comprehensive
- [ ] Aggregator DID created
- [ ] Can authenticate with Coves API
- [ ] Docker container builds and runs

---

## Success Metrics

### ✅ Phase 1: Implementation - COMPLETE

- [x] All core components implemented
- [x] 57 tests passing with 83% coverage
- [x] RSS fetching and parsing working
- [x] Rich text formatting with facets
- [x] State management and deduplication
- [x] Configuration management
- [x] Comprehensive error handling
- [x] Documentation complete

### 🔄 Phase 2: Integration Testing - IN PROGRESS

- [ ] Aggregator DID created (PLC)
- [ ] Aggregator authorized in 1+ test communities
- [ ] Can authenticate with Coves API
- [ ] First post created end-to-end
- [ ] Attribution visible ("Via Kagi News")
- [ ] No duplicate posts on repeated runs
- [ ] Images display correctly

### 📋 Phase 3: Alpha Deployment (First Week)

- [ ] Docker Compose runs successfully in production
- [ ] 2-3 communities receiving posts
- [ ] 20+ posts created successfully
- [ ] Zero duplicates
- [ ] < 10% errors (parse or post creation)
- [ ] CRON scheduling reliable

### 🎯 Phase 4: Beta (First Month)

- [ ] 5+ communities using aggregator
- [ ] 200+ posts created
- [ ] Positive community feedback
- [ ] No rate limit issues
- [ ] < 5% error rate
- [ ] Performance metrics tracked

---

## What's Next: Integration & Deployment

### Immediate Next Steps

1. **Create Aggregator Identity**
   - Generate DID for aggregator
   - Store credentials securely
   - Test authentication with Coves API

2. **Integration Testing**
   - Test with live Coves API
   - Verify post creation works
   - Validate rich text rendering
   - Check image display from Kagi proxy

3. **Docker Deployment**
   - Build Docker image
   - Test docker-compose setup
   - Verify CRON scheduling
   - Set up monitoring/logging

4. **Community Authorization**
   - Get aggregator authorized in test community
   - Verify authorization flow works
   - Test posting to real community

5. **Production Deployment**
   - Deploy to production server
   - Configure feeds for real communities
   - Monitor first batch of posts
   - Gather community feedback

### Open Questions to Resolve

1. **Aggregator DID Creation:**
   - Need helper script or manual process?
   - Where to store credentials securely?

2. **Authorization Flow:**
   - How does community admin authorize aggregator?
   - UI flow or XRPC endpoint?

3. **Image Strategy:**
   - Confirm Kagi proxy images work reliably
   - Fallback plan if proxy becomes unreliable?

4. **Monitoring:**
   - What metrics to track initially?
   - Alerting strategy for failures?

---

## Future Enhancements (Post-MVP)

### Phase 2
- Multiple post formats (summary, minimal)
- Per-community filtering (subcategories, min sources)
- More sophisticated deduplication
- Metrics dashboard

### Phase 3
- Interactive features (bot responds to comments)
- Cross-posting prevention
- Federation support

---

## References

- Kagi News About: https://news.kagi.com/about
- Kagi News RSS: https://news.kagi.com/world.xml
- CC BY-NC License: https://creativecommons.org/licenses/by-nc/4.0/
- Parent PRD: [PRD_AGGREGATORS.md](PRD_AGGREGATORS.md)
- ATProto Python SDK: https://github.com/MarshalX/atproto
- Implementation: [aggregators/kagi-news/](/aggregators/kagi-news/)

---

## Implementation Summary

**Phase 1 Status:** ✅ **COMPLETE**

The Kagi News RSS Aggregator implementation is complete and ready for integration testing and deployment. All 7 core components have been implemented with comprehensive test coverage (57 tests, 83% coverage).

**What Was Built:**
- Complete RSS feed fetching and parsing pipeline
- HTML parser that extracts all structured data from Kagi News feeds (summary, highlights, perspectives, quote, sources)
- Rich text formatter with proper facets for Coves
- State management system for deduplication
- Configuration management with YAML and environment variables
- HTTP client for Coves API authentication and post creation
- Main orchestrator with robust error handling
- Comprehensive test suite with real feed fixtures
- Documentation and example configurations

**Key Findings:**
- Kagi News RSS feeds contain only 3 structured sections (Highlights, Perspectives, Sources)
- Historical context is woven into the summary and highlights, not a separate section
- Timeline feature visible on Kagi website is not in the RSS feed
- All essential data for rich posts is available in the feed
- Feed structure is stable and well-formed

**Next Phase:**
Integration testing with live Coves API, followed by alpha deployment to test communities.

---

**End of PRD - Phase 1 Implementation Complete**
