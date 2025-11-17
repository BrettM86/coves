# Alpha Go-Live Readiness PRD

**Status**: Pre-Alpha
**Target**: Alpha launch with real users
**Last Updated**: 2025-11-16

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

#### 1. Full User Journey Test (CRITICAL)
**What**: Test complete user flow from signup to interaction
**Why**: No single test validates the entire happy path

- [ ] Create test: Signup → Authenticate → Create Community → Create Post → Add Comment → Vote
- [ ] Verify all data flows through Jetstream correctly
- [ ] Verify counts update (vote counts, comment counts, subscriber counts)
- [ ] Verify timeline feed shows posts from subscribed communities
- [ ] Test with 2+ users interacting (user A posts, user B comments)

**File**: Create `tests/integration/user_journey_e2e_test.go`
**Estimated Effort**: 4-6 hours

#### 2. Blob Upload E2E Test
**What**: Test image upload and display in posts
**Why**: No test validates the full blob upload → post → feed display flow

- [ ] Create post with embedded image
- [ ] Verify blob uploaded to PDS
- [ ] Verify blob URL transformation in feed responses
- [ ] Test multiple images in single post
- [ ] Test image in comment

**Estimated Effort**: 3-4 hours

#### 3. Multi-Community Timeline Test
**What**: Test timeline feed with multiple community subscriptions
**Why**: Timeline logic may have edge cases with multiple sources

- [ ] Create 3+ communities
- [ ] Subscribe user to all communities
- [ ] Create posts in each community
- [ ] Verify timeline shows posts from all subscribed communities
- [ ] Verify hot/top/new sorting across communities

**Estimated Effort**: 2-3 hours

#### 4. Concurrent User Scenarios
**What**: Test system behavior with simultaneous users
**Why**: Race conditions and locking issues only appear under concurrency

- [ ] Multiple users voting on same post simultaneously
- [ ] Multiple users commenting on same post simultaneously
- [ ] Community creation with same handle (should fail)
- [ ] Subscription race conditions

**Estimated Effort**: 4-5 hours

#### 5. Rate Limiting Tests
**What**: Verify rate limits work correctly
**Why**: Protection against abuse

- [ ] Test aggregator rate limits (already exists)
- [ ] Test general endpoint rate limits (100 req/min)
- [ ] Test comment rate limits (20 req/min)
- [ ] Verify 429 responses
- [ ] Verify rate limit headers

**Estimated Effort**: 2-3 hours

#### 6. Error Recovery Tests
**What**: Test system recovery from failures
**Why**: Production will have failures

- [ ] Jetstream reconnection after disconnect
- [ ] PDS temporarily unavailable during post creation
- [ ] Database connection loss and recovery
- [ ] Malformed Jetstream events (should skip, not crash)
- [ ] Out-of-order event handling (already partially covered)

**Estimated Effort**: 4-5 hours

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

### Week 3: E2E Testing + Polish
- **Days 11-12**: Critical E2E tests (user journey, blob upload)
- **Day 13**: Additional E2E tests
- **Days 14-15**: Load testing, bug fixes, polish

**Total**: 20-25 hours

**Grand Total: 65-80 hours (approximately 2-3 weeks full-time)**
*(Reduced from original 70-85 hours estimate due to completed handle resolution and comment count reconciliation)*

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
- [ ] All integration tests passing
- [ ] E2E user journey test passing
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
4. [ ] Review and prioritize with team
5. [ ] Test JWT verification with `pds.bretton.dev` (requires invite code or existing account)
6. [ ] Begin P0 blockers (DPoP fix first - highest user impact)
7. [ ] Set up monitoring infrastructure
8. [ ] Write critical E2E tests (especially full user journey)
9. [ ] Conduct load testing
10. [ ] Security review
11. [ ] Go/no-go decision
12. [ ] Launch! 🚀
