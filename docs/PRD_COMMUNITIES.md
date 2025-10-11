# Communities PRD: Federated Forum System

**Status:** In Development
**Owner:** Platform Team
**Last Updated:** 2025-10-10

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

## ✅ Completed Features (2025-10-10)

### Core Infrastructure
- [x] **V2 Architecture:** Communities own their own repositories
- [x] **PDS Account Provisioning:** Automatic account creation for each community
- [x] **Credential Management:** Secure storage of community PDS credentials
- [x] **Encryption at Rest:** PostgreSQL pgcrypto for sensitive credentials
- [x] **Write-Forward Pattern:** Service → PDS → Firehose → AppView
- [x] **Jetstream Consumer:** Real-time indexing from firehose
- [x] **V2 Validation:** Strict rkey="self" enforcement (no V1 compatibility)

### Security & Data Protection
- [x] **Encrypted Credentials:** Access/refresh tokens encrypted in database
- [x] **Credential Persistence:** PDS credentials survive server restarts
- [x] **JSON Exclusion:** Credentials never exposed in API responses (`json:"-"` tags)
- [x] **Password Hashing:** bcrypt for PDS account passwords
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
- [x] **V2 Validation Tests:** Rkey enforcement, self-ownership
- [x] **Consumer Tests:** Firehose event processing
- [x] **Repository Tests:** Database operations
- [x] **Unit Tests:** Service layer logic, timeout handling

---

## 🚧 In Progress / Needs Testing

### XRPC API Endpoints
**Status:** Handlers exist, need comprehensive E2E testing

- [ ] `social.coves.community.create` - **Handler exists**, needs E2E test with real PDS
- [ ] `social.coves.community.get` - **Handler exists**, needs E2E test
- [ ] `social.coves.community.update` - **Handler exists**, needs E2E test with community credentials
- [ ] `social.coves.community.list` - **Handler exists**, needs E2E test with pagination
- [ ] `social.coves.community.search` - **Handler exists**, needs E2E test with queries
- [ ] `social.coves.community.subscribe` - **Handler exists**, needs E2E test
- [ ] `social.coves.community.unsubscribe` - **Handler exists**, needs E2E test

**What's needed:**
- E2E tests that verify complete flow: HTTP → Service → PDS → Firehose → Consumer → DB → HTTP response
- Test with real PDS instance (not mocked)
- Verify Jetstream consumer picks up events in real-time

### Posts in Communities
**Status:** Lexicon designed, implementation TODO

- [ ] Extend `social.coves.post` lexicon with `community` field
- [ ] Create post endpoint (with community membership validation?)
- [ ] Feed generation for community posts
- [ ] Post consumer (index community posts from firehose)
- [ ] Community post count tracking

**What's needed:**
- Decide membership requirements for posting
- Design feed generation algorithm
- Implement post indexing in consumer
- Add tests for post creation/listing

---

## ⏳ TODO Before V1 Production Launch

### Critical Security & Authorization
- [ ] **OAuth Middleware:** Protect create/update/delete endpoints
- [ ] **Authorization Checks:** Verify user is community creator/moderator
- [ ] **Rate Limiting:** Prevent community creation spam (e.g., 5 per user per hour)
- [ ] **Handle Collision Detection:** Prevent duplicate community handles
- [ ] **DID Validation:** Verify DIDs before accepting create requests
- [ ] **Token Refresh Logic:** Handle expired PDS access tokens

### Community Discovery & Visibility
- [ ] **Visibility Enforcement:** Respect public/unlisted/private settings in listings
- [ ] **Federation Config:** Honor `allowExternalDiscovery` flag
- [ ] **Search Relevance:** Implement ranking algorithm (members, activity, etc.)
- [ ] **Directory Endpoint:** Public community directory with filters

### Membership & Participation
- [ ] **Membership Tracking:** Auto-create membership on first post
- [ ] **Reputation System:** Track user participation per community
- [ ] **Subscription → Membership Flow:** Define conversion logic
- [ ] **Member Lists:** Endpoint to list community members
- [ ] **Moderator Assignment:** Allow creators to add moderators

### Moderation (Basic)
- [ ] **Delist Community:** Remove from search/directory
- [ ] **Quarantine Community:** Show warning label
- [ ] **Remove Community:** Hide from instance AppView
- [ ] **Moderation Audit Log:** Track all moderation actions
- [ ] **Admin Endpoints:** Instance operator tools

### Token Refresh & Resilience
- [ ] **Refresh Token Logic:** Auto-refresh expired PDS access tokens
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
- `handle` - Scoped handle (`!gaming@coves.social`)
- `atprotoHandle` - Real atProto handle (`gaming.communities.coves.social`)
- `name` - Community name
- `createdBy` - DID of user who created community
- `hostedBy` - DID of hosting instance
- `visibility` - `"public"`, `"unlisted"`, or `"private"`
- `federation.allowExternalDiscovery` - Boolean

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
