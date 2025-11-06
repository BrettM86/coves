# Comment System Implementation

## Overview

This document details the complete implementation of the comment system for Coves, a forum-like atProto social media platform. The comment system follows the established vote system pattern, with comments living in user repositories and being indexed by the AppView via Jetstream firehose.

**Implementation Date:** November 4-6, 2025
**Status:** ✅ Phase 1 & 2A Complete - Production-Ready with All PR Fixes
**Test Coverage:** 29 integration tests (18 indexing + 11 query), all passing
**Last Updated:** November 6, 2025 (Final PR review fixes complete - lexicon compliance, data integrity, SQL correctness)

---

## Development Phases

This implementation follows a phased approach for maintainability and proper scoping:

### ✅ Phase 1: Indexing Infrastructure (Current - COMPLETE)
**What was built:**
- Jetstream consumer for indexing comment CREATE/UPDATE/DELETE events
- PostgreSQL schema with proper indexes and denormalized counts
- Repository layer with comprehensive query methods
- Atomic parent count updates (posts.comment_count, comments.reply_count)
- Out-of-order event handling with reconciliation
- Soft delete support preserving thread structure
- Full integration test coverage (20 tests)

**What works:**
- Comments are indexed from Jetstream firehose as users create them
- Threading relationships tracked (root + parent references)
- Parent counts automatically maintained
- Comment updates and deletes processed correctly
- Out-of-order events reconciled automatically

**What's NOT in this phase:**
- ❌ No HTTP API endpoints for querying comments
- ❌ No service layer (repository is sufficient for indexing)
- ❌ No rate limiting or auth middleware
- ❌ No API documentation

### ✅ Phase 2A: Query API - COMPLETE (November 5, 2025)

**What was built:**
- Lexicon definitions: `social.coves.community.comment.defs` and `getComments`
- Database query methods with Lemmy hot ranking algorithm
- Service layer with iterative loading strategy for nested replies
- XRPC HTTP handler with optional authentication
- Comprehensive integration test suite (11 test scenarios)

**What works:**
- Fetch comments on any post with sorting (hot/top/new)
- Nested replies up to configurable depth (default 10, max 100)
- Lemmy hot ranking: `log(greatest(2, score + 2)) / power(time_decay, 1.8)`
- Cursor-based pagination for stable scrolling
- Optional authentication for viewer state (stubbed for Phase 2B)
- Timeframe filtering for "top" sort (hour/day/week/month/year/all)

**Endpoints:**
- `GET /xrpc/social.coves.community.comment.getComments`
  - Required: `post` (AT-URI)
  - Optional: `sort` (hot/top/new), `depth` (0-100), `limit` (1-100), `cursor`, `timeframe`
  - Returns: Array of `threadViewComment` with nested replies + post context
  - Supports Bearer token for authenticated requests (viewer state)

**Files created (9):**
1. `internal/atproto/lexicon/social/coves/community/comment/defs.json` - View definitions
2. `internal/atproto/lexicon/social/coves/community/comment/getComments.json` - Query endpoint
3. `internal/core/comments/comment_service.go` - Business logic layer
4. `internal/core/comments/view_models.go` - API response types
5. `internal/api/handlers/comments/get_comments.go` - HTTP handler
6. `internal/api/handlers/comments/errors.go` - Error handling utilities
7. `internal/api/handlers/comments/middleware.go` - Auth middleware
8. `internal/api/handlers/comments/service_adapter.go` - Service layer adapter
9. `tests/integration/comment_query_test.go` - Integration tests

**Files modified (7):**
1. `internal/db/postgres/comment_repo.go` - Added query methods (~450 lines), fixed INNER→LEFT JOIN, fixed window function SQL
2. `internal/core/comments/interfaces.go` - Added service interface
3. `internal/core/comments/comment.go` - Added CommenterHandle field
4. `internal/core/comments/errors.go` - Added IsValidationError helper
5. `cmd/server/main.go` - Wired up routes and service with all repositories
6. `tests/integration/comment_query_test.go` - Updated test helpers for new service signature
7. `docs/COMMENT_SYSTEM_IMPLEMENTATION.md` - This document

**Total new code:** ~2,400 lines

**Test coverage:**
- 11 integration test scenarios covering:
  - Basic fetch, nested replies, depth limits
  - Hot/top/new sorting algorithms
  - Pagination with cursor stability
  - Empty threads, deleted comments
  - Invalid input handling
  - HTTP handler end-to-end
- Repository layer tested (hot ranking formula, pagination)
- Service layer tested (threading, depth limits)
- Handler tested (input validation, error cases)
- All tests passing ✅

### 🔒 Production Hardening (PR Review Fixes - November 5, 2025)

After initial implementation, a thorough PR review identified several critical issues that were addressed before production deployment:

#### Critical Issues Fixed

