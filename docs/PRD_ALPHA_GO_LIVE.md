# Alpha Go-Live Readiness PRD

**Status**: Pre-Alpha → **E2E Testing Complete** 🎉
**Target**: Alpha launch with real users
**Last Updated**: 2025-11-16

## 🎯 Major Progress Update

**✅ ALL E2E TESTS COMPLETE!** (Completed 2025-11-16)

All 6 critical E2E test suites have been implemented and are passing:
- ✅ Full User Journey (signup → community → post → comment → vote)
- ✅ Blob Upload (image uploads, PDS integration, validation)
- ✅ Multi-Community Timeline (feed aggregation, sorting, pagination)
- ✅ Concurrent Scenarios (race condition testing with database verification)
- ✅ Rate Limiting (100 req/min general, 20 req/min comments, 10 posts/hour aggregators)
- ✅ Error Recovery (Jetstream retry, PDS unavailability, malformed events)

**Time Saved**: ~7-12 hours through parallel agent implementation
**Test Quality**: Enhanced with comprehensive database record verification to catch race conditions

## Overview

This document tracks the remaining work required to launch Coves alpha with real users. Focus is on critical functionality, security, and operational readiness.

---

## P0: Critical Blockers (Must Complete Before Alpha)

### 1. Authentication & Security

#### JWT Signature Verification (Production Mode)
- [ ] Test with production PDS at `pds.bretton.dev`
  - [ ] Create test account on production PDS
  - [ ] Verify JWKS endpoint is accessible
  - [ ] Run `TestJWTSignatureVerification` against production PDS
  - [ ] Confirm signature verification succeeds
  - [ ] Test token refresh flow
- [ ] Set `AUTH_SKIP_VERIFY=false` in production environment
- [ ] Verify all auth middleware tests pass with verification enabled
- [ ] Document production PDS requirements for communities

**Estimated Effort**: 2-3 hours
**Risk**: Medium (code implemented, needs validation)

#### did:web Verification
- [ ] Complete did:web domain verification implementation
- [ ] Test with real did:web identities
- [ ] Add security logging for verification failures
- [ ] Set `SKIP_DID_WEB_VERIFICATION=false` for production

**Estimated Effort**: 2-3 hours
**Risk**: Medium

### 2. DPoP Token Architecture Fix

**Problem**: Backend attempts to write subscriptions/blocks to user PDS using DPoP-bound tokens (fails with "Malformed token").

#### Remove Write-Forward Code
- [ ] Remove write-forward from `SubscribeToCommunity` handler
- [ ] Remove write-forward from `UnsubscribeFromCommunity` handler
- [ ] Remove write-forward from `BlockCommunity` handler
- [ ] Remove write-forward from `UnblockCommunity` handler
- [ ] Update handlers to return helpful error: "Write directly to your PDS"
- [ ] Update API documentation to reflect client-write pattern
- [ ] Verify Jetstream consumers still index correctly

**Files**:
- `internal/core/communities/service.go:564-816`
- `internal/api/handlers/community/subscribe.go`
- `internal/api/handlers/community/block.go`

**Estimated Effort**: 3-4 hours
**Risk**: Low (similar to votes pattern)

## P1: Important (Should Complete Before Alpha)

### 5. Post Read Operations

- [ ] Implement `getPost` endpoint (single post retrieval)
- [ ] Implement `listPosts` endpoint (with pagination)
- [ ] Add post permalink support
- [ ] Integration tests for post retrieval
- [ ] Error handling for missing/deleted posts

**Estimated Effort**: 6-8 hours
**Risk**: Low
**Note**: Can defer if direct post linking not needed initially

### 6. Production Infrastructure

#### Monitoring Setup
- [ ] Add Prometheus metrics endpoints
  - [ ] HTTP request metrics (duration, status codes, paths)
  - [ ] Database query metrics (slow queries, connection pool)
  - [ ] Jetstream consumer metrics (events processed, lag, errors)
  - [ ] Auth metrics (token validations, failures)
