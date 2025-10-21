# Aggregators PRD: Automated Content Posting System

**Status:** Planning / Design Phase
**Owner:** Platform Team
**Last Updated:** 2025-10-19

## Overview

Coves Aggregators are autonomous services that automatically post content to communities. Each aggregator is identified by its own DID and operates as a specialized actor within the atProto ecosystem. This system enables communities to have automated content feeds (RSS, sports results, TV/movie discussion threads, Bluesky mirrors, etc.) while maintaining full community control over which aggregators can post and what content they can create.

**Key Differentiator:** Unlike other platforms where users manually aggregate content, Coves communities can enable automated aggregators to handle routine posting tasks, creating a more dynamic and up-to-date community experience.

## Architecture Principles

### ✅ atProto-Compliant Design

Aggregators follow established atProto patterns for autonomous services:

**Pattern:** Feed Generators + Labelers Model
- Each aggregator has its own DID (like feed generators)
- Declaration record published in aggregator's repo (like `app.bsky.feed.generator`)
- DID document advertises service endpoint
- Service makes authenticated XRPC calls
- Communities explicitly authorize aggregators (like subscribing to labelers)

**Key Design Decisions:**

1. **Aggregators are Actors, Not a Separate System**
   - Aggregators authenticate as themselves (their DID)
   - Use existing `social.coves.post.create` endpoint
   - Post record's `author` field = aggregator DID (server-populated)
   - No separate posting API needed

2. **Community Authorization Model**
   - Communities create `social.coves.aggregator.authorization` records
   - These records grant specific aggregators permission to post
   - Authorizations include configuration (which RSS feeds, which users to mirror, etc.)
   - Can be enabled/disabled at any time

3. **Hybrid Hosting**
   - Coves can host official aggregators (RSS, sports, media)
   - Third parties can build and host their own aggregators
   - SDK provided for easy aggregator development
   - All aggregators use same authorization system

---

## Architecture Overview

```
┌────────────────────────────────────────────────────────────┐
│  Aggregator Service (External)                             │
│  DID: did:web:rss-bot.coves.social                         │
│                                                             │
│  - Watches external data sources (RSS, APIs, etc.)         │
│  - Processes content (LLM deduplication, formatting)       │
│  - Queries which communities have authorized it            │
│  - Creates posts via social.coves.post.create              │
│  - Responds to config queries via XRPC                     │
└────────────────────────────────────────────────────────────┘
                            │
                            │ 1. Authenticate as aggregator DID (JWT)
                            │ 2. Call social.coves.post.create
                            │    {
                            │      community: "did:plc:gaming123",
                            │      title: "...",
                            │      content: "...",
                            │      federatedFrom: { platform: "rss", ... }
                            │    }
                            ▼
┌────────────────────────────────────────────────────────────┐
│  Coves AppView (social.coves.post.create Handler)          │
│                                                             │
│  1. Extract DID from JWT (aggregator's DID)                │
│  2. Check if DID is registered aggregator                  │
│  3. Validate authorization record exists & enabled         │
│  4. Apply aggregator-specific rate limits                  │
│  5. Validate content against community rules               │
│  6. Create post with author = aggregator DID               │
└────────────────────────────────────────────────────────────┘
                            │
                            │ Post record created:
                            │ {
                            │   $type: "social.coves.post.record",
                            │   author: "did:web:rss-bot.coves.social",
                            │   community: "did:plc:gaming123",
                            │   title: "Tech News Roundup",
                            │   content: "...",
                            │   federatedFrom: {
                            │     platform: "rss",
                            │     uri: "https://techcrunch.com/..."
                            │   }
                            │ }
                            ▼
┌────────────────────────────────────────────────────────────┐
│  Jetstream → AppView Indexing                              │
│  - Post indexed with aggregator attribution                │
│  - UI shows: "🤖 Posted by RSS Aggregator"                 │
│  - Community feed includes automated posts                 │
└────────────────────────────────────────────────────────────┘
```

---

## Use Cases

### 1. RSS News Aggregator
**Problem:** Multiple users posting the same breaking news from different sources
**Solution:** RSS aggregator with LLM deduplication
- Watches configured RSS feeds
- Uses LLM to identify duplicate stories from different outlets
- Creates single "megathread" with all sources linked
- Posts unbiased summary of event
- Automatically tags with relevant topics