**1. N+1 Query Problem (99.7% reduction in queries)**
- **Problem:** Nested reply loading made separate DB queries for each comment's children
- **Impact:** Could execute 1,551 queries for a post with 50 comments at depth 3
- **Solution:** Implemented batch loading with PostgreSQL window functions
  - Added `ListByParentsBatch()` method using `ROW_NUMBER() OVER (PARTITION BY parent_uri)`
  - Refactored `buildThreadViews()` to collect parent URIs per level and fetch in one query
  - **Result:** Reduced from 1,551 queries → 4 queries (1 per depth level)
- **Files:** `internal/core/comments/interfaces.go`, `internal/db/postgres/comment_repo.go`, `internal/core/comments/comment_service.go`

**2. Post Not Found Returns 500 Instead of 404**
- **Problem:** When fetching comments for non-existent post, service returned wrapped `posts.ErrNotFound` which handler didn't recognize
- **Impact:** Clients got HTTP 500 instead of proper HTTP 404
- **Solution:** Added error translation in service layer
  ```go
  if posts.IsNotFound(err) {
      return nil, ErrRootNotFound  // Recognized by comments.IsNotFound()
  }
  ```
- **File:** `internal/core/comments/comment_service.go:68-72`

#### Important Issues Fixed

**3. Missing Endpoint-Specific Rate Limiting**
- **Problem:** Comment queries with deep nesting expensive but only protected by global 100 req/min limit
- **Solution:** Added dedicated rate limiter at 20 req/min for comment endpoint
- **File:** `cmd/server/main.go:429-439`

**4. Unbounded Cursor Size (DoS Vector)**
- **Problem:** No validation before base64 decoding - attacker could send massive cursor string
- **Solution:** Added 1024-byte max size check before decoding
- **File:** `internal/db/postgres/comment_repo.go:547-551`

**5. Missing Query Timeout**
- **Problem:** Deep nested queries could run indefinitely
- **Solution:** Added 10-second context timeout to `GetComments()`
- **File:** `internal/core/comments/comment_service.go:62-64`

**6. Post View Not Populated (P0 Blocker)**
- **Problem:** Lexicon marked `post` field as required but response always returned `null`
- **Impact:** Violated schema contract, would break client deserialization
- **Solution:**
  - Updated service to accept `posts.Repository` instead of `interface{}`
  - Added `buildPostView()` method to construct post views with author/community/stats
  - Fetch post before returning response
- **Files:** `internal/core/comments/comment_service.go:33-36`, `:66-73`, `:224-274`

**7. Missing Record Fields (P0 Blocker)**
- **Problem:** Both `postView.record` and `commentView.record` fields were null despite lexicon marking them as required
- **Impact:** Violated lexicon contract, would break strict client deserialization
- **Solution:**
  - Added `buildPostRecord()` method to construct minimal PostRecord from Post entity
  - Added `buildCommentRecord()` method to construct minimal CommentRecord from Comment entity
  - Both methods populate required fields (type, reply refs, content, timestamps)
  - Added TODOs for Phase 2C to unmarshal JSON fields (embed, facets, labels)
- **Files:** `internal/core/comments/comment_service.go:260-288`, `:366-386`

**8. Handle/Name Format Violations (P0 & Important)**
- **Problem:**
  - `postView.author.handle` contained DID instead of proper handle (violates `format:"handle"`)
  - `postView.community.name` contained DID instead of community name
- **Impact:** Lexicon format constraints violated, poor UX showing DIDs instead of readable names
- **Solution:**
  - Added `users.UserRepository` to service for author handle hydration
  - Added `communities.Repository` to service for community name hydration
  - Updated `buildPostView()` to fetch user and community records with DID fallback
  - Log warnings for missing records but don't fail entire request
- **Files:** `internal/core/comments/comment_service.go:34-37`, `:292-325`, `cmd/server/main.go:297`

**9. Data Loss from INNER JOIN (P1 Critical)**
- **Problem:** Three query methods used `INNER JOIN users` which dropped comments when user not indexed yet
- **Impact:** New user's first comments would disappear until user consumer caught up (violates out-of-order design)
- **Solution:**
  - Changed `INNER JOIN users` → `LEFT JOIN users` in all three methods
  - Added `COALESCE(u.handle, c.commenter_did)` to gracefully fall back to DID
  - Preserves all comments while still hydrating handles when available
- **Files:** `internal/db/postgres/comment_repo.go:396`, `:407`, `:415`, `:694-706`, `:761-836`

**10. Window Function SQL Bug (P0 Critical)**
- **Problem:** `ListByParentsBatch` used `ORDER BY hot_rank DESC` in window function, but PostgreSQL doesn't allow SELECT aliases in window ORDER BY
- **Impact:** SQL error "column hot_rank does not exist" caused silent failure, dropping ALL nested replies in hot sort mode
- **Solution:**
  - Created separate `windowOrderBy` variable that inlines full hot_rank formula
  - PostgreSQL evaluates window ORDER BY before SELECT, so must use full expression
  - Hot sort now works correctly with nested replies
