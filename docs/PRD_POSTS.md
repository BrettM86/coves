# Posts PRD: Forum Content System

**Status:** ✅ Alpha CREATE Complete (2025-10-19) | Get/Update/Delete/Voting TODO
**Owner:** Platform Team
**Last Updated:** 2025-10-19

## 🎯 Implementation Status

### ✅ COMPLETED (Alpha - 2025-10-19)
- **Post Creation:** Full write-forward to community PDS with real-time Jetstream indexing
- **Handler Layer:** HTTP endpoint with authentication, validation, and security checks
- **Service Layer:** Business logic with token refresh and community resolution
- **Repository Layer:** PostgreSQL storage with proper indexing
- **Jetstream Consumer:** Real-time indexing with security validation
- **Database Migration:** Posts table created (migration 011)
- **E2E Tests:** Live PDS + Jetstream integration tests passing
- **at-identifier Support:** All 4 formats (DIDs, canonical, @-prefixed, scoped handles)

### ⚠️ DEFERRED TO BETA
- Content rules validation (text-only, image-only communities) - Governence
- Post read operations (get, list)
- Post update/edit operations
- Post deletion
- Voting system (upvotes/downvotes)
- Derived characteristics indexing (embed_type, text_length, etc.)

**See:** [IMPLEMENTATION_POST_CREATION.md](IMPLEMENTATION_POST_CREATION.md) for complete implementation details.

---

## Overview

Posts are the core content unit in Coves communities. Built on atProto, each post is stored in the **community's repository** and indexed by the AppView for discovery and interaction. Posts support rich text, embeds, voting, tagging, and federation with other atProto platforms.

## Architecture

### atProto Data Flow
Posts follow the community-owned repository pattern, matching the V2 Communities architecture:

```
User creates post → Written to COMMUNITY's PDS repository (using community credentials) →
Firehose broadcasts event → AppView Jetstream consumer indexes →
Post appears in feeds
```

**Repository Structure:**
```
Repository:  at://did:plc:community789/social.coves.post.record/3k2a4b5c6d7e
Owner:       did:plc:community789 (community owns the post)
Author:      did:plc:user123 (tracked in record metadata)
Hosted By:   did:web:coves.social (instance manages community credentials)
```

**Key Architectural Principles:**
- ✅ Communities own posts (posts live in community repos, like traditional forums)
- ✅ Author tracked in metadata (post.author field references user DID)
- ✅ Communities are portable (migrate instance = posts move with community)
- ✅ Matches V2 Communities pattern (community owns repository, instance manages credentials)
- ✅ Write operations use community's PDS credentials (not user credentials)

---

## Alpha Features (MVP - Ship First)

### Content Rules Integration
**Status:** Lexicon complete (2025-10-18), validation TODO
**Priority:** CRITICAL - Required for community content policies

Posts are validated against community-specific content rules at creation time. Communities can restrict:
- Allowed embed types (images, video, external, record)
- Text requirements (min/max length, required/optional)
- Title requirements
- Image count limits
- Federated content policies