**Community Config:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social",
  "enabled": true,
  "config": {
    "feeds": [
      "https://techcrunch.com/feed",
      "https://arstechnica.com/feed"
    ],
    "topics": ["technology", "ai"],
    "dedupeWindow": "6h",
    "minSources": 2
  }
}
```

### 2. Bluesky Post Mirror
**Problem:** Want to surface specific Bluesky discussions in community
**Solution:** Bluesky mirror aggregator
- Monitors specific users or hashtags on Bluesky
- Creates posts in community when criteria met
- Preserves `originalAuthor` metadata
- Links back to original Bluesky thread

**Community Config:**
```json
{
  "aggregatorDid": "did:web:bsky-mirror.coves.social",
  "enabled": true,
  "config": {
    "mirrorUsers": [
      "alice.bsky.social",
      "bob.bsky.social"
    ],
    "hashtags": ["covesalpha"],
    "minLikes": 10
  }
}
```

### 3. Sports Results Aggregator
**Problem:** Need post-game threads created immediately after games end
**Solution:** Sports aggregator watching game APIs
- Monitors sports APIs for game completions
- Creates post-game thread with final score, stats
- Tags with team names and league
- Posts within minutes of game ending

**Community Config:**
```json
{
  "aggregatorDid": "did:web:sports-bot.coves.social",
  "enabled": true,
  "config": {
    "league": "NBA",
    "teams": ["Lakers", "Warriors"],
    "includeStats": true,
    "autoPin": true
  }
}
```

### 4. TV/Movie Discussion Aggregator
**Problem:** Want episode discussion threads created when shows air
**Solution:** Media aggregator tracking release schedules
- Uses TMDB/IMDB APIs for release dates
- Creates discussion threads when episodes/movies release
- Includes metadata (cast, synopsis, ratings)
- Automatically pins for premiere episodes

**Community Config:**
```json
{
  "aggregatorDid": "did:web:media-bot.coves.social",
  "enabled": true,
  "config": {
    "shows": [
      {"tmdbId": "1234", "name": "Breaking Bad"}
    ],
    "createOn": "airDate",
    "timezone": "America/New_York",
    "spoilerProtection": true
  }
}
```

---

## Lexicon Schemas

### 1. Aggregator Service Declaration

**Collection:** `social.coves.aggregator.service`
**Key:** `literal:self` (one per aggregator account)
**Location:** Aggregator's own repository

This record declares the existence of an aggregator service and provides metadata for discovery.

```json
{
  "lexicon": 1,
  "id": "social.coves.aggregator.service",
  "defs": {
    "main": {
      "type": "record",
      "description": "Declaration of an aggregator service that can post to communities",
      "key": "literal:self",
      "record": {
        "type": "object",
        "required": ["did", "displayName", "createdAt", "aggregatorType"],
        "properties": {
          "did": {
            "type": "string",
            "format": "did",
            "description": "DID of the aggregator service (must match repo DID)"
          },
          "displayName": {
            "type": "string",
            "maxGraphemes": 64,
            "maxLength": 640,
            "description": "Human-readable name (e.g., 'RSS News Aggregator')"
          },
          "description": {
            "type": "string",
            "maxGraphemes": 300,
            "maxLength": 3000,
            "description": "Description of what this aggregator does"
          },
          "avatar": {
            "type": "blob",
            "accept": ["image/png", "image/jpeg"],
            "maxSize": 1000000,
            "description": "Avatar image for bot identity"
          },
          "aggregatorType": {
            "type": "string",
            "knownValues": [
              "social.coves.aggregator.types#rss",
              "social.coves.aggregator.types#blueskyMirror",
              "social.coves.aggregator.types#sports",
              "social.coves.aggregator.types#media",
              "social.coves.aggregator.types#custom"
            ],
            "description": "Type of aggregator for categorization"
          },
          "configSchema": {
            "type": "unknown",
            "description": "JSON Schema describing config options for this aggregator. Communities use this to know what configuration fields are available."
          },
          "sourceUrl": {
            "type": "string",
            "format": "uri",
            "description": "URL to aggregator's source code (for transparency)"
          },
          "maintainer": {
            "type": "string",
            "format": "did",
            "description": "DID of person/organization maintaining this aggregator"
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          }
        }
      }
    }
  }
}
```

**Example Record:**
```json
{
  "$type": "social.coves.aggregator.service",
  "did": "did:web:rss-bot.coves.social",
  "displayName": "RSS News Aggregator",
  "description": "Automatically posts breaking news from configured RSS feeds with LLM-powered deduplication",
  "aggregatorType": "social.coves.aggregator.types#rss",
  "configSchema": {
    "type": "object",
    "properties": {
      "feeds": {
        "type": "array",
        "items": { "type": "string", "format": "uri" }
      },
      "topics": {
        "type": "array",
        "items": { "type": "string" }
      },
      "dedupeWindow": { "type": "string" },
      "minSources": { "type": "integer", "minimum": 1 }
    }
  },
  "sourceUrl": "https://github.com/coves-social/rss-aggregator",
  "maintainer": "did:plc:coves-platform",
  "createdAt": "2025-10-19T12:00:00Z"
}
```

---

### 2. Community Authorization Record

**Collection:** `social.coves.aggregator.authorization`
**Key:** `any` (one per aggregator per community)
**Location:** Community's repository

This record grants an aggregator permission to post to a community and contains aggregator-specific configuration.

```json
{
  "lexicon": 1,
  "id": "social.coves.aggregator.authorization",
  "defs": {
    "main": {
      "type": "record",
      "description": "Authorization for an aggregator to post to a community with specific configuration",
      "key": "any",
      "record": {
        "type": "object",
        "required": ["aggregatorDid", "communityDid", "createdAt", "enabled"],
        "properties": {
          "aggregatorDid": {
            "type": "string",
            "format": "did",
            "description": "DID of the authorized aggregator"
          },
          "communityDid": {
            "type": "string",
            "format": "did",
            "description": "DID of the community granting access (must match repo DID)"
          },
          "enabled": {
            "type": "boolean",
            "description": "Whether this aggregator is currently active. Can be toggled without deleting the record."
          },
          "config": {
            "type": "unknown",
            "description": "Aggregator-specific configuration. Must conform to the aggregator's configSchema."
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          },
          "createdBy": {
            "type": "string",
            "format": "did",
            "description": "DID of moderator who authorized this aggregator"
          },
          "disabledAt": {
            "type": "string",
            "format": "datetime",
            "description": "When this authorization was disabled (if enabled=false)"
          },
          "disabledBy": {
            "type": "string",
            "format": "did",
            "description": "DID of moderator who disabled this aggregator"
          }
        }
      }
    }
  }
}
```

**Example Record:**
```json
{
  "$type": "social.coves.aggregator.authorization",
  "aggregatorDid": "did:web:rss-bot.coves.social",
  "communityDid": "did:plc:gaming123",
  "enabled": true,
  "config": {
    "feeds": [
      "https://techcrunch.com/feed",
      "https://arstechnica.com/feed"
    ],
    "topics": ["technology", "ai", "gaming"],
    "dedupeWindow": "6h",
    "minSources": 2
  },
  "createdAt": "2025-10-19T14:00:00Z",
  "createdBy": "did:plc:alice123"
}
```

---

### 3. Aggregator Type Definitions

**Collection:** `social.coves.aggregator.types`
**Purpose:** Define known aggregator types for categorization

```json
{
  "lexicon": 1,
  "id": "social.coves.aggregator.types",
  "defs": {
    "rss": {
      "type": "string",
      "description": "Aggregator that monitors RSS/Atom feeds"
    },
    "blueskyMirror": {
      "type": "string",
      "description": "Aggregator that mirrors Bluesky posts"
    },
    "sports": {
      "type": "string",
      "description": "Aggregator for sports scores and game threads"
    },
    "media": {
      "type": "string",
      "description": "Aggregator for TV/movie discussion threads"
    },
    "custom": {
      "type": "string",
      "description": "Custom third-party aggregator"
    }
  }
}
```

---

## XRPC Methods

### For Communities (Moderators)

#### `social.coves.aggregator.enable`
Enable an aggregator for a community

**Input:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social",
  "config": {
    "feeds": ["https://techcrunch.com/feed"],
    "topics": ["technology"]
  }
}
```