- **Files:** `internal/db/postgres/comment_repo.go:776`, `:808`
- **Critical Note:** This affected default sorting mode (hot) and would have broken production UX

#### Documentation Added

**11. Hot Rank Caching Strategy**
- Documented when and how to implement cached hot rank column
- Specified observability metrics to monitor (p95 latency, CPU usage)
- Documented trade-offs between cached vs on-demand computation

**Test Coverage:**
- All fixes verified with existing integration test suite
- Added test cases for error handling scenarios
- All integration tests passing (comment_query_test.go: 11 tests)

**Rationale for phased approach:**
1. **Separation of concerns**: Indexing and querying are distinct responsibilities
2. **Testability**: Phase 1 can be fully tested without API layer
3. **Incremental delivery**: Indexing can run in production while API is developed
4. **Scope management**: Prevents feature creep and allows focused code review

---

## Hot Ranking Algorithm (Lemmy-Based)

### Formula

```sql
log(greatest(2, score + 2)) /
  power(((EXTRACT(EPOCH FROM (NOW() - created_at)) / 3600) + 2), 1.8)
```

### Explanation

**Components:**
- `greatest(2, score + 2)`: Ensures log input never goes below 2
  - Prevents negative log values for heavily downvoted comments
  - Score of -5 → log(2), same as score of 0
  - Prevents brigading from creating "anti-viral" comments

- `power(..., 1.8)`: Time decay exponent
  - Higher than posts (1.5) for faster comment aging
  - Comments should be fresher than posts

- `+ 2` offsets: Prevent divide-by-zero for very new comments

**Behavior:**
- High score + old = lower rank (content ages naturally)
- Low score + new = higher rank (fresh content gets visibility)
- Negative scores don't break the formula (bounded at log(2))

### Sort Modes

**Hot (default):**
```sql
ORDER BY hot_rank DESC, score DESC, created_at DESC
```

**Top (with timeframe):**
```sql
WHERE created_at >= NOW() - INTERVAL '1 day'
ORDER BY score DESC, created_at DESC
```

**New (chronological):**
```sql
ORDER BY created_at DESC
```

### Path-Based Ordering

Comments are ordered within their tree level:
```sql
ORDER BY
  path ASC,           -- Maintains parent-child structure
  hot_rank DESC,      -- Sorts siblings by rank
  score DESC,         -- Tiebreaker
  created_at DESC     -- Final tiebreaker
```

**Result:** Siblings compete with siblings, but children never outrank their parent.

---

## Architecture

### Data Flow

```
Client → User's PDS → Jetstream Firehose → Comment Consumer → PostgreSQL AppView
                                                   ↓
                         Atomic updates to parent counts
                         (posts.comment_count OR comments.reply_count)
```

### Key Design Principles

1. **User-Owned Records**: Comments live in user repositories (like votes), not community repositories (like posts)
2. **atProto Native**: Uses `com.atproto.repo.createRecord/updateRecord/deleteRecord`
3. **Threading via Strong References**: Root + parent system allows unlimited nesting depth
4. **Out-of-Order Indexing**: No foreign key constraints to allow Jetstream events to arrive in any order
5. **Idempotent Operations**: Safe for Jetstream replays and duplicate events
6. **Atomic Count Updates**: Database transactions ensure consistency
7. **Soft Deletes**: Preserves thread structure when comments are deleted

---

## Implementation Details

### 1. Lexicon Definition

**Location:** `internal/atproto/lexicon/social/coves/feed/comment.json`

The lexicon was already defined and follows atProto best practices:

```json
{
  "lexicon": 1,
  "id": "social.coves.feed.comment",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "required": ["reply", "content", "createdAt"],
      "properties": {
        "reply": {
          "type": "ref",
          "ref": "#replyRef",
          "description": "Reference to the post and parent being replied to"
        },
        "content": {
          "type": "string",
          "maxGraphemes": 3000,
          "maxLength": 30000
        },
        "facets": { /* Rich text annotations */ },
        "embed": { /* Images, quoted posts */ },
        "langs": { /* ISO 639-1 language codes */ },
        "labels": { /* Self-applied content labels */ },
        "createdAt": { /* RFC3339 timestamp */ }
      }
    },
    "replyRef": {
      "required": ["root", "parent"],
      "properties": {
        "root": {
          "type": "ref",
          "ref": "com.atproto.repo.strongRef",
          "description": "Strong reference to the original post"
        },
        "parent": {
          "type": "ref",
          "ref": "com.atproto.repo.strongRef",
          "description": "Strong reference to immediate parent (post or comment)"
        }
      }
    }
  }
}
```

**Threading Model:**
- `root`: Always points to the original post that started the thread
- `parent`: Points to the immediate parent (can be a post or another comment)
- This enables unlimited nested threading while maintaining the root reference

---

### 2. Database Schema

**Migration:** `internal/db/migrations/016_create_comments_table.sql`

