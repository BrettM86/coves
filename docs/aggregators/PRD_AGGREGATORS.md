# Aggregators PRD: Automated Content Posting System

**Status:** In Development - Phase 1 (Core Infrastructure)
**Owner:** Platform Team
**Last Updated:** 2025-10-20

---

## Overview

Coves Aggregators are autonomous services that automatically post content to communities. Each aggregator is identified by its own DID and operates as a specialized actor within the atProto ecosystem. This enables communities to have automated content feeds (RSS, sports results, TV/movie discussion threads, Bluesky mirrors, etc.) while maintaining full community control.

**Key Differentiator:** Unlike other platforms where users manually aggregate content, Coves communities can enable automated aggregators to handle routine posting tasks, creating a more dynamic and up-to-date community experience.

---

## Architecture Principles

### ✅ atProto-Compliant Design

Aggregators follow established atProto patterns for autonomous services (Feed Generators + Labelers model):

1. **Aggregators are Actors, Not a Separate System**
   - Each aggregator has its own DID
   - Authenticate as themselves via JWT
   - Use existing `social.coves.post.create` endpoint
   - Post record's `author` field = aggregator DID (server-populated)
   - No separate posting API needed

2. **Community Authorization Model**
   - Communities create `social.coves.aggregator.authorization` records in their repo
   - Records grant specific aggregators permission to post
   - Include aggregator-specific configuration
   - Can be enabled/disabled without deletion

3. **Hybrid Hosting**
   - Coves can host official aggregators
   - Third parties can build and host their own
   - All use same authorization system

---

## Core Components

### 1. Service Declaration Record
**Lexicon:** `social.coves.aggregator.service`
**Location:** Aggregator's repository
**Key:** `literal:self`

Declares aggregator existence and provides metadata for discovery.

**Required Fields:**
- `did` - Aggregator's DID (must match repo)
- `displayName` - Human-readable name
- `createdAt` - Creation timestamp

**Optional Fields:**
- `description` - What this aggregator does
- `avatar` - Avatar image blob
- `configSchema` - JSON Schema for community config validation
- `sourceUrl` - Link to source code (transparency)
- `maintainer` - DID of maintainer

---

### 2. Authorization Record
**Lexicon:** `social.coves.aggregator.authorization`
**Location:** Community's repository
**Key:** `any`

Grants an aggregator permission to post with specific configuration.

**Required Fields:**
- `aggregatorDid` - DID of authorized aggregator
- `communityDid` - DID of community (must match repo)
- `enabled` - Active status (toggleable)
- `createdAt` - When authorized

**Optional Fields:**
- `config` - Aggregator-specific config (validated against schema)
- `createdBy` - Moderator who authorized
- `disabledAt` / `disabledBy` - Audit trail

---

## Data Flow

```
Aggregator Service (External)
  │
  │ 1. Authenticates as aggregator DID (JWT)
  │ 2. Calls social.coves.post.create
  ▼
Coves AppView Handler
  │
  │ 1. Extract DID from JWT
  │ 2. Check if DID is registered aggregator
  │ 3. Validate authorization exists & enabled
  │ 4. Apply aggregator rate limits
  │ 5. Create post with author = aggregator DID
  ▼
Jetstream → AppView Indexing
  │
  │ Post indexed with aggregator attribution
  │ UI shows: "🤖 Posted by [Aggregator Name]"
  ▼
Community Feed
```

---

## XRPC Methods

### For Communities (Moderators)

- **`social.coves.aggregator.enable`** - Create authorization record
- **`social.coves.aggregator.disable`** - Set enabled=false
- **`social.coves.aggregator.updateConfig`** - Update config
- **`social.coves.aggregator.listForCommunity`** - List aggregators for community

### For Aggregators

- **`social.coves.post.create`** - Modified to handle aggregator auth
- **`social.coves.aggregator.getAuthorizations`** - Query authorized communities

### For Discovery

- **`social.coves.aggregator.getServices`** - Fetch aggregator details by DID(s)

---

## Database Schema

### `aggregators` Table
Indexes aggregator service declarations from Jetstream.

**Key Columns:**
- `did` (PK) - Aggregator DID
- `display_name`, `description` - Service metadata
- `config_schema` - JSON Schema for config validation
- `avatar_url`, `source_url`, `maintainer_did` - Metadata
- `record_uri`, `record_cid` - atProto record metadata
- `communities_using`, `posts_created` - Cached stats (updated by triggers)

### `aggregator_authorizations` Table
Indexes community authorization records from Jetstream.

**Key Columns:**
- `aggregator_did`, `community_did` - Authorization pair (unique together)
- `enabled` - Active status
- `config` - Community-specific JSON config
- `created_by`, `disabled_by` - Audit trail
- `record_uri`, `record_cid` - atProto record metadata

**Critical Indexes:**
- `idx_aggregator_auth_lookup` - Fast (aggregator_did, community_did, enabled) lookups for post creation

### `aggregator_posts` Table
AppView-only tracking for rate limiting and stats (not from lexicon).

**Key Columns:**
- `aggregator_did`, `community_did`, `post_uri`
- `created_at` - For rate limit calculations

---

## Security