**Output:**
```json
{
  "uri": "at://did:plc:gaming123/social.coves.aggregator.authorization/3jui7kd58dt2g",
  "cid": "bafyreif5...",
  "authorization": {
    "aggregatorDid": "did:web:rss-bot.coves.social",
    "communityDid": "did:plc:gaming123",
    "enabled": true,
    "config": {...},
    "createdAt": "2025-10-19T14:00:00Z"
  }
}
```

**Behavior:**
- Validates caller is community moderator
- Validates aggregator exists and has service declaration
- Validates config against aggregator's configSchema
- Creates authorization record in community's repo
- Indexes to AppView for authorization checks

**Errors:**
- `NotAuthorized` - Caller is not a moderator
- `AggregatorNotFound` - Aggregator DID doesn't exist
- `InvalidConfig` - Config doesn't match configSchema

---

#### `social.coves.aggregator.disable`
Disable an aggregator for a community

**Input:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social"
}
```

**Output:**
```json
{
  "uri": "at://did:plc:gaming123/social.coves.aggregator.authorization/3jui7kd58dt2g",
  "disabled": true,
  "disabledAt": "2025-10-19T15:00:00Z"
}
```

**Behavior:**
- Validates caller is community moderator
- Updates authorization record (sets `enabled=false`, `disabledAt`, `disabledBy`)
- Aggregator can no longer post until re-enabled

---

#### `social.coves.aggregator.updateConfig`
Update configuration for an enabled aggregator

**Input:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social",
  "config": {
    "feeds": ["https://techcrunch.com/feed", "https://arstechnica.com/feed"],
    "topics": ["technology", "gaming"]
  }
}
```

**Output:**
```json
{
  "uri": "at://did:plc:gaming123/social.coves.aggregator.authorization/3jui7kd58dt2g",
  "cid": "bafyreif6...",
  "config": {...}
}
```

---

#### `social.coves.aggregator.listForCommunity`
List all aggregators (enabled and disabled) for a community

**Input:**
```json
{
  "community": "did:plc:gaming123",
  "enabledOnly": false,
  "limit": 50,
  "cursor": "..."
}
```