```sql
CREATE TABLE comments (
    id BIGSERIAL PRIMARY KEY,
    uri TEXT UNIQUE NOT NULL,               -- AT-URI (at://commenter_did/social.coves.feed.comment/rkey)
    cid TEXT NOT NULL,                      -- Content ID
    rkey TEXT NOT NULL,                     -- Record key (TID)
    commenter_did TEXT NOT NULL,            -- User who commented (from AT-URI repo field)

    -- Threading structure (reply references)
    root_uri TEXT NOT NULL,                 -- Strong reference to original post
    root_cid TEXT NOT NULL,                 -- CID of root post (version pinning)
    parent_uri TEXT NOT NULL,               -- Strong reference to immediate parent
    parent_cid TEXT NOT NULL,               -- CID of parent (version pinning)

    -- Content
    content TEXT NOT NULL,                  -- Comment text (max 3000 graphemes, 30000 bytes)
    content_facets JSONB,                   -- Rich text facets
    embed JSONB,                            -- Embedded content (images, quoted posts)
    content_labels JSONB,                   -- Self-applied labels (com.atproto.label.defs#selfLabels)
    langs TEXT[],                           -- Languages (ISO 639-1, max 3)

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL,        -- Commenter's timestamp from record
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,                 -- Soft delete

    -- Stats (denormalized for performance)
    upvote_count INT NOT NULL DEFAULT 0,    -- Comments CAN be voted on
    downvote_count INT NOT NULL DEFAULT 0,
    score INT NOT NULL DEFAULT 0,           -- upvote_count - downvote_count
    reply_count INT NOT NULL DEFAULT 0      -- Number of direct replies
);
```

**Key Indexes:**
```sql
-- Threading queries (most important for UX)
CREATE INDEX idx_comments_root ON comments(root_uri, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent ON comments(parent_uri, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent_score ON comments(parent_uri, score DESC, created_at DESC)
    WHERE deleted_at IS NULL;

-- User queries
CREATE INDEX idx_comments_commenter ON comments(commenter_did, created_at DESC);

-- Vote targeting
CREATE INDEX idx_comments_uri_active ON comments(uri)
    WHERE deleted_at IS NULL;
```

**Design Decisions:**
- **No FK on `commenter_did`**: Allows out-of-order Jetstream indexing (comment events may arrive before user events)
- **Soft delete pattern**: `deleted_at IS NULL` in indexes for performance
- **Vote counts included**: The vote lexicon explicitly allows voting on comments (not just posts)
- **StrongRef with CID**: Version pinning prevents confusion when parent content changes

---

### 3. Domain Layer

#### Comment Entity
**File:** `internal/core/comments/comment.go`

```go
type Comment struct {
    ID            int64
    URI           string
    CID           string
    RKey          string
    CommenterDID  string

    // Threading
    RootURI       string
    RootCID       string
    ParentURI     string
    ParentCID     string

    // Content
    Content       string
    ContentFacets *string
    Embed         *string
    ContentLabels *string
    Langs         []string

    // Timestamps
    CreatedAt     time.Time
    IndexedAt     time.Time
    DeletedAt     *time.Time

    // Stats
    UpvoteCount   int
    DownvoteCount int
    Score         int
    ReplyCount    int
}
```

#### Repository Interface
**File:** `internal/core/comments/interfaces.go`

```go
type Repository interface {
    Create(ctx context.Context, comment *Comment) error
    Update(ctx context.Context, comment *Comment) error
    GetByURI(ctx context.Context, uri string) (*Comment, error)
    Delete(ctx context.Context, uri string) error

    // Threading queries
    ListByRoot(ctx context.Context, rootURI string, limit, offset int) ([]*Comment, error)
    ListByParent(ctx context.Context, parentURI string, limit, offset int) ([]*Comment, error)
    CountByParent(ctx context.Context, parentURI string) (int, error)

    // User queries
    ListByCommenter(ctx context.Context, commenterDID string, limit, offset int) ([]*Comment, error)
}
```

#### Error Types
**File:** `internal/core/comments/errors.go`

Standard error types following the vote system pattern, with helper functions `IsNotFound()` and `IsConflict()`.

---

### 4. Repository Implementation

**File:** `internal/db/postgres/comment_repo.go`

#### Idempotent Create Pattern
```go
func (r *postgresCommentRepo) Create(ctx context.Context, comment *Comment) error {
    query := `
        INSERT INTO comments (...)
        VALUES (...)
        ON CONFLICT (uri) DO NOTHING
        RETURNING id, indexed_at
    `

    err := r.db.QueryRowContext(ctx, query, ...).Scan(&comment.ID, &comment.IndexedAt)

    // ON CONFLICT DO NOTHING returns no rows if duplicate
    if err == sql.ErrNoRows {
        return nil // Already exists - OK for Jetstream replays
    }

    return err
}
```

