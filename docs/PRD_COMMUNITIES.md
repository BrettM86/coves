# Communities PRD: Federated Forum System

**Status:** In Development
**Owner:** Platform Team
**Last Updated:** 2025-10-17

## Overview

Coves communities are federated, instance-scoped forums built on atProto. Each community is identified by a scoped handle (`!gaming@coves.social`) and owns its own atProto repository, enabling true portability and decentralized governance.

## Architecture Evolution

### ✅ V2 Architecture (Current - 2025-10-10)

**Communities own their own repositories:**
- Each community has its own DID (`did:plc:xxx`)
- Each community owns its own atProto repository (`at://community_did/...`)
- Each community has its own PDS account (managed by Coves backend)
- Communities are truly portable - can migrate between instances by updating DID document

**Repository Structure:**
```
Repository:  at://did:plc:community789/social.coves.community.profile/self
Owner:       did:plc:community789 (community owns itself)
Hosted By:   did:web:coves.social (instance manages credentials)
```

**Key Benefits:**
- ✅ True atProto compliance (matches feed generators, labelers)
- ✅ Portable URIs (never change when migrating instances)
- ✅ Self-owned identity model
- ✅ Standard rkey="self" for singleton profiles

---

## ✅ Completed Features (Updated 2025-10-17)

### Core Infrastructure
- [x] **V2 Architecture:** Communities own their own repositories
- [x] **PDS Account Provisioning:** Automatic account creation for each community
- [x] **Credential Management:** Secure storage of community PDS credentials
- [x] **Token Refresh:** Automatic refresh of expired access tokens (completed 2025-10-17)
- [x] **Encryption at Rest:** PostgreSQL pgcrypto for sensitive credentials
- [x] **Write-Forward Pattern:** Service → PDS → Firehose → AppView
- [x] **Jetstream Consumer:** Real-time indexing from firehose
- [x] **V2 Validation:** Strict rkey="self" enforcement (no V1 compatibility)

### Security & Data Protection
- [x] **Encrypted Credentials:** Access/refresh tokens encrypted in database
- [x] **Credential Persistence:** PDS credentials survive server restarts
- [x] **Automatic Token Refresh:** Tokens refresh 5 minutes before expiration (completed 2025-10-17)
- [x] **Password Fallback:** Re-authentication when refresh tokens expire
- [x] **Concurrency Safety:** Per-community mutex prevents refresh race conditions
- [x] **JSON Exclusion:** Credentials never exposed in API responses (`json:"-"` tags)
- [x] **Password Encryption:** Encrypted (not hashed) for session creation fallback
- [x] **Timeout Handling:** 30s timeout for write operations, 10s for reads

### Database Schema
- [x] **Communities Table:** Full metadata with V2 credential columns
- [x] **Subscriptions Table:** Lightweight feed following
- [x] **Memberships Table:** Active participation tracking
- [x] **Moderation Table:** Local moderation actions
- [x] **Encryption Keys Table:** Secure key management for pgcrypto
- [x] **Indexes:** Optimized for search, visibility filtering, and lookups

### Service Layer
- [x] **CreateCommunity:** Provisions PDS account, creates record, persists credentials
- [x] **UpdateCommunity:** Uses community's own credentials (not instance credentials)
- [x] **GetCommunity:** Fetches from AppView DB with decrypted credentials
- [x] **ListCommunities:** Pagination, filtering, sorting
- [x] **SearchCommunities:** Full-text search on name/description
- [x] **Subscribe/Unsubscribe:** Create subscription records
- [x] **Handle Validation:** Scoped handle format (`!name@instance`)
- [x] **DID Generation:** Uses `did:plc` for portability

### Jetstream Consumer
- [x] **Profile Events:** Create, update, delete community profiles
- [x] **Subscription Events:** Index user subscriptions to communities
- [x] **V2 Enforcement:** Reject non-"self" rkeys (no V1 communities)
- [x] **Self-Ownership Validation:** Verify owner_did == did
- [x] **Error Handling:** Graceful handling of malformed events

### Testing Coverage
- [x] **Integration Tests:** Full CRUD operations
- [x] **Credential Tests:** Persistence, encryption, decryption
- [x] **Token Refresh Tests:** JWT parsing, credential updates, concurrency (completed 2025-10-17)
- [x] **V2 Validation Tests:** Rkey enforcement, self-ownership
- [x] **Consumer Tests:** Firehose event processing
- [x] **Repository Tests:** Database operations
- [x] **Unit Tests:** Service layer logic, timeout handling

---

## 🚧 In Progress / Needs Testing

### XRPC API Endpoints
**Status:** All core endpoints E2E tested! ✅