**Output:**
```json
{
  "aggregators": [
    {
      "aggregatorDid": "did:web:rss-bot.coves.social",
      "displayName": "RSS News Aggregator",
      "description": "...",
      "aggregatorType": "social.coves.aggregator.types#rss",
      "enabled": true,
      "config": {...},
      "createdAt": "2025-10-19T14:00:00Z"
    }
  ],
  "cursor": "..."
}
```

---

### For Aggregators

#### Existing: `social.coves.post.create`
**Modified Behavior:** Now handles aggregator authentication

**Authorization Flow:**
1. Extract DID from JWT
2. Check if DID is registered aggregator (query `aggregators` table)
3. If aggregator:
   - Validate authorization record exists for this community
   - Check `enabled=true`
   - Apply aggregator rate limits (e.g., 10 posts/hour)
4. If regular user:
   - Validate membership, bans, etc. (existing logic)
5. Create post with `author = actorDID`

**Rate Limits:**
- Regular users: 20 posts/hour per community
- Aggregators: 10 posts/hour per community (to prevent spam)

---

#### `social.coves.aggregator.getAuthorizations`
Get list of communities that have authorized this aggregator

**Input:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social",
  "enabledOnly": true,
  "limit": 100,
  "cursor": "..."
}
```

**Output:**
```json
{
  "authorizations": [
    {
      "communityDid": "did:plc:gaming123",
      "communityName": "Gaming News",
      "enabled": true,
      "config": {...},
      "createdAt": "2025-10-19T14:00:00Z"
    }
  ],
  "cursor": "..."
}
```

**Use Case:** Aggregator queries this to know which communities to post to

---

### For Discovery

#### `social.coves.aggregator.list`
List all available aggregators

**Input:**
```json
{
  "type": "social.coves.aggregator.types#rss",
  "limit": 50,
  "cursor": "..."
}
```

**Output:**
```json
{
  "aggregators": [
    {
      "did": "did:web:rss-bot.coves.social",
      "displayName": "RSS News Aggregator",
      "description": "...",
      "aggregatorType": "social.coves.aggregator.types#rss",
      "avatar": "...",
      "maintainer": "did:plc:coves-platform",
      "sourceUrl": "https://github.com/coves-social/rss-aggregator"
    }
  ],
  "cursor": "..."
}
```

---

#### `social.coves.aggregator.get`
Get detailed information about a specific aggregator

**Input:**
```json
{
  "aggregatorDid": "did:web:rss-bot.coves.social"
}
```

**Output:**
```json
{
  "did": "did:web:rss-bot.coves.social",
  "displayName": "RSS News Aggregator",
  "description": "...",
  "aggregatorType": "social.coves.aggregator.types#rss",
  "configSchema": {...},
  "sourceUrl": "...",
  "maintainer": "...",
  "stats": {
    "communitiesUsing": 42,
    "postsCreated": 1337,
    "createdAt": "2025-10-19T12:00:00Z"
  }
}
```

---

## Database Schema

### `aggregators` Table
Indexed aggregator service declarations from Jetstream

```sql
CREATE TABLE aggregators (
  did TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  description TEXT,
  aggregator_type TEXT NOT NULL,
  config_schema JSONB,
  avatar_url TEXT,
  source_url TEXT,
  maintainer_did TEXT,

  -- Indexing metadata
  record_uri TEXT NOT NULL,
  record_cid TEXT NOT NULL,
  indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

  -- Stats (cached)
  communities_using INTEGER NOT NULL DEFAULT 0,
  posts_created BIGINT NOT NULL DEFAULT 0,

  CONSTRAINT aggregators_type_check CHECK (
    aggregator_type IN (
      'social.coves.aggregator.types#rss',
      'social.coves.aggregator.types#blueskyMirror',
      'social.coves.aggregator.types#sports',
      'social.coves.aggregator.types#media',
      'social.coves.aggregator.types#custom'
    )
  )
);

CREATE INDEX idx_aggregators_type ON aggregators(aggregator_type);
CREATE INDEX idx_aggregators_indexed_at ON aggregators(indexed_at DESC);
```

---

### `aggregator_authorizations` Table
Indexed authorization records from communities

```sql
CREATE TABLE aggregator_authorizations (
  id BIGSERIAL PRIMARY KEY,

  -- Authorization identity
  aggregator_did TEXT NOT NULL REFERENCES aggregators(did) ON DELETE CASCADE,
  community_did TEXT NOT NULL,

  -- Authorization state
  enabled BOOLEAN NOT NULL DEFAULT true,
  config JSONB,

  -- Audit trail
  created_at TIMESTAMPTZ NOT NULL,
  created_by TEXT NOT NULL, -- DID of moderator
  disabled_at TIMESTAMPTZ,
  disabled_by TEXT, -- DID of moderator

  -- atProto record metadata
  record_uri TEXT NOT NULL UNIQUE,
  record_cid TEXT NOT NULL,
  indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

  UNIQUE(aggregator_did, community_did)
);