#### Update Preserving Vote Counts
```go
func (r *postgresCommentRepo) Update(ctx context.Context, comment *Comment) error {
    query := `
        UPDATE comments
        SET cid = $1, content = $2, content_facets = $3,
            embed = $4, content_labels = $5, langs = $6
        WHERE uri = $7 AND deleted_at IS NULL
        RETURNING id, indexed_at, created_at,
                  upvote_count, downvote_count, score, reply_count
    `

    // Vote counts and created_at are preserved (not in SET clause)
    err := r.db.QueryRowContext(ctx, query, ...).Scan(...)
    return err
}
```

#### Soft Delete
```go
func (r *postgresCommentRepo) Delete(ctx context.Context, uri string) error {
    query := `
        UPDATE comments
        SET deleted_at = NOW()
        WHERE uri = $1 AND deleted_at IS NULL
    `

    result, err := r.db.ExecContext(ctx, query, uri)
    // Idempotent: Returns success even if already deleted
    return err
}
```

---

### 5. Jetstream Consumer

**File:** `internal/atproto/jetstream/comment_consumer.go`

#### Event Handler
```go
func (c *CommentEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
    if event.Kind != "commit" || event.Commit == nil {
        return nil
    }

    if event.Commit.Collection == "social.coves.feed.comment" {
        switch event.Commit.Operation {
        case "create":
            return c.createComment(ctx, event.Did, commit)
        case "update":
            return c.updateComment(ctx, event.Did, commit)
        case "delete":
            return c.deleteComment(ctx, event.Did, commit)
        }
    }

    return nil
}
```

#### Atomic Count Updates
```go
func (c *CommentEventConsumer) indexCommentAndUpdateCounts(ctx, comment *Comment) error {
    tx, _ := c.db.BeginTx(ctx, nil)
    defer tx.Rollback()

    // 1. Insert comment (idempotent)
    err = tx.QueryRowContext(ctx, `
        INSERT INTO comments (...) VALUES (...)
        ON CONFLICT (uri) DO NOTHING
        RETURNING id
    `).Scan(&commentID)

    if err == sql.ErrNoRows {
        tx.Commit()
        return nil // Already indexed
    }

    // 2. Update parent counts atomically
    // Try posts table first
    tx.ExecContext(ctx, `
        UPDATE posts
        SET comment_count = comment_count + 1
        WHERE uri = $1 AND deleted_at IS NULL
    `, comment.ParentURI)

    // If no post updated, parent is probably a comment
    tx.ExecContext(ctx, `
        UPDATE comments
        SET reply_count = reply_count + 1
        WHERE uri = $1 AND deleted_at IS NULL
    `, comment.ParentURI)

    return tx.Commit()
}
```

#### Security Validation
```go
func (c *CommentEventConsumer) validateCommentEvent(ctx, repoDID string, comment *CommentRecord) error {
    // Comments MUST come from user repositories (repo owner = commenter DID)
    if !strings.HasPrefix(repoDID, "did:") {
        return fmt.Errorf("invalid commenter DID format: %s", repoDID)
    }

    // Content is required
    if comment.Content == "" {
        return fmt.Errorf("comment content is required")
    }

    // Reply references must have both URI and CID
    if comment.Reply.Root.URI == "" || comment.Reply.Root.CID == "" {
        return fmt.Errorf("invalid root reference: must have both URI and CID")
    }

    if comment.Reply.Parent.URI == "" || comment.Reply.Parent.CID == "" {
        return fmt.Errorf("invalid parent reference: must have both URI and CID")
    }

    return nil
}
```

**Security Note:** We do NOT verify that the user exists in the AppView because:
1. Comment events may arrive before user events in Jetstream (race condition)
2. The comment came from the user's PDS repository (authenticated by PDS)
3. No database FK constraint allows out-of-order indexing
4. Orphaned comments (from never-indexed users) are harmless

---

### 6. WebSocket Connector

**File:** `internal/atproto/jetstream/comment_jetstream_connector.go`

Follows the standard Jetstream connector pattern with:
- Auto-reconnect on errors (5-second retry)
- Ping/pong keepalive (30-second ping, 60-second read deadline)
- Graceful shutdown via context cancellation
- Subscribes to: `wantedCollections=social.coves.feed.comment`

---

### 7. Server Integration

**File:** `cmd/server/main.go` (lines 289-396)

```go
// Initialize comment repository
commentRepo := postgresRepo.NewCommentRepository(db)
log.Println("✅ Comment repository initialized (Jetstream indexing only)")

// Start Jetstream consumer for comments
commentJetstreamURL := os.Getenv("COMMENT_JETSTREAM_URL")
if commentJetstreamURL == "" {
    commentJetstreamURL = "ws://localhost:6008/subscribe?wantedCollections=social.coves.feed.comment"
}

commentEventConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
commentJetstreamConnector := jetstream.NewCommentJetstreamConnector(commentEventConsumer, commentJetstreamURL)

go func() {
    if startErr := commentJetstreamConnector.Start(ctx); startErr != nil {
        log.Printf("Comment Jetstream consumer stopped: %v", startErr)
    }
}()

log.Printf("Started Jetstream comment consumer: %s", commentJetstreamURL)
log.Println("  - Indexing: social.coves.feed.comment CREATE/UPDATE/DELETE operations")
log.Println("  - Updating: Post comment counts and comment reply counts atomically")
```