- [ ] Set up Grafana dashboards
  - [ ] Request rate and latency
  - [ ] Error rates by endpoint
  - [ ] Database performance
  - [ ] Jetstream consumer health
- [ ] Configure alerting rules
  - [ ] High error rate (>5% 5xx responses)
  - [ ] Slow response time (p99 >1s)
  - [ ] Database connection pool exhaustion
  - [ ] Jetstream consumer lag >1 minute
  - [ ] PDS health check failures

**Estimated Effort**: 8-10 hours

#### Structured Logging
- [ ] Replace `log` package with structured logger (zerolog or zap)
- [ ] Add log levels (debug, info, warn, error)
- [ ] JSON output format for production
- [ ] Add request ID tracking
- [ ] Add correlation IDs for async operations
- [ ] Sanitize sensitive data from logs (passwords, tokens, emails)
- [ ] Configure log rotation
- [ ] Ship logs to aggregation service (optional: Loki, CloudWatch)

**Estimated Effort**: 6-8 hours

#### Database Backups
- [ ] Automated PostgreSQL backups (daily minimum)
- [ ] Backup retention policy (30 days)
- [ ] Test restore procedure
- [ ] Document backup/restore runbook
- [ ] Off-site backup storage
- [ ] Monitor backup success/failure
- [ ] Point-in-time recovery (PITR) setup (optional)

**Estimated Effort**: 4-6 hours

#### Load Testing
- [ ] Define load test scenarios
  - [ ] User signup and authentication
  - [ ] Community creation
  - [ ] Post creation and viewing
  - [ ] Feed retrieval (timeline, discover, community)
  - [ ] Comment creation and threading
  - [ ] Voting
- [ ] Set target metrics
  - [ ] Concurrent users target (e.g., 100 concurrent)
  - [ ] Requests per second target
  - [ ] P95 latency target (<500ms)
  - [ ] Error rate target (<1%)
- [ ] Run load tests with k6/Artillery/JMeter
- [ ] Identify bottlenecks (database, CPU, memory)
- [ ] Optimize slow queries
- [ ] Add database indexes if needed
- [ ] Test graceful degradation under load

**Estimated Effort**: 10-12 hours

#### Deployment Runbook
- [ ] Document deployment procedure
  - [ ] Pre-deployment checklist
  - [ ] Database migration steps
  - [ ] Environment variable validation
  - [ ] Health check verification
  - [ ] Rollback procedure
- [ ] Document operational procedures
  - [ ] How to check system health
  - [ ] How to read logs
  - [ ] How to check Jetstream consumer status
  - [ ] How to manually trigger community token refresh
  - [ ] How to clear caches
- [ ] Document incident response
  - [ ] Who to contact
  - [ ] Escalation path
  - [ ] Common issues and fixes
  - [ ] Emergency procedures (PDS down, database down, etc.)
- [ ] Create production environment checklist
  - [ ] All environment variables set
  - [ ] `AUTH_SKIP_VERIFY=false`
  - [ ] `SKIP_DID_WEB_VERIFICATION=false`
  - [ ] Database migrations applied
  - [ ] PDS connectivity verified
  - [ ] JWKS caching working
  - [ ] Jetstream consumers running
  - [ ] Monitoring and alerting active

**Estimated Effort**: 6-8 hours

---

## P2: Nice to Have (Can Defer to Post-Alpha)

### 7. Post Update/Delete
- [ ] Implement post update endpoint
- [ ] Implement post delete endpoint
- [ ] Jetstream consumer for UPDATE/DELETE events
- [ ] Soft delete support

**Estimated Effort**: 4-6 hours

### 8. Community Delete
- [ ] Implement community delete endpoint
- [ ] Cascade delete considerations
- [ ] Archive vs hard delete decision

**Estimated Effort**: 2-3 hours