CREATE INDEX idx_aggregator_auth_agg_did ON aggregator_authorizations(aggregator_did) WHERE enabled = true;
CREATE INDEX idx_aggregator_auth_comm_did ON aggregator_authorizations(community_did) WHERE enabled = true;
CREATE INDEX idx_aggregator_auth_enabled ON aggregator_authorizations(enabled);
```

---

### `aggregator_posts` Table
Track posts created by aggregators (for rate limiting and stats)

```sql
CREATE TABLE aggregator_posts (
  id BIGSERIAL PRIMARY KEY,

  aggregator_did TEXT NOT NULL REFERENCES aggregators(did) ON DELETE CASCADE,
  community_did TEXT NOT NULL,
  post_uri TEXT NOT NULL,
  post_cid TEXT NOT NULL,

  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

  UNIQUE(post_uri)
);

CREATE INDEX idx_aggregator_posts_agg_did_created ON aggregator_posts(aggregator_did, created_at DESC);
CREATE INDEX idx_aggregator_posts_comm_did_created ON aggregator_posts(community_did, created_at DESC);

-- For rate limiting: count posts in last hour
CREATE INDEX idx_aggregator_posts_rate_limit ON aggregator_posts(aggregator_did, community_did, created_at DESC);
```

---

## Implementation Plan

### Phase 1: Core Infrastructure (Coves AppView)

**Goal:** Enable aggregator authentication and authorization

#### 1.1 Database Setup
- [ ] Create migration for `aggregators` table
- [ ] Create migration for `aggregator_authorizations` table
- [ ] Create migration for `aggregator_posts` table

#### 1.2 Lexicon Definitions
- [ ] Create `social.coves.aggregator.service.json`
- [ ] Create `social.coves.aggregator.authorization.json`
- [ ] Create `social.coves.aggregator.types.json`
- [ ] Generate Go types from lexicons

#### 1.3 Repository Layer
```go
// internal/core/aggregators/repository.go

type Repository interface {
    // Aggregator management
    CreateAggregator(ctx context.Context, agg *Aggregator) error
    GetAggregator(ctx context.Context, did string) (*Aggregator, error)
    ListAggregators(ctx context.Context, filter AggregatorFilter) ([]*Aggregator, error)
    UpdateAggregatorStats(ctx context.Context, did string, stats Stats) error

    // Authorization management
    CreateAuthorization(ctx context.Context, auth *Authorization) error
    GetAuthorization(ctx context.Context, aggDID, commDID string) (*Authorization, error)
    ListAuthorizationsForAggregator(ctx context.Context, aggDID string, enabledOnly bool) ([]*Authorization, error)
    ListAuthorizationsForCommunity(ctx context.Context, commDID string) ([]*Authorization, error)
    UpdateAuthorization(ctx context.Context, auth *Authorization) error
    IsAuthorized(ctx context.Context, aggDID, commDID string) (bool, error)

    // Post tracking (for rate limiting)
    RecordAggregatorPost(ctx context.Context, aggDID, commDID, postURI string) error
    CountRecentPosts(ctx context.Context, aggDID, commDID string, since time.Time) (int, error)
}
```

#### 1.4 Service Layer
```go
// internal/core/aggregators/service.go

type Service interface {
    // For communities (moderators)
    EnableAggregator(ctx context.Context, commDID, aggDID string, config map[string]interface{}) (*Authorization, error)
    DisableAggregator(ctx context.Context, commDID, aggDID string) error
    UpdateAggregatorConfig(ctx context.Context, commDID, aggDID string, config map[string]interface{}) error
    ListCommunityAggregators(ctx context.Context, commDID string, enabledOnly bool) ([]*AggregatorInfo, error)

    // For aggregators
    GetAuthorizedCommunities(ctx context.Context, aggDID string) ([]*CommunityAuth, error)

    // For discovery
    ListAggregators(ctx context.Context, filter AggregatorFilter) ([]*Aggregator, error)
    GetAggregator(ctx context.Context, did string) (*AggregatorDetail, error)

    // Internal: called by post creation handler
    ValidateAggregatorPost(ctx context.Context, aggDID, commDID string) error
}
```

#### 1.5 Modify Post Creation Handler
```go
// internal/api/handlers/post/create.go