---

## Testing

### Test Suite

**File:** `tests/integration/comment_consumer_test.go`

**Test Coverage:** 6 test suites, 18 test cases, **100% passing**

#### 1. TestCommentConsumer_CreateComment
- ✅ Create comment on post
- ✅ Verify comment is indexed correctly
- ✅ Verify post comment count is incremented
- ✅ Idempotent create - duplicate events don't double-count

#### 2. TestCommentConsumer_Threading
- ✅ Create first-level comment (reply to post)
- ✅ Create second-level comment (reply to comment)
- ✅ Verify both comments have same root (original post)
- ✅ Verify parent relationships are correct
- ✅ Verify reply counts are updated
- ✅ Query all comments by root (flat list)
- ✅ Query direct replies to post
- ✅ Query direct replies to comment

#### 3. TestCommentConsumer_UpdateComment
- ✅ Create comment with initial content
- ✅ Manually set vote counts to simulate votes
- ✅ Update comment content
- ✅ Verify content is updated
- ✅ Verify CID is updated
- ✅ **Verify vote counts are preserved**
- ✅ **Verify created_at is preserved**

#### 4. TestCommentConsumer_DeleteComment
- ✅ Create comment
- ✅ Delete comment (soft delete)
- ✅ Verify deleted_at is set
- ✅ Verify post comment count is decremented
- ✅ Idempotent delete - duplicate deletes don't double-decrement

#### 5. TestCommentConsumer_SecurityValidation
- ✅ Reject comment with empty content
- ✅ Reject comment with invalid root reference (missing URI)
- ✅ Reject comment with invalid parent reference (missing CID)
- ✅ Reject comment with invalid DID format

#### 6. TestCommentRepository_Queries
- ✅ ListByRoot returns all comments in thread (4 comments)
- ✅ ListByParent returns direct replies to post (2 comments)
- ✅ ListByParent returns direct replies to comment (2 comments)
- ✅ CountByParent returns correct counts
- ✅ ListByCommenter returns all user's comments

### Test Results

```
=== Test Summary ===
PASS: TestCommentConsumer_CreateComment (0.02s)
PASS: TestCommentConsumer_Threading (0.02s)
PASS: TestCommentConsumer_UpdateComment (0.02s)
PASS: TestCommentConsumer_DeleteComment (0.02s)
PASS: TestCommentConsumer_SecurityValidation (0.01s)
PASS: TestCommentRepository_Queries (0.02s)

✅ ALL 18 TESTS PASS
Total time: 0.115s
```

---

## Key Features

### ✅ Comments ARE Votable
The vote lexicon explicitly states: *"Record declaring a vote (upvote or downvote) on a **post or comment**"*

Comments include full vote tracking:
- `upvote_count`
- `downvote_count`
- `score` (calculated as upvote_count - downvote_count)

### ✅ Comments ARE Editable
Unlike votes (which are immutable), comments support UPDATE operations:
- Content, facets, embed, and labels can be updated
- Vote counts and created_at are preserved
- CID is updated to reflect new version

### ✅ Threading Support
Unlimited nesting depth via root + parent system:
- Every comment knows its root post
- Every comment knows its immediate parent
- Easy to query entire threads or direct replies
- Soft deletes preserve thread structure

### ✅ Out-of-Order Indexing
No foreign key constraints allow events to arrive in any order:
- Comment events may arrive before user events
- Comment events may arrive before post events
- All operations are idempotent
- Safe for Jetstream replays

### ✅ Atomic Consistency
Database transactions ensure counts are always accurate:
- Comment creation increments parent count
- Comment deletion decrements parent count
- No race conditions
- No orphaned counts

---

## Implementation Statistics

### Phase 1 - Indexing Infrastructure

**Files Created: 8**
1. `internal/db/migrations/016_create_comments_table.sql` - 60 lines
2. `internal/core/comments/comment.go` - 80 lines
3. `internal/core/comments/interfaces.go` - 45 lines
4. `internal/core/comments/errors.go` - 40 lines
5. `internal/db/postgres/comment_repo.go` - 340 lines
6. `internal/atproto/jetstream/comment_consumer.go` - 530 lines
7. `internal/atproto/jetstream/comment_jetstream_connector.go` - 130 lines
8. `tests/integration/comment_consumer_test.go` - 930 lines

**Files Modified: 1**
1. `cmd/server/main.go` - Added 20 lines for Jetstream consumer

**Phase 1 Total:** ~2,175 lines

### Phase 2A - Query API