**✅ E2E Tested (via community_e2e_test.go):**
- [x] `social.coves.community.create` - Full E2E test with real PDS
- [x] `social.coves.community.get` - E2E test validates HTTP endpoint
- [x] `social.coves.community.list` - E2E test with pagination/filtering
- [x] `social.coves.community.update` - E2E test verifies write-forward + PDS update
- [x] `social.coves.community.subscribe` - E2E test verifies subscription in user's repo
- [x] `social.coves.community.unsubscribe` - E2E test verifies PDS deletion

**📍 Post-Alpha:**
- [ ] `social.coves.community.search` - Handler exists, defer E2E testing to post-alpha

**✅ OAuth Authentication Complete (2025-10-16):**
- User access tokens now flow through middleware → handlers → service
- Subscribe/unsubscribe operations use correct user-scoped credentials
- All E2E tests validate real PDS authentication with user tokens

---

## ⚠️ Alpha Blockers (Must Complete Before Alpha Launch)

### Critical Missing Features
- [x] **Community Blocking:** ✅ COMPLETE - Users can block communities from their feeds
  - ✅ Lexicon: `social.coves.community.block` record type implemented
  - ✅ Service: `BlockCommunity()` / `UnblockCommunity()` / `GetBlockedCommunities()` / `IsBlocked()`
  - ✅ Handlers: Block/unblock endpoints implemented
  - ✅ Repository: Full blocking methods with indexing
  - ✅ Jetstream Consumer: Real-time indexing of block events
  - ✅ Integration tests: Comprehensive coverage
  - **Completed:** 2025-10-16
  - **Impact:** Users can now hide unwanted communities from their feeds

