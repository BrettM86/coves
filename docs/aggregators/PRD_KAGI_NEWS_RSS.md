# Kagi News RSS Aggregator PRD

**Status:** Planning Phase
**Owner:** Platform Team
**Last Updated:** 2025-10-20
**Parent PRD:** [PRD_AGGREGATORS.md](PRD_AGGREGATORS.md)

## Overview

The Kagi News RSS Aggregator is a reference implementation of the Coves aggregator system that automatically posts high-quality, multi-source news summaries to communities. It leverages Kagi News's free RSS feeds to provide pre-aggregated, deduped news content with multiple perspectives and source citations.

**Key Value Propositions:**
- **Multi-source aggregation**: Kagi News already aggregates multiple sources per story
- **Balanced perspectives**: Built-in perspective tracking from different outlets
- **Rich metadata**: Categories, highlights, source links included
- **Legal & free**: CC BY-NC licensed for non-commercial use
- **Low complexity**: No LLM deduplication needed (Kagi does it)

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
- `tech.xml` - Technology (likely)
- `business.xml` - Business (likely)
- `sports.xml` - Sports (likely)
- Additional categories TBD (need to scrape homepage)

**Feed Format:** RSS 2.0 (standard XML)

**Update Frequency:** One daily update (~noon UTC)

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