func CreatePost(ctx context.Context, input *CreatePostInput) (*CreatePostOutput, error) {
    actorDID := GetDIDFromAuth(ctx)

    // Check if actor is an aggregator
    if isAggregator, _ := aggregatorService.IsAggregator(ctx, actorDID); isAggregator {
        // Validate aggregator authorization
        if err := aggregatorService.ValidateAggregatorPost(ctx, actorDID, input.Community); err != nil {
            return nil, err
        }

        // Apply aggregator rate limits
        if err := rateLimitAggregator(ctx, actorDID, input.Community); err != nil {
            return nil, ErrRateLimitExceeded
        }
    } else {
        // Regular user validation (existing logic)
        // ... membership checks, ban checks, etc.
    }

    // Create post (author will be actorDID - either user or aggregator)
    post, err := postService.CreatePost(ctx, actorDID, input)
    if err != nil {
        return nil, err
    }

    // If aggregator, track the post
    if isAggregator {
        _ = aggregatorService.RecordPost(ctx, actorDID, input.Community, post.URI)
    }

    return post, nil
}
```

#### 1.6 XRPC Handlers
- [ ] `social.coves.aggregator.enable` handler
- [ ] `social.coves.aggregator.disable` handler
- [ ] `social.coves.aggregator.updateConfig` handler
- [ ] `social.coves.aggregator.listForCommunity` handler
- [ ] `social.coves.aggregator.getAuthorizations` handler
- [ ] `social.coves.aggregator.list` handler
- [ ] `social.coves.aggregator.get` handler

#### 1.7 Jetstream Consumer
```go
// internal/atproto/jetstream/aggregator_consumer.go

func (c *AggregatorConsumer) HandleEvent(ctx context.Context, evt *jetstream.Event) error {
    switch evt.Collection {
    case "social.coves.aggregator.service":
        switch evt.Operation {
        case "create", "update":
            return c.indexAggregatorService(ctx, evt)
        case "delete":
            return c.deleteAggregator(ctx, evt.DID)
        }

    case "social.coves.aggregator.authorization":
        switch evt.Operation {
        case "create", "update":
            return c.indexAuthorization(ctx, evt)
        case "delete":
            return c.deleteAuthorization(ctx, evt.URI)
        }
    }
    return nil
}
```

#### 1.8 Integration Tests
- [ ] Test aggregator service indexing from Jetstream
- [ ] Test authorization record indexing
- [ ] Test `social.coves.post.create` with aggregator auth
- [ ] Test authorization validation (enabled/disabled)
- [ ] Test rate limiting for aggregators
- [ ] Test config validation against schema

**Milestone:** Aggregators can authenticate and post to communities with authorization

---

### Phase 2: Aggregator SDK (Go)

**Goal:** Provide SDK for building aggregators easily

#### 2.1 SDK Core
```go
// github.com/coves-social/aggregator-sdk-go

package aggregator

type Aggregator interface {
    // Identity
    GetDID() string
    GetDisplayName() string
    GetDescription() string
    GetType() string
    GetConfigSchema() map[string]interface{}

    // Lifecycle
    Start(ctx context.Context) error
    Stop() error

    // Posting (provided by SDK)
    CreatePost(ctx context.Context, communityDID string, post Post) error
    GetAuthorizedCommunities(ctx context.Context) ([]*CommunityAuth, error)
}

type BaseAggregator struct {
    DID          string
    DisplayName  string
    Description  string
    Type         string
    PrivateKey   crypto.PrivateKey
    CovesAPIURL  string

    client *http.Client
}

type Post struct {
    Title          string
    Content        string
    Embed          interface{}
    FederatedFrom  *FederatedSource
    ContentLabels  []string
}

type FederatedSource struct {
    Platform           string // "rss", "bluesky", etc.
    URI                string
    ID                 string
    OriginalCreatedAt  time.Time
}

// Helper methods provided by SDK
func (a *BaseAggregator) CreatePost(ctx context.Context, communityDID string, post Post) error {
    // 1. Sign JWT with aggregator's private key
    token := a.signJWT()

    // 2. Call social.coves.post.create via XRPC
    resp, err := a.client.Post(
        a.CovesAPIURL + "/xrpc/social.coves.post.create",
        &CreatePostInput{
            Community:     communityDID,
            Title:         post.Title,
            Content:       post.Content,
            Embed:         post.Embed,
            FederatedFrom: post.FederatedFrom,
            ContentLabels: post.ContentLabels,
        },
        &CreatePostOutput{},
        WithAuth(token),
    )

    return err
}

func (a *BaseAggregator) GetAuthorizedCommunities(ctx context.Context) ([]*CommunityAuth, error) {
    // Call social.coves.aggregator.getAuthorizations
    token := a.signJWT()

    resp, err := a.client.Get(
        a.CovesAPIURL + "/xrpc/social.coves.aggregator.getAuthorizations",
        map[string]string{"aggregatorDid": a.DID, "enabledOnly": "true"},
        &GetAuthorizationsOutput{},
        WithAuth(token),
    )

    return resp.Authorizations, err
}
```

#### 2.2 SDK Documentation
- [ ] README with quickstart guide
- [ ] Example aggregators (RSS, Bluesky mirror)
- [ ] API reference documentation
- [ ] Configuration schema guide

**Milestone:** Third parties can build aggregators using SDK

---

### Phase 3: Reference Aggregator (RSS)

**Goal:** Build working RSS aggregator as reference implementation

#### 3.1 RSS Aggregator Implementation
```go
// github.com/coves-social/rss-aggregator

package main

import "github.com/coves-social/aggregator-sdk-go"