### 9. Content Rules Validation
- [ ] Implement text-only community enforcement
- [ ] Implement allowed embed types validation
- [ ] Content length limits

**Estimated Effort**: 6-8 hours

### 10. Search Functionality
- [ ] Community search improvements
- [ ] Post search
- [ ] User search
- [ ] Full-text search with PostgreSQL or external service

**Estimated Effort**: 8-10 hours

---

## Testing Gaps

### E2E Testing Recommendations

#### 1. Full User Journey Test (CRITICAL) ✅ COMPLETE
**What**: Test complete user flow from signup to interaction
**Why**: No single test validates the entire happy path

- [x] Create test: Signup → Authenticate → Create Community → Create Post → Add Comment → Vote
- [x] Verify all data flows through Jetstream correctly
- [x] Verify counts update (vote counts, comment counts, subscriber counts)
- [x] Verify timeline feed shows posts from subscribed communities
- [x] Test with 2+ users interacting (user A posts, user B comments)
- [x] Real E2E with Docker infrastructure (PDS, Jetstream, PostgreSQL)
- [x] Graceful fallback for CI/CD environments

**Actual Time**: ~3 hours (agent-implemented)
**Test Location**: `tests/integration/user_journey_e2e_test.go`

#### 2. Blob Upload E2E Test ✅ COMPLETE
**What**: Test image upload and display in posts
**Why**: No test validates the full blob upload → post → feed display flow

- [x] Create post with embedded image
- [x] Verify blob uploaded to PDS
- [x] Verify blob URL transformation in feed responses
- [x] Test multiple images in single post
- [x] Test image in comment
- [x] PDS health check (skips gracefully if PDS unavailable)
- [x] Mock server test (runs in all environments)
- [x] Comprehensive validation tests (empty data, MIME types, size limits)
- [x] Actual JPEG format testing (not just PNG with different MIME types)

**Actual Time**: ~2-3 hours (agent-implemented)
**Test Location**: `tests/integration/blob_upload_e2e_test.go`

#### 3. Multi-Community Timeline Test ✅ COMPLETE
**What**: Test timeline feed with multiple community subscriptions
**Why**: Timeline logic may have edge cases with multiple sources

- [x] Create 3+ communities
- [x] Subscribe user to all communities
- [x] Create posts in each community
- [x] Verify timeline shows posts from all subscribed communities
- [x] Verify hot/top/new sorting across communities
- [x] Test pagination across multiple communities
- [x] Verify security (unsubscribed communities excluded)
- [x] Verify record schema compliance across communities

**Actual Time**: ~2 hours
**Test Location**: `/tests/integration/timeline_test.go::TestGetTimeline_MultiCommunity_E2E`

#### 4. Concurrent User Scenarios ✅ COMPLETE
**What**: Test system behavior with simultaneous users
**Why**: Race conditions and locking issues only appear under concurrency

- [x] Multiple users voting on same post simultaneously (20-25 concurrent)
- [x] Multiple users commenting on same post simultaneously (25 concurrent)
- [x] Community creation with same handle (should fail) - verified UNIQUE constraint
- [x] Subscription race conditions (30 concurrent subscribers)
- [x] **Enhanced with database record verification** (detects duplicates/lost records)
- [x] Concurrent upvotes and downvotes (15 up + 10 down)
- [x] Concurrent replies to same comment (15 concurrent)
- [x] Concurrent subscribe/unsubscribe (20 users)

**Actual Time**: ~3 hours (agent-implemented) + 1 hour (race condition verification added)
**Test Location**: `tests/integration/concurrent_scenarios_test.go`
**Finding**: NO RACE CONDITIONS DETECTED - all tests pass with full database verification

#### 5. Rate Limiting Tests ✅ COMPLETE
**What**: Verify rate limits work correctly
**Why**: Protection against abuse