**Key Features:**
- Multiple source citations inline
- Balanced perspectives from different actors
- Highlights extract key points
- Direct quotes preserved
- All sources linked with attribution

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
│  Kagi News Aggregator Service                               │
│  DID: did:web:kagi-news.coves.social                        │
│                                                             │
│  Components:                                                 │
│  1. Feed Poller: Fetches RSS feeds on schedule              │
│  2. Item Parser: Extracts structured data from HTML         │
│  3. Deduplication: Tracks posted GUIDs (no LLM needed)      │
│  4. Category Mapper: Maps Kagi categories to communities    │
│  5. Post Formatter: Converts to Coves post format           │
│  6. Post Publisher: Calls social.coves.post.create          │
└─────────────────────────────────────────────────────────────┘
                            │
                            │ Authenticated XRPC calls
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  Coves AppView (social.coves.post.create)                   │
│  - Validates aggregator authorization                        │
│  - Creates post with author = did:web:kagi-news.coves.social│
│  - Indexes to community feeds                                │
└─────────────────────────────────────────────────────────────┘
```

---

### Aggregator Service Declaration

```json
{
  "$type": "social.coves.aggregator.service",
  "did": "did:web:kagi-news.coves.social",
  "displayName": "Kagi News Aggregator",
  "description": "Automatically posts breaking news from Kagi News RSS feeds. Kagi News aggregates multiple sources per story with balanced perspectives and comprehensive source citations.",
  "aggregatorType": "social.coves.aggregator.types#rss",
  "avatar": "<blob reference to Kagi logo>",
  "configSchema": {
    "type": "object",
    "properties": {
      "categories": {
        "type": "array",
        "items": {
          "type": "string",
          "enum": ["world", "tech", "business", "sports", "science"]
        },
        "description": "Kagi News categories to monitor",
        "minItems": 1
      },
      "subcategoryFilter": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Optional: only post stories with these subcategories (e.g., 'World/Middle East', 'Tech/AI')"
      },
      "minSources": {
        "type": "integer",
        "minimum": 1,
        "default": 2,
        "description": "Minimum number of sources required for a story to be posted"
      },
      "includeImages": {
        "type": "boolean",
        "default": true,
        "description": "Include images from Kagi proxy in posts"
      },
      "postFormat": {
        "type": "string",
        "enum": ["full", "summary", "minimal"],
        "default": "full",
        "description": "How much content to include: full (all sections), summary (main paragraph + sources), minimal (title + link only)"
      }
    },
    "required": ["categories"]
  },
  "sourceUrl": "https://github.com/coves-social/kagi-news-aggregator",
  "maintainer": "did:plc:coves-platform",
  "createdAt": "2025-10-20T12:00:00Z"
}
```

---

## Community Configuration Examples

### Example 1: World News Community

```json
{
  "aggregatorDid": "did:web:kagi-news.coves.social",
  "enabled": true,
  "config": {
    "categories": ["world"],
    "minSources": 3,
    "includeImages": true,
    "postFormat": "full"
  }
}
```

**Result:** Posts all world news stories with 3+ sources, full content including images/highlights/perspectives.

---

### Example 2: AI/Tech Community (Filtered)

```json
{
  "aggregatorDid": "did:web:kagi-news.coves.social",
  "enabled": true,
  "config": {
    "categories": ["tech", "business"],
    "subcategoryFilter": ["Tech/AI", "Tech/Machine Learning", "Business/Tech Industry"],
    "minSources": 2,
    "includeImages": true,
    "postFormat": "full"
  }
}
```

**Result:** Only posts tech stories about AI/ML or tech industry business news with 2+ sources.

---

### Example 3: Breaking News (Minimal)

```json
{
  "aggregatorDid": "did:web:kagi-news.coves.social",
  "enabled": true,
  "config": {
    "categories": ["world", "business", "tech"],
    "minSources": 5,
    "includeImages": false,
    "postFormat": "minimal"
  }
}
```

**Result:** Only major stories (5+ sources), minimal format (headline + link), no images.

---

## Post Format Specification

### Post Record Structure

```json
{
  "$type": "social.coves.post.record",
  "author": "did:web:kagi-news.coves.social",
  "community": "did:plc:worldnews123",
  "title": "{Kagi story title}",
  "content": "{formatted content based on postFormat config}",
  "embed": {
    "$type": "app.bsky.embed.external",
    "external": {
      "uri": "https://kite.kagi.com/{uuid}/{category}/{id}",
      "title": "{story title}",
      "description": "{summary excerpt}",
      "thumb": "{image blob if includeImages=true}"
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

---

### Content Formatting by `postFormat`

#### Format: `full` (Default)

```markdown
{Main summary paragraph with source citations}

**Highlights:**
• {Bullet point 1}
• {Bullet point 2}
• ...

**Perspectives:**
• **{Actor}**: {Their perspective} ([Source]({url}))
• ...

> {Notable quote} — {Attribution}

**Sources:**
• [{Title}]({url}) - {domain}
• ...

---
📰 Story aggregated by [Kagi News]({kagi_story_url})
```

**Rationale:** Preserves Kagi's rich multi-source analysis, provides maximum value.

---

#### Format: `summary`

```markdown
{Main summary paragraph with source citations}

**Sources:**
• [{Title}]({url}) - {domain}
• ...

---
📰 Story aggregated by [Kagi News]({kagi_story_url})
```

**Rationale:** Clean summary with source links, less overwhelming.

---

#### Format: `minimal`

```markdown
{Story title}

Read more: {kagi_story_url}

**Sources:** {domain1}, {domain2}, {domain3}...

---
📰 Via [Kagi News]({kagi_story_url})
```

**Rationale:** Just headlines with link, for high-volume communities or breaking news alerts.

---

## Implementation Details

### Component 1: Feed Poller

**Responsibility:** Fetch RSS feeds on schedule

```go
type FeedPoller struct {
    categories   []string
    pollInterval time.Duration
    httpClient   *http.Client
}

func (p *FeedPoller) Start(ctx context.Context) error {
    ticker := time.NewTicker(p.pollInterval) // 15 minutes
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            for _, category := range p.categories {
                feedURL := fmt.Sprintf("https://news.kagi.com/%s.xml", category)
                feed, err := p.fetchFeed(feedURL)
                if err != nil {
                    log.Printf("Failed to fetch %s: %v", feedURL, err)
                    continue
                }
                p.handleFeed(ctx, category, feed)
            }
        case <-ctx.Done():
            return nil
        }
    }
}

func (p *FeedPoller) fetchFeed(url string) (*gofeed.Feed, error) {
    parser := gofeed.NewParser()
    feed, err := parser.ParseURL(url)
    return feed, err
}
```

**Libraries:**
- `github.com/mmcdole/gofeed` - RSS/Atom parser

---

### Component 2: Item Parser

**Responsibility:** Extract structured data from RSS item HTML

```go
type KagiStory struct {
    Title             string
    Link              string
    GUID              string
    PubDate           time.Time
    Categories        []string

    // Parsed from HTML description
    Summary           string
    Highlights        []string
    Perspectives      []Perspective
    Quote             *Quote
    Sources           []Source
    ImageURL          string
    ImageAlt          string
}

type Perspective struct {
    Actor       string
    Description string
    SourceURL   string
}

type Quote struct {
    Text        string
    Attribution string
}

type Source struct {
    Title  string
    URL    string
    Domain string
}

func (p *ItemParser) Parse(item *gofeed.Item) (*KagiStory, error) {
    doc, err := goquery.NewDocumentFromReader(strings.NewReader(item.Description))
    if err != nil {
        return nil, err
    }

    story := &KagiStory{
        Title:      item.Title,
        Link:       item.Link,
        GUID:       item.GUID,
        PubDate:    *item.PublishedParsed,
        Categories: item.Categories,
    }

    // Extract summary (first <p> tag)
    story.Summary = doc.Find("p").First().Text()

    // Extract highlights
    doc.Find("h3:contains('Highlights')").Next("ul").Find("li").Each(func(i int, s *goquery.Selection) {
        story.Highlights = append(story.Highlights, s.Text())
    })

    // Extract perspectives
    doc.Find("h3:contains('Perspectives')").Next("ul").Find("li").Each(func(i int, s *goquery.Selection) {
        text := s.Text()
        link := s.Find("a").First()
        sourceURL, _ := link.Attr("href")

        // Parse format: "Actor: Description (Source)"
        parts := strings.SplitN(text, ":", 2)
        if len(parts) == 2 {
            story.Perspectives = append(story.Perspectives, Perspective{
                Actor:       strings.TrimSpace(parts[0]),
                Description: strings.TrimSpace(parts[1]),
                SourceURL:   sourceURL,
            })
        }
    })

    // Extract quote
    doc.Find("blockquote").Each(func(i int, s *goquery.Selection) {
        text := s.Text()
        parts := strings.Split(text, " - ")
        if len(parts) == 2 {
            story.Quote = &Quote{
                Text:        strings.TrimSpace(parts[0]),
                Attribution: strings.TrimSpace(parts[1]),
            }
        }
    })

    // Extract sources
    doc.Find("h3:contains('Sources')").Next("ul").Find("li").Each(func(i int, s *goquery.Selection) {
        link := s.Find("a").First()
        url, _ := link.Attr("href")
        title := link.Text()
        domain := extractDomain(s.Text())

        story.Sources = append(story.Sources, Source{
            Title:  title,
            URL:    url,
            Domain: domain,
        })
    })

    // Extract image
    img := doc.Find("img").First()
    if img.Length() > 0 {
        story.ImageURL, _ = img.Attr("src")
        story.ImageAlt, _ = img.Attr("alt")
    }

    return story, nil
}
```

**Libraries:**
- `github.com/PuerkitoBio/goquery` - HTML parsing

---

### Component 3: Deduplication

**Responsibility:** Track posted stories to prevent duplicates

```go
type Deduplicator struct {
    db *sql.DB
}

func (d *Deduplicator) AlreadyPosted(guid string) (bool, error) {
    var exists bool
    err := d.db.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM kagi_news_posted_stories
            WHERE guid = $1
        )
    `, guid).Scan(&exists)
    return exists, err
}

func (d *Deduplicator) MarkPosted(guid, postURI string) error {
    _, err := d.db.Exec(`
        INSERT INTO kagi_news_posted_stories (guid, post_uri, posted_at)
        VALUES ($1, $2, NOW())
        ON CONFLICT (guid) DO NOTHING
    `, guid, postURI)
    return err
}
```

**Database Table:**
```sql
CREATE TABLE kagi_news_posted_stories (
    guid TEXT PRIMARY KEY,
    post_uri TEXT NOT NULL,
    posted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_kagi_posted_at ON kagi_news_posted_stories(posted_at DESC);
```

**Cleanup:** Periodic job deletes rows older than 30 days (Kagi unlikely to re-post old stories).

---

### Component 4: Category Mapper

**Responsibility:** Map Kagi categories to authorized communities

```go
func (m *CategoryMapper) GetTargetCommunities(story *KagiStory) ([]*CommunityAuth, error) {
    // Get all communities that have authorized this aggregator
    allAuths, err := m.aggregator.GetAuthorizedCommunities(context.Background())
    if err != nil {
        return nil, err
    }

    var targets []*CommunityAuth
    for _, auth := range allAuths {
        if !auth.Enabled {
            continue
        }

        config := auth.Config

        // Check if story's primary category is in config.categories
        primaryCategory := story.Categories[0]
        if !contains(config["categories"], primaryCategory) {
            continue
        }

        // Check subcategory filter (if specified)
        if subcatFilter, ok := config["subcategoryFilter"].([]string); ok && len(subcatFilter) > 0 {
            if !hasAnySubcategory(story.Categories, subcatFilter) {
                continue
            }
        }

        // Check minimum sources requirement
        minSources := config["minSources"].(int)
        if len(story.Sources) < minSources {
            continue
        }

        targets = append(targets, auth)
    }

    return targets, nil
}
```

---

### Component 5: Post Formatter

**Responsibility:** Convert Kagi story to Coves post format

```go
func (f *PostFormatter) Format(story *KagiStory, format string) string {
    switch format {
    case "full":
        return f.formatFull(story)
    case "summary":
        return f.formatSummary(story)
    case "minimal":
        return f.formatMinimal(story)
    default:
        return f.formatFull(story)
    }
}

func (f *PostFormatter) formatFull(story *KagiStory) string {
    var buf strings.Builder

    // Summary
    buf.WriteString(story.Summary)
    buf.WriteString("\n\n")

    // Highlights
    if len(story.Highlights) > 0 {
        buf.WriteString("**Highlights:**\n")
        for _, h := range story.Highlights {
            buf.WriteString(fmt.Sprintf("• %s\n", h))
        }
        buf.WriteString("\n")
    }

    // Perspectives
    if len(story.Perspectives) > 0 {
        buf.WriteString("**Perspectives:**\n")
        for _, p := range story.Perspectives {
            buf.WriteString(fmt.Sprintf("• **%s**: %s ([Source](%s))\n", p.Actor, p.Description, p.SourceURL))
        }
        buf.WriteString("\n")
    }

    // Quote
    if story.Quote != nil {
        buf.WriteString(fmt.Sprintf("> %s — %s\n\n", story.Quote.Text, story.Quote.Attribution))
    }

    // Sources
    buf.WriteString("**Sources:**\n")
    for _, s := range story.Sources {
        buf.WriteString(fmt.Sprintf("• [%s](%s) - %s\n", s.Title, s.URL, s.Domain))
    }
    buf.WriteString("\n")

    // Attribution
    buf.WriteString(fmt.Sprintf("---\n📰 Story aggregated by [Kagi News](%s)", story.Link))

    return buf.String()
}

func (f *PostFormatter) formatSummary(story *KagiStory) string {
    var buf strings.Builder

    buf.WriteString(story.Summary)
    buf.WriteString("\n\n**Sources:**\n")
    for _, s := range story.Sources {
        buf.WriteString(fmt.Sprintf("• [%s](%s) - %s\n", s.Title, s.URL, s.Domain))
    }
    buf.WriteString("\n")
    buf.WriteString(fmt.Sprintf("---\n📰 Story aggregated by [Kagi News](%s)", story.Link))

    return buf.String()
}

func (f *PostFormatter) formatMinimal(story *KagiStory) string {
    sourceDomains := make([]string, len(story.Sources))
    for i, s := range story.Sources {
        sourceDomains[i] = s.Domain
    }

    return fmt.Sprintf(
        "%s\n\nRead more: %s\n\n**Sources:** %s\n\n---\n📰 Via [Kagi News](%s)",
        story.Title,
        story.Link,
        strings.Join(sourceDomains, ", "),
        story.Link,
    )
}
```

---

### Component 6: Post Publisher

**Responsibility:** Create posts via Coves API

```go
func (p *PostPublisher) PublishStory(ctx context.Context, story *KagiStory, communities []*CommunityAuth) error {
    for _, comm := range communities {
        config := comm.Config

        // Format content based on config
        postFormat := config["postFormat"].(string)
        content := p.formatter.Format(story, postFormat)

        // Build embed
        var embed *aggregator.Embed
        if config["includeImages"].(bool) && story.ImageURL != "" {
            // TODO: Handle image upload/blob creation
            embed = &aggregator.Embed{
                Type: "app.bsky.embed.external",
                External: &aggregator.External{
                    URI:         story.Link,
                    Title:       story.Title,
                    Description: truncate(story.Summary, 300),
                    Thumb:       story.ImageURL, // or blob reference
                },
            }
        }

        // Create post
        post := aggregator.Post{
            Title:   story.Title,
            Content: content,
            Embed:   embed,
            FederatedFrom: &aggregator.FederatedSource{
                Platform:          "kagi-news-rss",
                URI:               story.Link,
                ID:                story.GUID,
                OriginalCreatedAt: story.PubDate,
            },
            ContentLabels: story.Categories,
        }

        err := p.aggregator.CreatePost(ctx, comm.CommunityDID, post)
        if err != nil {
            log.Printf("Failed to create post in %s: %v", comm.CommunityDID, err)
            continue
        }

        // Mark as posted
        _ = p.deduplicator.MarkPosted(story.GUID, "post-uri-from-response")
    }

    return nil
}
```

---

## Image Handling Strategy

### Initial Implementation (MVP)

**Approach:** Use Kagi proxy URLs directly in embeds

**Rationale:**
- Simplest implementation
- Kagi proxy likely allows hotlinking for non-commercial use
- No storage costs
- Images are already optimized by Kagi

**Risk Mitigation:**
- Monitor for broken images
- Add fallback: if image fails to load, skip embed
- Prepare migration plan to self-hosting if needed

**Code:**
```go
if config["includeImages"].(bool) && story.ImageURL != "" {
    // Use Kagi proxy URL directly
    embed = &aggregator.Embed{
        External: &aggregator.External{
            Thumb: story.ImageURL, // https://kagiproxy.com/img/...
        },
    }
}
```

---

### Future Enhancement (If Issues Arise)

**Approach:** Download and re-host images

**Implementation:**
1. Download image from Kagi proxy
2. Upload to Coves blob storage (or S3/CDN)
3. Use blob reference in embed

**Code:**
```go
func (p *PostPublisher) uploadImage(imageURL string) (string, error) {
    // Download from Kagi proxy
    resp, err := http.Get(imageURL)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    // Upload to blob storage
    blob, err := p.blobStore.Upload(resp.Body, resp.Header.Get("Content-Type"))
    if err != nil {
        return "", err
    }

    return blob.Ref, nil
}
```

**Decision Point:** Only implement if:
- Kagi blocks hotlinking
- Kagi proxy becomes unreliable
- Legal clarification needed

---

## Rate Limiting & Performance

### Rate Limits

**RSS Fetching:**
- Poll each category feed every 15 minutes
- Max 4 categories = 4 requests per 15 min = 16 req/hour
- Well within any reasonable limit

**Post Creation:**
- Aggregator rate limit: 10 posts/hour per community
- Global limit: 100 posts/hour across all communities
- Kagi News publishes ~5-10 stories per category per day
- = ~20-40 posts/day total across all categories
- = ~2-4 posts/hour average
- Well within limits

**Performance Targets:**
- Story posted within 15 minutes of appearing in RSS feed
- < 1 second to parse and format a story
- < 500ms to publish a post via API

---

## Monitoring & Observability

### Metrics to Track

**Feed Polling:**
- `kagi_feed_poll_total` (counter) - Total feed polls by category
- `kagi_feed_poll_errors` (counter) - Failed polls by category/error
- `kagi_feed_items_fetched` (gauge) - Items per poll by category
- `kagi_feed_poll_duration_seconds` (histogram) - Poll latency

**Story Processing:**
- `kagi_stories_parsed_total` (counter) - Successfully parsed stories
- `kagi_stories_parse_errors` (counter) - Parse failures by error type
- `kagi_stories_filtered` (counter) - Stories filtered out by reason (duplicate, min sources, category)
- `kagi_stories_posted` (counter) - Stories successfully posted by community

**Post Publishing:**
- `kagi_posts_created_total` (counter) - Total posts created
- `kagi_posts_failed` (counter) - Failed posts by error type
- `kagi_post_publish_duration_seconds` (histogram) - Post creation latency

**Health:**
- `kagi_aggregator_up` (gauge) - Service health (1 = healthy, 0 = down)
- `kagi_last_successful_poll_timestamp` (gauge) - Last successful poll time by category

---

### Logging

**Structured Logging:**
```go
log.Info("Story posted",
    "guid", story.GUID,
    "title", story.Title,
    "community", comm.CommunityDID,
    "post_uri", postURI,
    "sources", len(story.Sources),
    "format", postFormat,
)

log.Error("Failed to parse story",
    "guid", item.GUID,
    "feed", feedURL,
    "error", err,
)
```

**Log Levels:**
- DEBUG: Feed items, parsing details
- INFO: Stories posted, communities targeted
- WARN: Parse errors, rate limit approaching
- ERROR: Failed posts, feed fetch failures

---

### Alerts

**Critical:**
- Feed polling failing for > 1 hour
- Post creation failing for > 10 consecutive attempts
- Aggregator unauthorized (auth record disabled/deleted)

**Warning:**
- Post creation rate < 50% of expected
- Parse errors > 10% of items
- Approaching rate limits (> 80% of quota)

---

## Deployment

### Infrastructure

**Service Type:** Long-running daemon

**Hosting:** Kubernetes (same cluster as Coves AppView)

**Resources:**
- CPU: 0.5 cores (low CPU usage, mostly I/O)
- Memory: 512 MB (small in-memory cache for recent GUIDs)
- Storage: 1 GB (SQLite for deduplication tracking)

---

### Configuration

**Environment Variables:**
```bash
# Aggregator identity
AGGREGATOR_DID=did:web:kagi-news.coves.social
AGGREGATOR_PRIVATE_KEY_PATH=/secrets/private-key.pem

# Coves API
COVES_API_URL=https://api.coves.social

# Feed polling
POLL_INTERVAL=15m
CATEGORIES=world,tech,business,sports

# Database (for deduplication)
DB_PATH=/data/kagi-news.db

# Monitoring
METRICS_PORT=9090
LOG_LEVEL=info
```

---

### Deployment Manifest

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagi-news-aggregator
  namespace: coves
spec:
  replicas: 1
  selector:
    matchLabels:
      app: kagi-news-aggregator
  template:
    metadata:
      labels:
        app: kagi-news-aggregator
    spec:
      containers:
      - name: aggregator
        image: coves/kagi-news-aggregator:latest
        env:
        - name: AGGREGATOR_DID
          value: did:web:kagi-news.coves.social
        - name: COVES_API_URL
          value: https://api.coves.social
        - name: POLL_INTERVAL
          value: 15m
        - name: CATEGORIES
          value: world,tech,business,sports
        - name: DB_PATH
          value: /data/kagi-news.db
        - name: AGGREGATOR_PRIVATE_KEY_PATH
          value: /secrets/private-key.pem
        volumeMounts:
        - name: data
          mountPath: /data
        - name: secrets
          mountPath: /secrets
          readOnly: true
        ports:
        - name: metrics
          containerPort: 9090
        resources:
          requests:
            cpu: 250m
            memory: 256Mi
          limits:
            cpu: 500m
            memory: 512Mi
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: kagi-news-data
      - name: secrets
        secret:
          secretName: kagi-news-private-key
```

---

## Testing Strategy

### Unit Tests

**Feed Parsing:**
```go
func TestParseFeed(t *testing.T) {
    feed := loadTestFeed("testdata/world.xml")
    stories, err := parser.Parse(feed)
    assert.NoError(t, err)
    assert.Len(t, stories, 10)

    story := stories[0]
    assert.NotEmpty(t, story.Title)
    assert.NotEmpty(t, story.Summary)
    assert.Greater(t, len(story.Sources), 1)
}

func TestParseStoryHTML(t *testing.T) {
    html := `<p>Summary [source.com#1]</p>
             <h3>Highlights:</h3>
             <ul><li>Point 1</li></ul>
             <h3>Sources:</h3>
             <ul><li><a href="https://example.com">Title</a> - example.com</li></ul>`

    story, err := parser.ParseHTML(html)
    assert.NoError(t, err)
    assert.Equal(t, "Summary [source.com#1]", story.Summary)
    assert.Len(t, story.Highlights, 1)
    assert.Len(t, story.Sources, 1)
}
```

**Formatting:**
```go
func TestFormatFull(t *testing.T) {
    story := &KagiStory{
        Summary: "Test summary",
        Sources: []Source{
            {Title: "Article", URL: "https://example.com", Domain: "example.com"},
        },
    }

    content := formatter.Format(story, "full")
    assert.Contains(t, content, "Test summary")
    assert.Contains(t, content, "**Sources:**")
    assert.Contains(t, content, "📰 Story aggregated by")
}
```

**Deduplication:**
```go
func TestDeduplication(t *testing.T) {
    guid := "test-guid-123"

    posted, err := deduplicator.AlreadyPosted(guid)
    assert.NoError(t, err)
    assert.False(t, posted)

    err = deduplicator.MarkPosted(guid, "at://...")
    assert.NoError(t, err)

    posted, err = deduplicator.AlreadyPosted(guid)
    assert.NoError(t, err)
    assert.True(t, posted)
}
```

---

### Integration Tests

**With Mock Coves API:**
```go
func TestPublishStory(t *testing.T) {
    // Setup mock Coves API
    mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        assert.Equal(t, "/xrpc/social.coves.post.create", r.URL.Path)

        var input CreatePostInput
        json.NewDecoder(r.Body).Decode(&input)

        assert.Equal(t, "did:plc:test-community", input.Community)
        assert.NotEmpty(t, input.Title)
        assert.Contains(t, input.Content, "📰 Story aggregated by")

        w.WriteHeader(200)
        json.NewEncoder(w).Encode(CreatePostOutput{URI: "at://..."})
    }))
    defer mockAPI.Close()

    // Test story publishing
    publisher := NewPostPublisher(mockAPI.URL)
    err := publisher.PublishStory(ctx, testStory, []*CommunityAuth{testComm})
    assert.NoError(t, err)
}
```

---

### E2E Tests

**With Real RSS Feed:**
```go
func TestE2E_FetchAndParse(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping E2E test")
    }

    // Fetch real Kagi News feed
    feed, err := poller.fetchFeed("https://news.kagi.com/world.xml")
    assert.NoError(t, err)
    assert.NotEmpty(t, feed.Items)

    // Parse first item
    story, err := parser.Parse(feed.Items[0])
    assert.NoError(t, err)
    assert.NotEmpty(t, story.Title)
    assert.NotEmpty(t, story.Summary)
    assert.Greater(t, len(story.Sources), 0)
}
```

**With Test Coves Instance:**
```go
func TestE2E_CreatePost(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping E2E test")
    }

    // Create post in test community
    post := aggregator.Post{
        Title:   "Test Kagi News Post",
        Content: "Test content...",
    }

    err := aggregator.CreatePost(ctx, testCommunityDID, post)
    assert.NoError(t, err)

    // Verify post appears in feed
    // (requires test community setup)
}
```

---

## Success Metrics

### Pre-Launch Checklist

- [ ] Aggregator service declaration published
- [ ] DID created and configured (did:web:kagi-news.coves.social)
- [ ] RSS feed parser handles all Kagi HTML structures
- [ ] Deduplication prevents duplicate posts
- [ ] Category mapping works for all configs
- [ ] All 3 post formats render correctly
- [ ] Attribution to Kagi News visible on all posts
- [ ] Rate limiting prevents spam
- [ ] Monitoring/alerting configured
- [ ] E2E tests passing against test instance

---

### Alpha Goals (First Week)

- [ ] 3+ communities using Kagi News aggregator
- [ ] 50+ posts created successfully
- [ ] Zero duplicate posts
- [ ] < 5% parse errors
- [ ] < 1% post creation failures
- [ ] Stories posted within 15 minutes of RSS publication

---

### Beta Goals (First Month)

- [ ] 10+ communities using aggregator
- [ ] 500+ posts created
- [ ] Community feedback positive (surveys)
- [ ] Attribution compliance verified
- [ ] No rate limit violations
- [ ] < 1% error rate (parsing + posting)

---

## Future Enhancements

### Phase 2 Features

**Smart Category Detection:**
- Use LLM to suggest additional categories for stories
- Map Kagi categories to community tags automatically

**Customizable Templates:**
- Allow communities to customize post format with templates
- Support Markdown/Handlebars templates in config

**Story Scoring:**
- Prioritize high-impact stories (many sources, breaking news)
- Delay low-priority stories to avoid flooding feed

**Cross-posting Prevention:**
- Detect when multiple communities authorize same category
- Intelligently cross-post vs. duplicate

---

### Phase 3 Features

**Interactive Features:**
- Bot responds to comments with additional sources
- Updates megathread with new sources as story develops

**Analytics Dashboard:**
- Show communities which stories get most engagement
- Trending topics from Kagi News
- Source diversity metrics

**Federation:**
- Support other Coves instances using same aggregator
- Shared deduplication across instances

---

## Open Questions

### Need to Resolve Before Launch

1. **Image Licensing:**
   - ❓ Are images from Kagi proxy covered by CC BY-NC?
   - ❓ Do we need to attribute original image sources?
   - **Action:** Email support@kagi.com for clarification

2. **Hotlinking Policy:**
   - ❓ Is embedding Kagi proxy images acceptable?
   - ❓ Should we download and re-host?
   - **Action:** Test in staging, monitor for issues

3. **Category Discovery:**
   - ❓ How to discover all available category feeds?
   - ❓ Are there categories beyond world/tech/business/sports?
   - **Action:** Scrape https://news.kagi.com/ for all .xml links

4. **Attribution Format:**
   - ❓ Is "📰 Story aggregated by Kagi News" sufficient?
   - ❓ Do we need more prominent attribution?
   - **Action:** Review CC BY-NC best practices

---

## References

- Kagi News About Page: https://news.kagi.com/about
- Kagi News RSS Example: https://news.kagi.com/world.xml
- Kagi Kite Public Repo: https://github.com/kagisearch/kite-public
- CC BY-NC License: https://creativecommons.org/licenses/by-nc/4.0/
- Parent PRD: [PRD_AGGREGATORS.md](PRD_AGGREGATORS.md)
- Aggregator SDK: [TBD]