type RSSAggregator struct {
    *aggregator.BaseAggregator

    // RSS-specific config
    pollInterval time.Duration
    llmClient    *openai.Client
}

func (r *RSSAggregator) Start(ctx context.Context) error {
    // 1. Get authorized communities
    communities, err := r.GetAuthorizedCommunities(ctx)
    if err != nil {
        return err
    }

    // 2. Start polling loop
    ticker := time.NewTicker(r.pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            r.pollFeeds(ctx, communities)
        case <-ctx.Done():
            return nil
        }
    }
}

func (r *RSSAggregator) pollFeeds(ctx context.Context, communities []*CommunityAuth) {
    for _, comm := range communities {
        // Get RSS feeds from community config
        feeds := comm.Config["feeds"].([]string)

        for _, feedURL := range feeds {
            items, err := r.fetchFeed(feedURL)
            if err != nil {
                continue
            }

            // Process new items
            for _, item := range items {
                // Check if already posted
                if r.alreadyPosted(item.GUID) {
                    continue
                }

                // LLM deduplication logic
                duplicate := r.findDuplicate(item, comm.CommunityDID)
                if duplicate != nil {
                    r.addToMegathread(duplicate, item)
                    continue
                }

                // Create new post
                post := aggregator.Post{
                    Title:   item.Title,
                    Content: r.summarize(item),
                    FederatedFrom: &aggregator.FederatedSource{
                        Platform:          "rss",
                        URI:               item.Link,
                        OriginalCreatedAt: item.PublishedAt,
                    },
                }

                err = r.CreatePost(ctx, comm.CommunityDID, post)
                if err != nil {
                    log.Printf("Failed to create post: %v", err)
                    continue
                }

                r.markPosted(item.GUID)
            }
        }
    }
}

func (r *RSSAggregator) summarize(item *RSSItem) string {
    // Use LLM to create unbiased summary
    prompt := fmt.Sprintf("Summarize this news article in 2-3 sentences: %s", item.Description)
    summary, _ := r.llmClient.Complete(prompt)
    return summary
}