### ✅ Critical Infrastructure - RESOLVED (2025-10-16)
- [x] **✅ Subscription Indexing & ContentVisibility - COMPLETE**
  - **Status:** Subscriptions now fully indexed in AppView with feed slider support
  - **Completed:** 2025-10-16
  - **What Was Fixed:**
    1. ✅ Fixed critical collection name bug (`social.coves.actor.subscription` → `social.coves.community.subscription`)
    2. ✅ Implemented ContentVisibility (1-5 slider) across all layers (handler, service, consumer, repository)
    3. ✅ Production Jetstream consumer now running ([cmd/server/main.go:220-243](cmd/server/main.go#L220-L243))
    4. ✅ Migration 008 adds `content_visibility` column with defaults and constraints
    5. ✅ Atomic subscriber count updates (SubscribeWithCount/UnsubscribeWithCount)
    6. ✅ DELETE operations properly handled (unsubscribe indexing)
    7. ✅ Idempotent operations (safe for Jetstream event replays)
    8. ✅ atProto naming compliance: singular namespace + `subject` field
  - **Impact:**
    - ✅ Users CAN subscribe/unsubscribe (writes to their PDS repo)
    - ✅ AppView INDEXES subscriptions from Jetstream in real-time
    - ✅ Can query user's subscriptions (data persisted with contentVisibility)
    - ✅ Feed generation ENABLED (know who's subscribed with visibility preferences)
    - ✅ Subscriber counts accurate (atomic updates)
  - **Testing:**
    - ✅ 13 comprehensive integration tests (subscription_indexing_test.go) - ALL PASSING
    - ✅ Enhanced E2E tests verify complete flow (HTTP → PDS → Jetstream → AppView)
    - ✅ ContentVisibility clamping tested (0→1, 10→5, defaults to 3)
    - ✅ Idempotency verified (duplicate events handled gracefully)
  - **Files:**
    - Implementation Doc: [docs/IMPLEMENTATION_SUBSCRIPTION_INDEXING.md](docs/IMPLEMENTATION_SUBSCRIPTION_INDEXING.md)
    - Lexicon: [internal/atproto/lexicon/social/coves/community/subscription.json](internal/atproto/lexicon/social/coves/community/subscription.json)
    - Consumer: [internal/atproto/jetstream/community_consumer.go](internal/atproto/jetstream/community_consumer.go)
    - Connector: [internal/atproto/jetstream/community_jetstream_connector.go](internal/atproto/jetstream/community_jetstream_connector.go)
    - Migration: [internal/db/migrations/008_add_content_visibility_to_subscriptions.sql](internal/db/migrations/008_add_content_visibility_to_subscriptions.sql)
    - Tests: [tests/integration/subscription_indexing_test.go](tests/integration/subscription_indexing_test.go)

### Critical Security (High Priority)
- [x] **OAuth Authentication:** ✅ COMPLETE - User access tokens flow end-to-end
  - ✅ Middleware stores user access token in context
  - ✅ Handlers extract and pass token to service
  - ✅ Service uses user token for user repo operations (subscribe/unsubscribe)
  - ✅ All E2E tests pass with real PDS authentication
  - **Completed:** 2025-10-16

- [x] **Token Refresh Logic:** ✅ COMPLETE - Auto-refresh expired PDS access tokens
  - ✅ Automatic token refresh before PDS operations (5-minute buffer)
  - ✅ Password fallback when refresh tokens expire (~2 months)
  - ✅ Concurrency-safe with per-community mutex locking
  - ✅ Atomic credential updates in database
  - ✅ Integration tests and structured logging
  - **Completed:** 2025-10-17
  - **See:** [IMPLEMENTATION_TOKEN_REFRESH.md](docs/IMPLEMENTATION_TOKEN_REFRESH.md)

---

## 📍 Beta Features (High Priority - Post Alpha)

### Posts in Communities
**Status:** Lexicon designed, implementation TODO
**Priority:** HIGHEST for Beta 1

- [ ] `social.coves.post` already has `community` field ✅
- [ ] Create post endpoint (decide: membership validation?)
- [ ] Feed generation for community posts
- [ ] Post consumer (index community posts from firehose)
- [ ] Community post count tracking
- [ ] Decide membership requirements for posting

**Without posts, communities exist but can't be used!**

---

## 📍 Beta Features (Lower Priority)

### Membership System
**Status:** Lexicon exists, design decisions needed
**Deferred:** Answer design questions before implementing

- [ ] Decide: Auto-join on first post vs explicit join?
- [ ] Decide: Reputation tracking in lexicon vs AppView only?
- [ ] Implement membership record creation (if explicit join)
- [ ] Member lists endpoint
- [ ] Reputation tracking (if in lexicon)

### Community Management
- [ ] **Community Deletion:** Soft delete / permanent delete
- [ ] **Wiki System:** Lexicon exists, no implementation
- [ ] **Advanced Rules:** Separate rules records, moderation config
- [ ] **Moderator Management:** Assign/remove moderators (governance work)
- [ ] **Categories:** REMOVE from lexicon and code (not needed)

### User Features
- [ ] **Saved Items:** Save posts/comments for later
- [ ] **User Flairs:** Per-community user flair (design TBD)

### Instance Moderation
- [ ] **Delist Community:** Remove from search/directory
- [ ] **Quarantine Community:** Show warning label
- [ ] **Remove Community:** Hide from instance AppView
- [ ] **Moderation Audit Log:** Track all moderation actions

---

## ⏳ TODO Before V1 Production Launch

### Community Discovery & Visibility
- [ ] **Visibility Enforcement:** Respect public/unlisted/private settings in listings
- [ ] **Federation Config:** Honor `allowExternalDiscovery` flag
- [ ] **Search Relevance:** Implement ranking algorithm (members, activity, etc.)
- [ ] **Directory Endpoint:** Public community directory with filters
- [ ] **Rate Limiting:** Prevent community creation spam (e.g., 5 per user per hour)
- [ ] **Handle Collision Detection:** Prevent duplicate community handles
- [ ] **DID Validation:** Verify DIDs before accepting create requests

### Token Refresh & Resilience
- [ ] **Retry Mechanism:** Retry failed PDS calls with backoff
- [ ] **Credential Rotation:** Periodic password rotation for security
- [ ] **Error Recovery:** Graceful degradation if PDS is unavailable

### Performance & Scaling
- [ ] **Database Indexes:** Verify all common queries are indexed
- [ ] **Query Optimization:** Review N+1 query patterns
- [ ] **Caching Strategy:** Cache frequently accessed communities
- [ ] **Pagination Limits:** Enforce max results per request
- [ ] **Connection Pooling:** Optimize PDS HTTP client reuse

### Documentation & Deployment
- [ ] **API Documentation:** OpenAPI/Swagger specs for all endpoints
- [ ] **Deployment Guide:** Production setup instructions
- [ ] **Migration Guide:** How to upgrade from test to production
- [ ] **Monitoring Guide:** Metrics and alerting setup
- [ ] **Security Checklist:** Pre-launch security audit

### Infrastructure & DNS
- [ ] **DNS Wildcard Setup:** Configure `*.communities.coves.social` for community handle resolution
- [ ] **Well-Known Endpoint:** Implement `.well-known/atproto-did` handler for `*.communities.coves.social` subdomains

---

## Out of Scope (Future Versions)

### V3: Federation & Discovery
- [ ] Cross-instance community search
- [ ] Federated moderation signals
- [ ] Trust networks between instances
- [ ] Moderation signal subscription

### V4: Community Governance
- [ ] Community-owned governance (voting on moderators)
- [ ] Migration voting (community votes to move instances)
- [ ] Custom domain DIDs (`did:web:gaming.community`)
- [ ] Governance thresholds and time locks

---

## Recent Critical Fixes (2025-10-10)

### Security & Credential Management
**Issue:** PDS credentials were created but never persisted
**Fix:** Service layer now immediately persists credentials via `repo.Create()`
**Impact:** Communities can now be updated after creation (credentials survive restarts)

**Issue:** Credentials stored in plaintext in PostgreSQL
**Fix:** Added pgcrypto encryption for access/refresh tokens
**Impact:** Database compromise no longer exposes active tokens

**Issue:** UpdateCommunity used instance credentials instead of community credentials
**Fix:** Changed to use `existing.DID` and `existing.PDSAccessToken`
**Impact:** Updates now correctly authenticate as the community itself

### V2 Architecture Enforcement
**Issue:** Consumer accepted V1 communities with TID-based rkeys
**Fix:** Strict validation - only rkey="self" accepted
**Impact:** No legacy V1 data in production

**Issue:** PDS write operations timed out (10s too short)
**Fix:** Dynamic timeout - writes get 30s, reads get 10s
**Impact:** Community creation no longer fails on slow PDS operations

---

## Lexicon Summary

### `social.coves.community.profile`
**Status:** ✅ Implemented and tested

**Required Fields:**
- `handle` - atProto handle (DNS-resolvable, e.g., `gaming.communities.coves.social`)
- `name` - Short community name for !mentions (e.g., `gaming`)
- `createdBy` - DID of user who created community
- `hostedBy` - DID of hosting instance
- `visibility` - `"public"`, `"unlisted"`, or `"private"`
- `federation.allowExternalDiscovery` - Boolean

**Note:** The `!gaming@coves.social` format is derived client-side from `name` + instance for UI display. The `handle` field contains only the DNS-resolvable atProto handle.

**Optional Fields:**
- `displayName` - Display name for UI
- `description` - Community description
- `descriptionFacets` - Rich text annotations
- `avatar` - Blob reference for avatar image
- `banner` - Blob reference for banner image
- `moderationType` - `"moderator"` or `"sortition"`
- `contentWarnings` - Array of content warning types
- `memberCount` - Cached count
- `subscriberCount` - Cached count

### `social.coves.community.subscription`
**Status:** ✅ Schema exists, consumer TODO

**Fields:**
- `community` - DID of community being subscribed to
- `subscribedAt` - Timestamp

### `social.coves.post` (Community Extension)
**Status:** ⏳ TODO

**New Field:**
- `community` - Optional DID of community this post belongs to

---

## Success Metrics

### Pre-Launch Checklist
- [ ] All XRPC endpoints have E2E tests
- [ ] OAuth authentication working on all protected endpoints
- [ ] Rate limiting prevents abuse
- [ ] Communities can be created, updated, searched, and subscribed to
- [ ] Jetstream consumer indexes events in < 1 second
- [ ] Database handles 10,000+ communities without performance issues
- [ ] Security audit completed

### V1 Launch Goals
- Communities can be created with scoped handles
- Posts can be made to communities (when implemented)
- Community discovery works on local instance
- All three visibility levels function correctly
- Basic moderation (delist/remove) works

---

## Technical Decisions Log

### 2025-10-11: Single Handle Field (atProto-Compliant)
**Decision:** Use single `handle` field containing DNS-resolvable atProto handle; remove `atprotoHandle` field

**Rationale:**
- Matches Bluesky pattern: `app.bsky.actor.profile` has one `handle` field
- Reduces confusion about which handle is "real"
- Simplifies lexicon (one field vs two)
- `!gaming@coves.social` display format is client-side UX concern, not protocol concern
- Follows separation of concerns: protocol layer uses DNS handles, UI layer formats for display

**Implementation:**
- Lexicon: `handle` = `gaming.communities.coves.social` (DNS-resolvable)
- Client derives display: `!${name}@${instance}` from `name` + parsed instance
- Rich text facets can encode community mentions with `!` prefix for UX

**Trade-offs Accepted:**
- Clients must parse/format for display (but already do this for `@user` mentions)
- No explicit "display handle" in record (but `displayName` serves this purpose)

---

### 2025-10-10: V2 Architecture Completed
- Migrated from instance-owned to community-owned repositories
- Each community now has own PDS account
- Credentials encrypted at rest using pgcrypto
- Strict V2 enforcement (no V1 compatibility)

### 2025-10-08: DID Architecture & atProto Compliance
- Migrated from `did:coves` to `did:plc` (portable DIDs)
- Added required `did` field to lexicon
- Fixed critical `record_uri` bug
- Matches Bluesky feed generator pattern

---

## References

- atProto Lexicon Spec: https://atproto.com/specs/lexicon
- DID Web Spec: https://w3c-ccg.github.io/did-method-web/
- Bluesky Handle System: https://atproto.com/specs/handle
- PLC Directory: https://plc.directory