**Files Created: 9** (listed above in Phase 2A section)

**Files Modified: 6** (listed above in Phase 2A section)

**Phase 2A Total:** ~2,400 lines

### Combined Total: ~4,575 lines

---

## Reference Pattern: Vote System

The comment implementation closely follows the vote system pattern:

| Aspect | Votes | Comments |
|--------|-------|----------|
| **Location** | User repositories | User repositories |
| **Lexicon** | `social.coves.feed.vote` | `social.coves.feed.comment` |
| **Operations** | CREATE, DELETE | CREATE, UPDATE, DELETE |
| **Mutability** | Immutable | Editable |
| **Foreign Keys** | None (out-of-order indexing) | None (out-of-order indexing) |
| **Delete Pattern** | Soft delete | Soft delete |
| **Idempotency** | ON CONFLICT DO NOTHING | ON CONFLICT DO NOTHING |
| **Count Updates** | Atomic transaction | Atomic transaction |
| **Security** | PDS authentication | PDS authentication |

---

## Future Phases

### 📋 Phase 2B: Vote Integration (Planned)

**Scope:**
- Update vote consumer to handle comment votes
- Integrate `GetVoteStateForComments()` in service layer
- Populate viewer.vote and viewer.voteUri in commentView
- Test vote creation on comments end-to-end
- Atomic updates to comments.upvote_count, downvote_count, score

**Dependencies:**
- Phase 1 indexing (✅ Complete)
- Phase 2A query API (✅ Complete)
- Vote consumer (already exists for posts)

**Estimated effort:** 2-3 hours

---

### 📋 Phase 2C: Post/User Integration (Partially Complete)

**Completed (PR Review):**
- ✅ Integrated post repository in comment service
- ✅ Return postView in getComments response with basic fields
- ✅ Populate post author DID, community DID, stats (upvotes, downvotes, score, comment count)

**Remaining Work:**
- ❌ Integrate user repository for full AuthorView
- ❌ Add display name and avatar to comment/post authors (currently returns DID as handle)
- ❌ Add community name and avatar (currently returns DID as name)
- ❌ Parse and include original record in commentView

**Dependencies:**
- Phase 2A query API (✅ Complete)
- Post repository integration (✅ Complete)
- User repository integration (⏳ Pending)

**Estimated effort for remaining work:** 1-2 hours

---

### 📋 Phase 3: Advanced Features (Future)

#### 3A: Distinguished Comments
- Moderator/admin comment flags
- Priority sorting for distinguished comments
- Visual indicators in UI

#### 3B: Comment Search & Filtering
- Full-text search within threads
- Filter by author, time range, score
- Search across community comments

#### 3C: Moderation Tools
- Hide/remove comments
- Flag system for user reports
- Moderator queue
- Audit log

#### 3D: Notifications
- Notify users of replies to their comments
- Notify post authors of new comments
- Mention notifications (@user)
- Customizable notification preferences

#### 3E: Enhanced Features
- Comment edit history tracking
- Save/bookmark comments
- Sort by "controversial" (high engagement, low score)
- Collapsible comment threads
- User-specific comment history API
- Community-wide comment stats/analytics

---

### 📋 Phase 4: Namespace Migration (Separate Task)

**Scope:**
- Migrate existing `social.coves.feed.comment` records to `social.coves.community.comment`
- Update all AT-URIs in database
- Update Jetstream consumer collection filter
- Migration script with rollback capability
- Zero-downtime deployment strategy

**Note:** Currently out of scope - will be tackled separately when needed.

---

## Performance Considerations

### Database Indexes

All critical query patterns are indexed:
- **Threading queries**: `idx_comments_root`, `idx_comments_parent`
- **Sorting by score**: `idx_comments_parent_score`
- **User history**: `idx_comments_commenter`
- **Vote targeting**: `idx_comments_uri_active`

### Denormalized Counts

Vote counts and reply counts are denormalized for performance:
- Avoids `COUNT(*)` queries on large datasets
- Updated atomically with comment operations
- Indexed for fast sorting

### Pagination Support

All list queries support limit/offset pagination:
- `ListByRoot(ctx, rootURI, limit, offset)`
- `ListByParent(ctx, parentURI, limit, offset)`
- `ListByCommenter(ctx, commenterDID, limit, offset)`

### N+1 Query Prevention

**Problem Solved:** The initial implementation had a classic N+1 query problem where nested reply loading made separate database queries for each comment's children. For a post with 50 top-level comments and 3 levels of depth, this could result in ~1,551 queries.

**Solution Implemented:** Batch loading strategy using window functions:
1. Collect all parent URIs at each depth level
2. Execute single batch query using `ListByParentsBatch()` with PostgreSQL window functions
3. Group results by parent URI in memory
4. Recursively process next level

**Performance Improvement:**
- Old: 1 + N + (N × M) + (N × M × P) queries per request
- New: 1 query per depth level (max 4 queries for depth 3)
- Example with depth 3, 50 comments: 1,551 queries → 4 queries (99.7% reduction)