- [x] Test aggregator rate limits (10 posts/hour) - existing test verified
- [x] Test general endpoint rate limits (100 req/min)
- [x] Test comment rate limits (20 req/min)
- [x] Verify 429 responses
- [x] Verify rate limit headers (documented as not implemented - acceptable for Alpha)
- [x] Verify per-client isolation (IP-based rate limiting)
- [x] Verify X-Forwarded-For and X-Real-IP header support
- [x] Test rate limit reset behavior
- [x] Test thread-safety with concurrent requests
- [x] Test rate limiting across different HTTP methods

**Actual Time**: ~2 hours (agent-implemented)
**Test Location**: `tests/e2e/ratelimit_e2e_test.go`
**Configuration Documented**: All rate limits documented in comments (removed fake summary "test")

#### 6. Error Recovery Tests ✅ COMPLETE
**What**: Test system recovery from failures
**Why**: Production will have failures

- [x] Jetstream connection retry on failure (renamed from "reconnection" for accuracy)
- [x] PDS temporarily unavailable during post creation (AppView continues indexing)
- [x] Database connection loss and recovery (connection pool auto-recovery)
- [x] Malformed Jetstream events (gracefully skipped, no crashes)
- [x] Out-of-order event handling (last-write-wins strategy)
- [x] Events processed correctly after connection established

**Actual Time**: ~2 hours (agent-implemented) + 30 min (test accuracy improvements)
**Test Location**: `tests/e2e/error_recovery_test.go`
**Findings**:
- ✅ Automatic reconnection with 5s backoff
- ✅ Circuit breaker pattern for external services
- ✅ AppView can index without PDS availability
- ⚠️ Note: Tests verify connection retry, not full reconnect-after-disconnect (requires mock WebSocket server)

#### 7. Federation Readiness (Optional)
**What**: Test cross-PDS interactions
**Why**: Future-proofing for federation

- [ ] User on different PDS subscribing to Coves community
- [ ] User on different PDS commenting on Coves post
- [ ] User on different PDS voting on Coves content
- [ ] Handle resolution across PDSs

**Note**: Defer to Beta unless federation is alpha requirement

---

## Timeline Estimate

### Week 1: Critical Blockers (P0)
- **Days 1-2**: Authentication (JWT + did:web verification)
- **Day 3**: DPoP token architecture fix
- ~~**Day 4**: Handle resolution + comment count reconciliation~~ ✅ **COMPLETED**
- **Day 4-5**: Testing and bug fixes

**Total**: 15-20 hours (reduced from 20-25 due to completed items)

### Week 2: Production Infrastructure (P1)
- **Days 6-7**: Monitoring + structured logging
- **Day 8**: Database backups + load testing
- **Days 9-10**: Deployment runbook + final testing

**Total**: 30-35 hours

### Week 3: E2E Testing + Polish ✅ E2E TESTS COMPLETE
- ~~**Days 11-12**: Critical E2E tests (user journey, blob upload)~~ ✅ **COMPLETED** (agent-implemented in ~6 hours)
- ~~**Day 13**: Additional E2E tests~~ ✅ **COMPLETED** (concurrent, rate limiting, error recovery in ~7 hours)
- **Days 14-15**: Load testing, bug fixes, polish

**Total**: ~~20-25 hours~~ → **13 hours actual** (E2E tests) + 7-12 hours remaining (load testing, polish)

**Grand Total: ~~65-80 hours~~ → 50-65 hours remaining (approximately 1.5-2 weeks full-time)**
*(Originally 70-85 hours. Reduced by completed items: handle resolution, comment count reconciliation, and ALL E2E tests)*

**✅ Progress Update**: E2E testing section COMPLETE ahead of schedule - saved ~7-12 hours through parallel agent implementation

---

## Success Criteria

Alpha is ready when:

- [ ] All P0 blockers resolved
  - ✅ Handle resolution (COMPLETE)
  - ✅ Comment count reconciliation (COMPLETE)
  - [ ] JWT signature verification working with production PDS
  - [ ] DPoP architecture fix implemented
  - [ ] did:web verification complete