### Authentication
- DID-based authentication via JWT signatures
- No shared secrets or API keys
- Aggregators can only post to authorized communities

### Authorization Checks
- Server validates aggregator status (not client-provided)
- Checks `aggregator_authorizations` table on every post
- Config validated against aggregator's JSON schema

### Rate Limiting
- Aggregators: 10 posts/hour per community
- Tracked via `aggregator_posts` table
- Prevents spam

### Audit Trail
- `created_by` / `disabled_by` track moderator actions
- Full history preserved in authorization records

---

## Implementation Phases

### ✅ Phase 1: Core Infrastructure (COMPLETE)
**Status:** ✅ COMPLETE - All components implemented and tested
**Goal:** Enable aggregator authentication and authorization

**Components:**
- ✅ Lexicon schemas (9 files)
- ✅ Database migrations (2 migrations: 3 tables, 2 triggers, indexes)
- ✅ Repository layer (CRUD operations, bulk queries, optimized indexes)
- ✅ Service layer (business logic, validation, rate limiting)
- ✅ Modified post creation handler (aggregator authentication & authorization)
- ✅ XRPC query handlers (getServices, getAuthorizations, listForCommunity)
- ✅ Jetstream consumer (indexes service & authorization records from firehose)
- ✅ Integration tests (10+ test suites, E2E validation)
- ✅ E2E test validation (verified records exist in both PDS and AppView)

**Milestone:** ✅ ACHIEVED - Aggregators can authenticate and post to authorized communities

**Deferred to Phase 2:**
- Write-forward operations (enable, disable, updateConfig) - require PDS integration
- Moderator permission checks - require communities ownership validation

---

### Phase 2: Aggregator SDK (Post-Alpha)
**Deferred** - Will build SDK after Phase 1 is validated in production.

Core functionality works without SDK - aggregators just need to:
1. Create atProto account (get DID)
2. Publish service declaration record
3. Sign JWTs with their DID keys
4. Call existing XRPC endpoints

---

### Phase 3: Reference Implementation (Future)
**Deferred** - First aggregator will likely be built inline to validate the system.

Potential first aggregator: RSS news bot for select communities.

---

## Key Design Decisions

### 2025-10-20: Remove `aggregatorType` Field
**Decision:** Removed `aggregatorType` enum from service declaration and database.

**Rationale:**
- Pre-production - can break things
- Over-engineering for alpha
- Description field is sufficient for discovery
- Avoids rigid categorization
- Can add tags later if needed

**Impact:**
- Simplified lexicons
- Removed database constraint
- More flexible for third-party developers

---

### 2025-10-19: Reuse `social.coves.post.create` Endpoint
**Decision:** Aggregators use existing post creation endpoint.

**Rationale:**
- Post record already server-populates `author` from JWT
- Simpler: one code path for all post creation
- Follows atProto principle: actors are actors
- `federatedFrom` field handles external content attribution

**Implementation:**
- Add branching logic in post handler: if aggregator, check authorization; else check membership
- Apply different rate limits based on actor type

---

### 2025-10-19: Config as JSON Schema
**Decision:** Aggregators declare `configSchema` in service record.

**Rationale:**
- Communities need to know what config options are available
- JSON Schema is standard and well-supported
- Enables UI auto-generation (forms from schema)
- Validation at authorization creation time
- Flexible: each aggregator has different config needs

---

## Use Cases

### RSS News Aggregator
Watches configured RSS feeds, uses LLM for deduplication, posts news articles to community.

**Community Config Example:**
```json
{
  "feeds": ["https://techcrunch.com/feed"],
  "topics": ["technology"],
  "dedupeWindow": "6h"
}
```

---

### Bluesky Post Mirror
Monitors specific users/hashtags on Bluesky, creates posts in community with original author metadata.

**Community Config Example:**
```json
{
  "mirrorUsers": ["alice.bsky.social"],
  "hashtags": ["covesalpha"],
  "minLikes": 10
}
```

---

### Sports Results
Monitors sports APIs, creates post-game threads with scores and stats.

**Community Config Example:**
```json
{
  "league": "NBA",
  "teams": ["Lakers", "Warriors"],
  "includeStats": true
}
```

---

## Success Metrics

### Alpha Goals
- ✅ Lexicons validated
- ✅ Database migrations tested
- ⏳ Jetstream consumer indexes records
- ⏳ Post creation validates aggregator auth
- ⏳ Rate limiting prevents spam
- ⏳ Integration tests passing

### Beta Goals (Future)
- First aggregator deployed in production
- 3+ communities using aggregators
- < 0.1% spam posts
- Third-party developer documentation

---

## Out of Scope (Future)

- Aggregator marketplace with ratings/reviews
- UI for aggregator management (alpha uses XRPC only)
- Scheduled posts
- Interactive aggregators (respond to comments)
- Cross-instance aggregator discovery
- SDK (deferred until post-alpha)
- LLM features (deferred)

---

## References

- atProto Lexicon Spec: https://atproto.com/specs/lexicon
- Feed Generator Pattern: https://github.com/bluesky-social/feed-generator
- Labeler Pattern: https://github.com/bluesky-social/atproto/tree/main/packages/ozone
- JSON Schema: https://json-schema.org/