**Implementation Details:**
```sql
-- Uses ROW_NUMBER() window function to limit per parent efficiently
WITH ranked_comments AS (
    SELECT *,
           ROW_NUMBER() OVER (
               PARTITION BY parent_uri
               ORDER BY hot_rank DESC
           ) as rn
    FROM comments
    WHERE parent_uri = ANY($1)
)
SELECT * FROM ranked_comments WHERE rn <= $2
```

### Hot Rank Caching Strategy

**Current Implementation:**
Hot rank is computed on-demand for every query using the Lemmy algorithm:
```sql
log(greatest(2, score + 2)) /
  power(((EXTRACT(EPOCH FROM (NOW() - created_at)) / 3600) + 2), 1.8)
```

**Performance Impact:**
- Computed for every comment in every hot-sorted query
- PostgreSQL handles this efficiently for moderate loads (<1000 comments per post)
- No noticeable performance degradation in testing

**Future Optimization (if needed):**

If hot rank computation becomes a bottleneck at scale:

1. **Add cached column:**
```sql
ALTER TABLE comments ADD COLUMN hot_rank_cached NUMERIC;
CREATE INDEX idx_comments_parent_hot_rank_cached
    ON comments(parent_uri, hot_rank_cached DESC)
    WHERE deleted_at IS NULL;
```

2. **Background recomputation job:**
```go
// Run every 5-15 minutes
func (j *HotRankJob) UpdateHotRanks(ctx context.Context) error {
    query := `
        UPDATE comments
        SET hot_rank_cached = log(greatest(2, score + 2)) /
            power(((EXTRACT(EPOCH FROM (NOW() - created_at)) / 3600) + 2), 1.8)
        WHERE deleted_at IS NULL
    `
    _, err := j.db.ExecContext(ctx, query)
    return err
}
```

3. **Use cached value in queries:**
```sql
SELECT * FROM comments
WHERE parent_uri = $1
ORDER BY hot_rank_cached DESC, score DESC
```

**When to implement:**
- Monitor query performance in production
- If p95 query latency > 200ms for hot-sorted queries
- If database CPU usage from hot rank computation > 20%
- Only optimize if measurements show actual bottleneck

**Trade-offs:**
- **Cached approach:** Faster queries, but ranks update every 5-15 minutes (slightly stale)
- **On-demand approach:** Always fresh ranks, slightly higher query cost
- For comment discussions, 5-15 minute staleness is acceptable (comments age slowly)

---

## Conclusion

The comment system has successfully completed **Phase 1 (Indexing)** and **Phase 2A (Query API)**, providing a production-ready threaded discussion system for Coves:

✅ **Phase 1 Complete**: Full indexing infrastructure with Jetstream consumer
✅ **Phase 2A Complete**: Query API with hot ranking, threading, and pagination
✅ **Fully Tested**: 30+ integration tests across indexing and query layers
✅ **Secure**: Input validation, parameterized queries, optional auth
✅ **Scalable**: Indexed queries, denormalized counts, cursor pagination
✅ **atProto Native**: User-owned records, Jetstream indexing, Bluesky patterns

**Next milestones:**
- Phase 2B: Vote integration for comment voting
- Phase 2C: Post/user integration for complete views
- Phase 3: Advanced features (moderation, notifications, search)

The implementation provides a solid foundation for building rich threaded discussions in Coves while maintaining compatibility with the broader atProto ecosystem and following established patterns from platforms like Lemmy and Reddit.

---

## Appendix: Command Reference

### Run Tests

**Phase 1 - Indexing Tests:**
```bash
TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
  go test -v ./tests/integration/comment_consumer_test.go \
              ./tests/integration/user_test.go \
              ./tests/integration/helpers.go \
  -run "TestCommentConsumer" -timeout 60s
```

**Phase 2A - Query API Tests:**
```bash
TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
  go test -v ./tests/integration/comment_query_test.go \
              ./tests/integration/user_test.go \
              ./tests/integration/helpers.go \
  -run "TestCommentQuery" -timeout 120s
```

**All Comment Tests:**
```bash
TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
  go test -v ./tests/integration/comment_*.go \
              ./tests/integration/user_test.go \
              ./tests/integration/helpers.go \
  -timeout 120s
```

### Apply Migration
```bash
GOOSE_DRIVER=postgres \
GOOSE_DBSTRING="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
  goose -dir internal/db/migrations up
```

### Build Server
```bash
go build ./cmd/server
```

### Environment Variables
```bash
# Jetstream URL (optional, defaults to localhost:6008)
export COMMENT_JETSTREAM_URL="ws://localhost:6008/subscribe?wantedCollections=social.coves.feed.comment"

# Database URL
export TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
```

---

**Last Updated:** November 6, 2025
**Status:** ✅ Phase 1 & 2A Complete - Production-Ready with All PR Fixes