**See:** [PRD_GOVERNANCE.md - Content Rules System](PRD_GOVERNANCE.md#content-rules-system) for full details.

**Implementation checklist:**
- [x] Lexicon: `contentRules` in `social.coves.community.profile` ✅
- [x] Lexicon: `postType` removed from `social.coves.post.create` ✅
- [ ] Validation: `ValidatePostAgainstRules()` service function
- [ ] Handler: Integrate validation in post creation endpoint
- [ ] AppView: Index derived characteristics (embed_type, text_length, etc.)
- [ ] Tests: Validate content rule enforcement

---

### Core Post Management
**Status:** ✅ CREATE COMPLETE (2025-10-19) - Get/Update/Delete TODO
**Priority:** CRITICAL - Posts are the foundation of the platform

#### Create Post
- [x] Lexicon: `social.coves.post.record` ✅
- [x] Lexicon: `social.coves.post.create` ✅
- [x] Removed `postType` enum in favor of content rules ✅ (2025-10-18)
- [x] Removed `postType` from record and get lexicons ✅ (2025-10-18)
- [x] **Handler:** `POST /xrpc/social.coves.post.create` ✅ (Alpha - see IMPLEMENTATION_POST_CREATION.md)
  - ✅ Accept: community (DID/handle), title (optional), content, facets, embed, contentLabels
  - ✅ Validate: User is authenticated, community exists, content within limits
  - ✅ Write: Create record in **community's PDS repository**
  - ✅ Return: AT-URI and CID of created post
  - ⚠️ Content rules validation deferred to Beta
- [x] **Service Layer:** `PostService.Create()` ✅
  - ✅ Resolve community identifier to DID (supports all 4 at-identifier formats)
  - ✅ Validate community exists and is not private
  - ✅ Fetch community from AppView
  - ⚠️ **Validate post against content rules** DEFERRED (see [PRD_GOVERNANCE.md](PRD_GOVERNANCE.md#content-rules-system))
  - ✅ Fetch community's PDS credentials with automatic token refresh
  - ✅ Build post record with author DID, timestamp, content
  - ✅ **Write to community's PDS** using community's access token
  - ✅ Return URI/CID for AppView indexing
- [x] **Validation:** ✅
  - ✅ Community reference is valid (supports DIDs and handles)
  - ✅ Content length ≤ 50,000 characters
  - ✅ Title (if provided) ≤ 3,000 bytes
  - ✅ ContentLabels are from known values (nsfw, spoiler, violence)
  - ⚠️ **Content rules compliance:** DEFERRED TO BETA
    - Check embed types against `allowedEmbedTypes`
    - Verify `requireText` / `minTextLength` / `maxTextLength`
    - Verify `requireTitle` if set
    - Check image counts against `minImages` / `maxImages`
    - Block federated posts if `allowFederated: false`
    - Return `ContentRuleViolation` error if validation fails
- [x] **E2E Test:** Create text post → Write to **community's PDS** → Index via Jetstream → Verify in AppView ✅

#### Get Post
- [x] Lexicon: `social.coves.post.get` ✅
- [ ] **Handler:** `GET /xrpc/social.coves.post.get?uri=at://...`
  - Accept: AT-URI of post
  - Return: Full post view with author, community, stats, viewer state
- [ ] **Service Layer:** `PostService.Get(uri, viewerDID)`
  - Fetch post from AppView PostgreSQL
  - Join with user/community data
  - Calculate stats (upvotes, downvotes, score, comment count)
  - Include viewer state (vote status, saved status, tags)
- [ ] **Repository:** `PostRepository.GetByURI()`
  - Single query with JOINs for author, community, stats
  - Handle missing posts gracefully (deleted or not indexed)
- [ ] **E2E Test:** Get post by URI → Verify all fields populated

#### Update Post
- [x] Lexicon: `social.coves.post.update` ✅
- [ ] **Handler:** `POST /xrpc/social.coves.post.update`
  - Accept: uri, title, content, facets, embed, contentLabels, editNote
  - Validate: User is post author, within 24-hour edit window
  - Write: Update record in **community's PDS**
  - Return: New CID
- [ ] **Service Layer:** `PostService.Update()`
  - Fetch existing post from AppView
  - Verify authorship (post.author == authenticated user DID)
  - Verify edit window (createdAt + 24 hours > now)
  - Fetch community's PDS credentials (with token refresh)
  - **Update record in community's PDS** using community's access token
  - Track edit timestamp (editedAt field)
- [ ] **Edit Window:** 24 hours from creation (hardcoded for Alpha)
- [ ] **Edit Note:** Optional explanation field (stored in record)
- [ ] **E2E Test:** Update post → Verify edit reflected in AppView

#### Delete Post
- [x] Lexicon: `social.coves.post.delete` ✅
- [ ] **Handler:** `POST /xrpc/social.coves.post.delete`
  - Accept: uri
  - Validate: User is post author OR community moderator
  - Write: Delete record from **community's PDS**
- [ ] **Service Layer:** `PostService.Delete()`
  - Verify authorship OR moderator permission
  - Fetch community's PDS credentials
  - **Delete from community's PDS** (broadcasts DELETE event to firehose)
  - Consumer handles soft delete in AppView
- [ ] **AppView Behavior:** Mark as deleted (soft delete), hide from feeds
- [ ] **Moderator Delete:** Community moderators can delete any post in their community
- [ ] **E2E Test:** Delete post → Verify hidden from queries

---

### Post Content Features

#### Rich Text Support
- [x] Lexicon: Facets reference `social.coves.richtext.facet` ✅
- [ ] **Supported Facets:**
  - Mentions: `@user.bsky.social` → Links to user profile
  - Links: `https://example.com` → Clickable URLs
  - Community mentions: `!community@instance` → Links to community
  - Hashtags: `#topic` → Tag-based discovery (Future)
- [ ] **Implementation:**
  - Store facets as JSON array in post record
  - Validate byte ranges match content
  - Render facets in AppView responses

#### Embeds (Alpha Scope)
- [x] Lexicon: Embed union type ✅
- [ ] **Alpha Support:**
  - **Images:** Upload to community's PDS blob storage, reference in embed
  - **External Links:** URL, title, description, thumbnail (client-fetched)
  - **Quoted Posts:** Reference another post's AT-URI
- [ ] **Defer to Beta:**
  - Video embeds (requires video processing infrastructure)

#### Content Labels
- [x] Lexicon: Self-applied labels ✅
- [ ] **Alpha Labels:**
  - `nsfw` - Not safe for work
  - `spoiler` - Spoiler content (blur/hide by default)
  - `violence` - Violent or graphic content
- [ ] **Implementation:**
  - Store as string array in post record
  - AppView respects labels in feed filtering
  - Client renders appropriate warnings/blurs

---

### Voting System

#### Upvotes & Downvotes
- [x] Lexicon: `social.coves.interaction.vote` ✅
- [ ] **Handler:** `POST /xrpc/social.coves.interaction.createVote`
  - Accept: subject (post AT-URI), direction (up/down)
  - Write: Create vote record in **user's repository**
- [ ] **Handler:** `POST /xrpc/social.coves.interaction.deleteVote`
  - Accept: voteUri (AT-URI of vote record)
  - Write: Delete vote record from **user's repository**
- [ ] **Vote Toggling:**
  - Upvote → Upvote = Delete upvote
  - Upvote → Downvote = Delete upvote + Create downvote
  - No vote → Upvote = Create upvote
- [ ] **Downvote Controls (Alpha):**
  - Global default: Downvotes enabled
  - Community-level toggle: `allowDownvotes` (Boolean in community.profile)
  - Instance-level toggle: Environment variable `ALLOW_DOWNVOTES` (Future)
- [ ] **AppView Indexing:**
  - Consumer tracks vote CREATE/DELETE events
  - Aggregate counts: upvotes, downvotes, score (upvotes - downvotes)
  - Track viewer's vote state (for "already voted" UI)
- [ ] **E2E Test:** Create vote → Index → Verify count updates → Delete vote → Verify count decrements

**Note:** Votes live in user's repository (user owns their voting history), but posts live in community's repository.

#### Vote Statistics
- [x] Lexicon: `postStats` in post view ✅
- [ ] **Stats Fields:**
  - `upvotes` - Total upvote count
  - `downvotes` - Total downvote count (0 if community disables)
  - `score` - Calculated score (upvotes - downvotes)
  - `commentCount` - Total comments (placeholder for Beta)
  - `shareCount` - Share tracking (Future)
  - `tagCounts` - Aggregate tag counts (Future)

---

### Jetstream Consumer (Indexing)

#### Post Event Handling
- [x] **Consumer:** `PostConsumer.HandlePostEvent()` ✅ (2025-10-19)
  - ✅ Listen for `social.coves.post.record` CREATE from **community repositories**
  - ✅ Parse post record, extract author DID and community DID (from AT-URI owner)
  - ⚠️ **Derive post characteristics:** DEFERRED (embed_type, text_length, has_title, has_embed for content rules filtering)
  - ✅ Insert in AppView PostgreSQL (CREATE only - UPDATE/DELETE deferred)
  - ✅ Index: uri, cid, author_did, community_did, title, content, created_at, indexed_at
  - ✅ **Security Validation:**
    - ✅ Verify event.repo matches community DID (posts must come from community repos)
    - ✅ Verify community exists in AppView (foreign key integrity)
    - ✅ Verify author exists in AppView (foreign key integrity)
    - ✅ Idempotent indexing for Jetstream replays

#### Vote Event Handling
- [ ] **Consumer:** `PostConsumer.HandleVoteEvent()` - DEFERRED TO BETA (voting system not yet implemented)
  - Listen for `social.coves.interaction.vote` CREATE/DELETE from **user repositories**
  - Parse subject URI (extract post)
  - Increment/decrement vote counts atomically
  - Track vote URI for viewer state queries
  - **Validation:** Verify event.repo matches voter DID (votes must come from user repos)

#### Error Handling
- [x] Invalid community references → Reject post (foreign key enforcement) ✅
- [x] Invalid author references → Reject post (foreign key enforcement) ✅
- [x] Malformed records → Skip indexing, log error ✅
- [x] Duplicate events → Idempotent operations (unique constraint on URI) ✅
- [x] Posts from user repos → Reject (repository DID validation) ✅

---

### Database Schema

#### Posts Table ✅ IMPLEMENTED (2025-10-19)

**Migration:** [internal/db/migrations/011_create_posts_table.sql](../internal/db/migrations/011_create_posts_table.sql)

```sql
CREATE TABLE posts (
  id BIGSERIAL PRIMARY KEY,
  uri TEXT UNIQUE NOT NULL,              -- AT-URI (at://community_did/collection/rkey)
  cid TEXT NOT NULL,                      -- Content ID
  rkey TEXT NOT NULL,                     -- Record key (TID)
  author_did TEXT NOT NULL,               -- Author's DID (from record metadata)
  community_did TEXT NOT NULL,            -- Community DID (from AT-URI owner)
  title TEXT,                             -- Post title (nullable)
  content TEXT,                           -- Post content
  content_facets JSONB,                   -- Rich text facets
  embed JSONB,                            -- Embedded content
  content_labels TEXT[],                  -- Self-applied labels

  -- ⚠️ Derived characteristics DEFERRED TO BETA (for content rules filtering)
  -- Will be added when content rules are implemented:
  -- embed_type TEXT,                        -- images, video, external, record (NULL if no embed)
  -- text_length INT NOT NULL DEFAULT 0,     -- Character count of content
  -- has_title BOOLEAN NOT NULL DEFAULT FALSE,
  -- has_embed BOOLEAN NOT NULL DEFAULT FALSE,

  created_at TIMESTAMPTZ NOT NULL,        -- Author's timestamp
  edited_at TIMESTAMPTZ,                  -- Last edit timestamp
  indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed
  deleted_at TIMESTAMPTZ,                 -- Soft delete

  -- Stats (denormalized for performance)
  upvote_count INT NOT NULL DEFAULT 0,
  downvote_count INT NOT NULL DEFAULT 0,
  score INT NOT NULL DEFAULT 0,           -- upvote_count - downvote_count
  comment_count INT NOT NULL DEFAULT 0,

  CONSTRAINT fk_author FOREIGN KEY (author_did) REFERENCES users(did) ON DELETE CASCADE,
  CONSTRAINT fk_community FOREIGN KEY (community_did) REFERENCES communities(did) ON DELETE CASCADE
);

-- ✅ Implemented indexes
CREATE INDEX idx_posts_community_created ON posts(community_did, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_posts_community_score ON posts(community_did, score DESC, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_posts_author ON posts(author_did, created_at DESC);
CREATE INDEX idx_posts_uri ON posts(uri);

-- ⚠️ Deferred until content rules are implemented:
-- CREATE INDEX idx_posts_embed_type ON posts(community_did, embed_type) WHERE deleted_at IS NULL;
```

#### Votes Table
```sql
CREATE TABLE votes (
  id BIGSERIAL PRIMARY KEY,
  uri TEXT UNIQUE NOT NULL,               -- Vote record AT-URI (at://voter_did/collection/rkey)
  cid TEXT NOT NULL,
  rkey TEXT NOT NULL,
  voter_did TEXT NOT NULL,                -- User who voted (from AT-URI owner)
  subject_uri TEXT NOT NULL,              -- Post/comment AT-URI
  direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
  created_at TIMESTAMPTZ NOT NULL,
  indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  deleted_at TIMESTAMPTZ,

  CONSTRAINT fk_voter FOREIGN KEY (voter_did) REFERENCES users(did),
  UNIQUE (voter_did, subject_uri, deleted_at) -- One active vote per user per subject
);

CREATE INDEX idx_votes_subject ON votes(subject_uri, direction) WHERE deleted_at IS NULL;
CREATE INDEX idx_votes_voter_subject ON votes(voter_did, subject_uri) WHERE deleted_at IS NULL;
```

---

## Alpha Blockers (Must Complete)

### Critical Path
- [x] **Post CREATE Endpoint:** ✅ COMPLETE (2025-10-19)
  - ✅ Handler with authentication and validation
  - ✅ Service layer with business logic
  - ✅ Repository layer for database access
  - ⚠️ Get, Update, Delete deferred to Beta
- [x] **Community Credentials:** ✅ Use community's PDS credentials for post writes
- [x] **Token Refresh Integration:** ✅ Reuse community token refresh logic for post operations
- [x] **Jetstream Consumer:** ✅ Posts indexed in real-time (2025-10-19)
  - ✅ CREATE operations indexed
  - ✅ Security validation (repository ownership)
  - ⚠️ UPDATE/DELETE deferred until those features exist
  - ⚠️ Derived characteristics deferred to Beta
- [x] **Database Migrations:** ✅ Posts table created (migration 011)
- [x] **E2E Tests:** ✅ Full flow tested (handler → community PDS → Jetstream → AppView)
  - ✅ Service layer tests (9 subtests)
  - ✅ Repository tests (2 subtests)
  - ✅ Handler security tests (10+ subtests)
  - ✅ Live PDS + Jetstream E2E test
- [x] **Community Integration:** ✅ Posts correctly reference communities via at-identifiers
- [x] **at-identifier Support:** ✅ All 4 formats supported (DIDs, canonical, @-prefixed, scoped)
- [ ] **Content Rules Validation:** ⚠️ DEFERRED TO BETA - Posts validated against community content rules
- [ ] **Vote System:** - Upvote/downvote with community-level controls
- [ ] **Moderator Permissions:** ⚠️ DEFERRED TO BETA - Community moderators can delete posts

### Testing Requirements
- [x] Create text post in community → Appears in AppView ✅
- [x] Post lives in community's repository (verify AT-URI owner) ✅
- [x] Post written to PDS → Broadcast to Jetstream → Indexed in AppView ✅
- [x] Handler security: Rejects client-provided authorDid ✅
- [x] Handler security: Requires authentication ✅
- [x] Handler security: Validates request body size ✅
- [x] Handler security: All 4 at-identifier formats accepted ✅
- [x] Consumer security: Rejects posts from wrong repository ✅
- [x] Consumer security: Verifies community and author exist ✅
- [ ] **Content rules validation:** Text-only community rejects image posts ⚠️ DEFERRED - Governence
- [ ] **Content rules validation:** Image community rejects posts without images ⚠️ DEFERRED - Governence
- [ ] **Content rules validation:** Post with too-short text rejected ⚠️ DEFERRED - Governence
- [ ] **Content rules validation:** Federated post rejected if `allowFederated: false` ⚠️ DEFERRED - Governence
- [ ] Update post within 24 hours → Edit reflected ⚠️ DEFERRED
- [ ] Delete post as author → Hidden from queries ⚠️ DEFERRED
- [ ] Delete post as moderator → Hidden from queries ⚠️ DEFERRED
- [ ] Upvote post → Count increments
- [ ] Downvote post → Count increments (if enabled)
- [ ] Toggle vote → Counts update correctly ⚠️ DEFERRED - Governence
- [ ] Community with downvotes disabled → Downvote returns error ⚠️ DEFERRED - Governence

---

## Beta Features (Post-Alpha)

### Advanced Post Types
**Status:** Deferred - Simplify for Alpha
**Rationale:** Text posts are sufficient for MVP, other types need more infrastructure

- [ ] **Image Posts:** Full image upload/processing pipeline
  - Multi-image support (up to 4 images)
  - Upload to community's PDS blob storage
  - Thumbnail generation
  - Alt text requirements (accessibility)

- [ ] **Video Posts:** Video hosting and processing
  - Video upload to community's PDS blob storage
  - Thumbnail extraction
  - Format validation
  - Streaming support

- [ ] **Microblog Posts:** Bluesky federation integration
  - Fetch Bluesky posts by AT-URI
  - Display inline with native posts
  - Track original author info
  - Federation metadata

- [ ] **Decision Point:** Remove "Article" type entirely?
  - Obsoleted by planned RSS aggregation service
  - LLMs will break down articles into digestible content
  - May not need native article posting

### Post Interaction Features

#### Tagging System
- [x] Lexicon: `social.coves.interaction.tag` ✅
- [ ] **Known Tags:** helpful, insightful, spam, hostile, offtopic, misleading
- [ ] **Community Custom Tags:** Communities define their own tags
- [ ] **Aggregate Counts:** Track tag distribution on posts
- [ ] **Moderation Integration:** High spam/hostile tags trigger tribunal review
- [ ] **Reputation Impact:** Helpful/insightful tags boost author reputation
- [ ] **Tag Storage:** Tags live in **user's repository** (users own their tags)

#### Crossposting
- [x] Lexicon: `social.coves.post.crosspost` ✅
- [ ] **Crosspost Tracking:** Share post to multiple communities
- [ ] **Implementation:** Create new post record in each community's repository
- [ ] **Crosspost Chain:** Track all crosspost relationships
- [ ] **Deduplication:** Show original + crosspost count (don't spam feeds)
- [ ] **Rules:** Communities can disable crossposting

#### Save Posts
- [ ] **Lexicon:** Create `social.coves.actor.savedPost` record type
- [ ] **Functionality:** Bookmark posts for later reading
- [ ] **Private List:** Saved posts stored in **user's repository**
- [ ] **AppView Query:** Endpoint to fetch user's saved posts

### Post Search
- [x] Lexicon: `social.coves.post.search` ✅
- [ ] **Search Parameters:**
  - Query string (q)
  - Filter by community
  - Filter by author
  - Filter by post type
  - Filter by tags
  - Sort: relevance, new, top
  - Timeframe: hour, day, week, month, year, all
- [ ] **Implementation:**
  - PostgreSQL full-text search (tsvector on title + content)
  - Relevance ranking algorithm
  - Pagination with cursor

### Edit History
- [ ] **Track Edits:** Store edit history in AppView (not in atProto record)
- [ ] **Edit Diff:** Show what changed between versions
- [ ] **Edit Log:** List all edits with timestamps and edit notes
- [ ] **Revision Viewing:** View previous versions of post

### Advanced Voting

#### Vote Weight by Reputation
- [ ] **Reputation Multiplier:** High-reputation users' votes count more
- [ ] **Community-Specific:** Reputation calculated per-community
- [ ] **Transparency:** Show vote weight in moderation logs (not public)

#### Fuzzing & Vote Obfuscation
- [ ] **Count Fuzzing:** Add noise to vote counts (prevent manipulation detection)
- [ ] **Delay Display:** Don't show exact counts for new posts (first hour)
- [ ] **Rate Limiting:** Prevent vote brigading

---

## Future Features

### Federation

#### Bluesky Integration
- [ ] **Display Bluesky Posts:** Show Bluesky posts in community feeds (microblog type)
- [ ] **Original Author Info:** Track Bluesky user metadata
- [ ] **No Native Commenting:** Users see Bluesky posts, can't comment (yet)
- [ ] **Reference Storage:** Store Bluesky AT-URI, don't duplicate content

#### ActivityPub Integration
- [ ] **Lemmy/Mbin Posts:** Convert ActivityPub posts to Coves posts
- [ ] **Bidirectional Sync:** Coves posts appear on Lemmy instances
- [ ] **User Identity Mapping:** Assign DIDs to ActivityPub users
- [ ] **Action Translation:** Upvotes ↔ ActivityPub likes

### Advanced Features

#### Post Scheduling
- [ ] Schedule posts for future publishing
- [ ] Edit scheduled posts before they go live
- [ ] Cancel scheduled posts

#### Post Templates
- [ ] Communities define post templates
- [ ] Auto-fill fields for common post types
- [ ] Game threads, event announcements, etc.

#### Polls
- [ ] Create polls in posts
- [ ] Multiple choice, ranked choice, approval voting
- [ ] Time-limited voting windows
- [ ] Results visualization

#### Location-Based Posting
- [x] Lexicon: `location` field in post record ✅
- [ ] **Geo-Tagging:** Attach coordinates to posts
- [ ] **Community Rules:** Require location for certain posts (local events)
- [ ] **Privacy:** User controls location precision
- [ ] **Discovery:** Filter posts by location

---

## Technical Decisions Log

### 2025-10-18: Content Rules Over Post Type Enum
**Decision:** Remove `postType` from post creation input; validate posts against community's `contentRules` instead

**Rationale:**
- `postType` enum forced users to explicitly select type (bad UX - app should infer from structure)
- Structure-based validation is more flexible ("text required, images optional" vs rigid type categories)
- Content rules are extensible without changing post lexicon
- Enables both community restrictions (governance) AND user filtering (UI preferences)
- Follows atProto philosophy: describe data structure, not UI intent

**Implementation:**
- Post creation no longer accepts `postType` parameter
- Community profile contains optional `contentRules` object
- Handler validates post structure against community's content rules
- AppView indexes derived characteristics (embed_type, text_length, has_title, has_embed)
- Validation error changed from `InvalidPostType` to `ContentRuleViolation`

**Database Changes:**
- Remove `post_type` enum column
- Add derived fields: `embed_type`, `text_length`, `has_title`, `has_embed`
- Add index on `embed_type` for filtering

**Example Rules:**
- Text-only community: `allowedEmbedTypes: []` + `requireText: true`
- Image community: `allowedEmbedTypes: ["images"]` + `minImages: 1`
- No restrictions: `contentRules: null`

**See:** [PRD_GOVERNANCE.md - Content Rules System](PRD_GOVERNANCE.md#content-rules-system)

---

### 2025-10-18: Posts Live in Community Repositories
**Decision:** Posts are stored in community's repository, not user's repository

**Rationale:**
- **Matches V2 Communities Architecture:** Communities own their repositories
- **Traditional Forum Model:** Community owns content, author tracked in metadata
- **Simpler Permissions:** Use community credentials for all post writes
- **Portability:** Posts migrate with community when changing instances
- **Moderation:** Community has full control over content
- **Reuses Token Refresh:** Can leverage existing community credential management

**Implementation Details:**
- Post AT-URI: `at://community_did/social.coves.post.record/tid`
- Write operations use community's PDS credentials (encrypted, stored in AppView)
- Author tracked in post record's `author` field (DID)
- Moderators can delete any post in their community
- Token refresh reuses community's refresh logic

**Trade-offs vs User-Owned Posts:**
- ❌ Users can't take posts when leaving community/instance
- ❌ Less "web3" (content not user-owned)
- ✅ Traditional forum UX (users expect community to own content)
- ✅ Simpler implementation (one credential store per community)
- ✅ Easier moderation (community has full control)
- ✅ Posts move with community during migration

**Comparison to Bluesky:**
- Bluesky: Users own posts (posts in user repo)
- Coves: Communities own posts (posts in community repo)
- This is acceptable - different platforms, different models
- Still atProto-compliant (just different ownership pattern)

---

### 2025-10-18: Votes Live in User Repositories
**Decision:** Vote records are stored in user's repository, not community's

**Rationale:**
- Users own their voting history (personal preference)
- Matches Bluesky pattern (likes in user's repo)
- Enables portable voting history across instances
- User controls their own voting record

**Implementation Details:**
- Vote AT-URI: `at://user_did/social.coves.interaction.vote/tid`
- Write operations use user's PDS credentials
- Subject field references post AT-URI (in community's repo)
- Consumer aggregates votes from all users into post stats

---

### 2025-10-18: Simplify Post Types for Alpha
**Decision:** Launch with text posts only, defer other embed types to Beta
**Status:** SUPERSEDED by content rules approach (see above)

**Rationale:**
- Text posts are sufficient for forum discussions (core use case)
- Image/video embeds require additional infrastructure (blob storage, processing)
- Article format can be handled with long-form text posts
- Microblog type is for Bluesky federation (not immediate priority)
- Simplicity accelerates alpha launch

**Updated Approach (2025-10-18):**
- Post structure determines "type" (not explicit enum)
- Communities use `contentRules` to restrict embed types
- AppView derives `embed_type` from post structure for filtering
- More flexible than rigid type system

---

### 2025-10-18: Include Downvotes with Community Controls
**Decision:** Support both upvotes and downvotes, with toggles to disable downvotes

**Rationale:**
- Downvotes provide valuable signal for content quality
- Some communities prefer upvote-only (toxic negativity concerns)
- Instance operators should have global control option
- Reddit/HN have proven downvotes work with good moderation

**Implementation:**
- Community-level: `allowDownvotes` boolean in community profile
- Instance-level: Environment variable `ALLOW_DOWNVOTES` (future)
- Downvote attempts on disabled communities return error
- Stats show 0 downvotes when disabled

---

### 2025-10-18: 24-Hour Edit Window (Hardcoded for Alpha)
**Decision:** Posts can be edited for 24 hours after creation

**Rationale:**
- Allows fixing typos and errors
- Prevents historical revisionism (can't change old posts)
- 24 hours balances flexibility with integrity
- Future: Community-configurable edit windows

**Future Enhancements:**
- Edit history tracking (show what changed)
- Community-specific edit windows (0-72 hours)
- Moderator override (edit any post)

---

### 2025-10-18: Comments Separate from Posts PRD
**Decision:** Comments get their own dedicated PRD

**Rationale:**
- Comments are complex enough to warrant separate planning
- Threaded replies, vote inheritance, moderation all need design
- Posts are usable without comments (voting, tagging still work)
- Allows shipping posts sooner

**Scope Boundary:**
- **Posts PRD:** Post CRUD, voting, tagging, search
- **Comments PRD:** Comment threads, reply depth, sorting, moderation

---

### 2025-10-18: Feeds Separate from Posts PRD
**Decision:** Feed generation gets its own PRD

**Rationale:**
- Feed algorithms are complex (ranking, personalization, filtering)
- Posts need to exist before feeds can be built
- Feed work includes: Home feed, Community feed, All feed, read state tracking
- Allows iterating on feed algorithms independently

**Scope Boundary:**
- **Posts PRD:** Post creation, indexing, retrieval
- **Feeds PRD:** Feed generation, ranking algorithms, read state, personalization

---

## Success Metrics

### Alpha Launch Checklist ✅ COMPLETE (2025-10-19)
- [x] Users can create text posts in communities ✅
- [x] Posts are stored in community's repository (verify AT-URI) ✅
- [x] Posts use community's PDS credentials for writes ✅
- [x] Posts are indexed from firehose within 1 second ✅ (real-time Jetstream)
- [x] E2E tests cover full write-forward flow ✅
- [x] Database handles posts without performance issues ✅
- [x] Handler security tests passing (authentication, validation, body size) ✅
- [x] Consumer security validation (repository ownership, community/author checks) ✅
- [x] All 4 at-identifier formats supported ✅

### Beta Checklist (TODO)
- [ ] Post editing works within 24-hour window ⚠️ DEFERRED
- [ ] Upvote/downvote system functional ⚠️ DEFERRED
- [ ] Community downvote toggle works ⚠️ DEFERRED
- [ ] Post deletion soft-deletes and hides from queries ⚠️ DEFERRED
- [ ] Moderators can delete posts in their community ⚠️ DEFERRED
- [ ] Get post endpoint returns full post view with stats ⚠️ DEFERRED
- [ ] Content rules validation working ⚠️ DEFERRED
- [ ] Database handles 100,000+ posts (load testing)

### Beta Goals
- [ ] All post types supported (text, image, video, microblog)
- [ ] Tagging system enables community moderation
- [ ] Post search returns relevant results
- [ ] Edit history tracked and viewable
- [ ] Crossposting works across communities
- [ ] Save posts feature functional

### V1 Goals
- [ ] Bluesky posts display inline (federation)
- [ ] Vote fuzzing prevents manipulation
- [ ] Reputation affects vote weight
- [ ] Location-based posting for local communities
- [ ] Post templates reduce friction for common posts

---

## Related Documents

- [PRD_COMMUNITIES.md](PRD_COMMUNITIES.md) - Community system (posts require communities)
- [DOMAIN_KNOWLEDGE.md](DOMAIN_KNOWLEDGE.md) - Overall platform architecture
- [PRD_GOVERNANCE.md](PRD_GOVERNANCE.md) - Moderation and tagging systems
- **PRD_COMMENTS.md** (TODO) - Comment threading and replies
- **PRD_FEEDS.md** (TODO) - Feed generation and ranking algorithms

---

## Lexicon Summary

### `social.coves.post.record`
**Status:** ✅ Defined, implementation TODO
**Last Updated:** 2025-10-18 (removed `postType` enum)

**Required Fields:**
- `community` - DID of community (owner of repository)
- `createdAt` - Timestamp

**Optional Fields:**
- `title` - Post title (300 graphemes / 3000 bytes)
- `content` - Post content (50,000 characters max)
- `facets` - Rich text annotations
- `embed` - Images, video, external links, quoted posts (union type)
- `contentLabels` - Self-applied labels (nsfw, spoiler, violence)
- `originalAuthor` - For microblog posts (federated author info)
- `federatedFrom` - Reference to federated post
- `location` - Geographic coordinates
- `crosspostOf` - AT-URI of original post
- `crosspostChain` - Array of crosspost URIs

**Notes:**
- Author DID is inferred from the creation context (authenticated user), not stored in record
- Post "type" is derived from structure (has embed? what embed type? has title? text length?)
- Community's `contentRules` validate post structure at creation time

### `social.coves.post.create` (Procedure)
**Status:** ✅ Defined, implementation TODO
**Last Updated:** 2025-10-18 (removed `postType` parameter)

**Input Parameters:**
- `community` (required) - DID or handle of community to post in
- `title` (optional) - Post title
- `content` (optional) - Post content
- `facets` (optional) - Rich text annotations
- `embed` (optional) - Embedded content (images, video, external, post)
- `contentLabels` (optional) - Self-applied labels
- `originalAuthor` (optional) - For federated posts
- `federatedFrom` (optional) - Reference to federated post
- `location` (optional) - Geographic coordinates

**Validation:**
- Community exists and is accessible
- Post structure complies with community's `contentRules`
- Content within global limits (unless community sets stricter limits)

**Errors:**
- `CommunityNotFound` - Community doesn't exist
- `NotAuthorized` - User not authorized to post
- `Banned` - User is banned from community
- `InvalidContent` - Content violates general rules
- `ContentRuleViolation` - Post violates community's content rules

---

### `social.coves.interaction.vote`
**Status:** ✅ Defined, implementation TODO

**Fields:**
- `subject` - AT-URI of post/comment being voted on
- `createdAt` - Timestamp

**Note:** Direction (up/down) inferred from record creation/deletion pattern. Stored in user's repository (user owns votes).

### `social.coves.interaction.tag`
**Status:** ✅ Defined, deferred to Beta

**Fields:**
- `subject` - AT-URI of post/comment
- `tag` - Tag string (known values: helpful, insightful, spam, hostile, offtopic, misleading)
- `createdAt` - Timestamp

**Note:** Tags live in user's repository (users own their tags).

---

## References

- atProto Lexicon Spec: https://atproto.com/specs/lexicon
- atProto Repository Spec: https://atproto.com/specs/repository
- Bluesky Post Record: https://github.com/bluesky-social/atproto/blob/main/lexicons/app/bsky/feed/post.json
- Rich Text Facets: https://atproto.com/specs/rich-text
- Coves V2 Communities Architecture: [PRD_COMMUNITIES.md](PRD_COMMUNITIES.md)