func (r *RSSAggregator) findDuplicate(item *RSSItem, communityDID string) *Post {
    // Use LLM to detect semantic duplicates
    // Query recent posts in community
    // Compare with embeddings/similarity
    return nil // or duplicate post
}
```

#### 3.2 Deployment
- [ ] Dockerfile for RSS aggregator
- [ ] Kubernetes manifests (for Coves-hosted instance)
- [ ] Environment configuration guide
- [ ] Monitoring and logging setup

#### 3.3 Testing
- [ ] Unit tests for feed parsing
- [ ] Integration tests with mock Coves API
- [ ] E2E test with real Coves instance
- [ ] LLM deduplication accuracy tests

**Milestone:** RSS aggregator running in production for select communities

---

### Phase 4: Additional Aggregators

#### 4.1 Bluesky Mirror Aggregator
- [ ] Monitor Jetstream for specific users/hashtags
- [ ] Preserve `originalAuthor` metadata
- [ ] Link back to original Bluesky post
- [ ] Rate limiting (don't flood community)

#### 4.2 Sports Aggregator
- [ ] Integrate with ESPN/TheSportsDB APIs
- [ ] Monitor game completions
- [ ] Create post-game threads with stats
- [ ] Auto-pin major games

#### 4.3 Media (TV/Movie) Aggregator
- [ ] Integrate with TMDB API
- [ ] Track show release schedules
- [ ] Create episode discussion threads
- [ ] Spoiler protection tags

**Milestone:** Multiple official aggregators available for communities

---

## Security Considerations

### Authentication
✅ **DID-based Authentication**
- Aggregators sign JWTs with their private keys
- Server validates JWT signature against DID document
- No shared secrets or API keys

✅ **Scoped Authorization**
- Authorization records are per-community
- Aggregator can only post to authorized communities
- Communities can revoke at any time

### Rate Limiting
✅ **Per-Aggregator Limits**
- 10 posts/hour per community (configurable)
- Prevents aggregator spam
- Separate from user rate limits

✅ **Global Limits**
- Total posts across all communities: 100/hour
- Prevents runaway aggregators

### Content Validation
✅ **Community Rules**
- Aggregator posts validated against community content rules
- No special exemptions (same rules as users)
- Community can ban specific content patterns

✅ **Config Validation**
- Authorization config validated against aggregator's configSchema
- Prevents injection attacks via config
- JSON schema validation

### Monitoring & Auditing
✅ **Audit Trail**
- All aggregator posts logged
- `created_by` tracks which moderator authorized
- `disabled_by` tracks who revoked access
- Full history preserved

✅ **Abuse Detection**
- Monitor for spam patterns
- Alert if aggregator posts rejected repeatedly
- Auto-disable after threshold violations

### Transparency
✅ **Open Source**
- Official aggregators open source
- Source URL in service declaration
- Community can audit behavior

✅ **Attribution**
- Posts clearly show aggregator authorship
- UI shows "🤖 Posted by [Aggregator Name]"
- No attempt to impersonate users

---

## UI/UX Considerations

### Community Settings
**Aggregator Management Page:**
- List of available aggregators (with descriptions, types)
- "Enable" button opens config modal
- Config form generated from aggregator's configSchema
- Toggle to enable/disable without deleting config
- Stats: posts created, last active

**Post Display:**
- Posts from aggregators have bot badge: "🤖"
- Shows aggregator name (e.g., "Posted by RSS News Bot")
- `federatedFrom` shows original source
- Link to original content (RSS article, Bluesky post, etc.)

### User Preferences
- Option to hide all aggregator posts
- Option to hide specific aggregators
- Filter posts by "user-created only" or "include bots"

---

## Success Metrics

### Pre-Launch Checklist
- [ ] Lexicons defined and validated
- [ ] Database migrations tested
- [ ] Jetstream consumer indexes aggregator records
- [ ] Post creation handler validates aggregator auth
- [ ] Rate limiting prevents spam
- [ ] SDK published and documented
- [ ] Reference RSS aggregator working
- [ ] E2E tests passing
- [ ] Security audit completed

### Alpha Goals
- 3+ official aggregators (RSS, Bluesky mirror, sports)
- 10+ communities using aggregators
- < 0.1% spam posts (false positives)
- Aggregator posts appear in feed within 1 minute

### Beta Goals
- Third-party aggregators launched
- 50+ communities using aggregators
- Developer documentation complete
- Marketplace/directory for discovery

---

## Out of Scope (Future Versions)

### Aggregator Marketplace
- [ ] Community ratings/reviews for aggregators
- [ ] Featured aggregators
- [ ] Paid aggregators (premium features)
- [ ] Aggregator analytics dashboard

### Advanced Features
- [ ] Scheduled posts (post at specific time)
- [ ] Content moderation integration (auto-label NSFW)
- [ ] Multi-community posting (single post to multiple communities)
- [ ] Interactive aggregators (respond to comments)
- [ ] Aggregator-to-aggregator communication (chains)

### Federation
- [ ] Cross-instance aggregator discovery
- [ ] Aggregator migration (change hosting provider)
- [ ] Federated aggregator authorization (trust other instances' aggregators)

---

## Technical Decisions Log

### 2025-10-19: Reuse `social.coves.post.create` Endpoint

**Decision:** Aggregators use existing post creation endpoint, not a separate `social.coves.aggregator.post.create`

**Rationale:**
- Post record already server-populates `author` field from JWT
- Aggregators authenticate as themselves → `author = aggregator DID`
- Simpler: one code path for all post creation
- Follows atProto principle: actors are actors (users, bots, aggregators)
- `federatedFrom` field already handles external content attribution

**Implementation:**
- Add authorization check to `social.coves.post.create` handler
- Check if authenticated DID is aggregator
- Validate authorization record exists and enabled
- Apply aggregator-specific rate limits
- Otherwise same logic as user posts

**Trade-offs Accepted:**
- Post creation handler has branching logic (user vs aggregator)
- But: keeps lexicon simple, reuses existing validation

---

### 2025-10-19: Hybrid Hosting Model

**Decision:** Support both Coves-hosted and third-party aggregators

**Rationale:**
- Coves can provide high-quality official aggregators (RSS, sports, media)
- Third parties can build specialized aggregators (niche communities)
- SDK makes it easy to build custom aggregators
- Follows feed generator model (anyone can run one)
- Decentralization-friendly

**Requirements:**
- SDK must be well-documented and maintained
- Authorization system must be DID-agnostic (works for any DID)
- Discovery system shows all aggregators (official + third-party)

---

### 2025-10-19: Config as JSON Schema

**Decision:** Aggregators declare configSchema in their service record

**Rationale:**
- Communities need to know what config options are available
- JSON Schema is standard, well-supported
- Enables UI auto-generation (forms from schema)
- Validation at authorization creation time
- Flexible: each aggregator can have different config structure

**Example:**
```json
{
  "configSchema": {
    "type": "object",
    "properties": {
      "feeds": {
        "type": "array",
        "items": { "type": "string", "format": "uri" },
        "description": "RSS feed URLs to monitor"
      },
      "topics": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Topics to filter posts by"
      }
    },
    "required": ["feeds"]
  }
}
```

**Trade-offs Accepted:**
- More complex than simple key-value config
- But: better UX (self-documenting), prevents errors

---

## References

- atProto Lexicon Spec: https://atproto.com/specs/lexicon
- Feed Generator Starter Kit: https://github.com/bluesky-social/feed-generator
- Labeler Implementation: https://github.com/bluesky-social/atproto/tree/main/packages/ozone
- JSON Schema Spec: https://json-schema.org/
- Coves Communities PRD: [PRD_COMMUNITIES.md](PRD_COMMUNITIES.md)
- Coves Posts Implementation: [IMPLEMENTATION_POST_CREATION.md](IMPLEMENTATION_POST_CREATION.md)