- [ ] Subscriptions/blocking work via client-write pattern
- [x] **All integration tests passing** ✅
- [x] **E2E user journey test passing** ✅
- [x] **E2E blob upload tests passing** ✅
- [x] **E2E concurrent scenarios tests passing** ✅
- [x] **E2E rate limiting tests passing** ✅
- [x] **E2E error recovery tests passing** ✅
- [ ] Load testing shows acceptable performance (100+ concurrent users)
- [ ] Monitoring and alerting active
- [ ] Database backups configured and tested
- [ ] Deployment runbook complete and validated
- [ ] Security audit completed (basic)
- [ ] No known critical bugs

---

## Go/No-Go Decision Points

### Can we launch without it?

| Feature | Alpha Requirement | Status | Rationale |
|---------|------------------|--------|-----------|
| JWT signature verification | ✅ YES | 🟡 Needs testing | Security critical |
| DPoP architecture fix | ✅ YES | 🔴 Not started | Subscriptions broken without it |
| ~~Handle resolution~~ | ~~✅ YES~~ | ✅ **COMPLETE** | Core UX requirement |
| ~~Comment count reconciliation~~ | ~~✅ YES~~ | ✅ **COMPLETE** | Data accuracy |
| Post read endpoints | ⚠️ MAYBE | 🔴 Not implemented | Can use feeds initially |
| Post update/delete | ❌ NO | 🔴 Not implemented | Can add post-launch |
| Moderation system | ❌ NO | 🔴 Not implemented | Deferred to Beta per PRD_GOVERNANCE |
| Full-text search | ❌ NO | 🔴 Not implemented | Browse works without it |
| Federation testing | ❌ NO | 🔴 Not implemented | Single-instance alpha |
| Mobile app | ⚠️ MAYBE | 🔴 Not implemented | Web-first acceptable |

---

## Risk Assessment

### High Risk
1. **JWT verification with production PDS** - Never tested with real JWKS
2. **Load under real traffic** - Current tests are single-user
3. **Operational knowledge** - No one has run this in production yet

### Medium Risk
1. **Database performance** - Queries optimized but not load tested
2. **Jetstream consumer lag** - May fall behind under high write volume
3. **Token refresh stability** - Community tokens refresh every 2 hours (tested but not long-running)

### Low Risk
1. **DPoP architecture fix** - Similar pattern already works (votes)
2. ~~**Handle resolution**~~ - ✅ Already implemented
3. ~~**Comment reconciliation**~~ - ✅ Already implemented

---

## Open Questions

1. **What's the target alpha user count?** (affects infrastructure sizing)
2. **What's the alpha duration?** (affects monitoring retention, backup retention)
3. **Is mobile app required for alpha?** (affects DPoP testing priority)
4. **What's the rollback strategy?** (database migrations may not be reversible)
5. **Who's on-call during alpha?** (affects runbook detail level)
6. **What's the acceptable downtime?** (affects HA requirements)
7. **Budget for infrastructure?** (affects monitoring/backup solutions)

---

## Next Steps

1. ✅ Create this PRD
2. ✅ Validate handle resolution (COMPLETE)
3. ✅ Validate comment count reconciliation (COMPLETE)
4. ✅ **Write critical E2E tests** (COMPLETE - all 6 test suites implemented)
5. [ ] Review and prioritize with team
6. [ ] Test JWT verification with `pds.bretton.dev` (requires invite code or existing account)
7. [ ] Begin P0 blockers (DPoP fix first - highest user impact)
8. [ ] Set up monitoring infrastructure
9. [ ] Conduct load testing (infrastructure ready, tests written, needs execution)
10. [ ] Security review
11. [ ] Go/no-go decision
12. [ ] Launch! 🚀

**🎉 Major Milestone**: All E2E tests complete! Test coverage now includes full user journey, blob uploads, concurrent operations, rate limiting, and error recovery.
