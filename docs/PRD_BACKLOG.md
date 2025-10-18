# Backlog PRD: Platform Improvements & Technical Debt

**Status:** Ongoing
**Owner:** Platform Team
**Last Updated:** 2025-10-17

## Overview

Miscellaneous platform improvements, bug fixes, and technical debt that don't fit into feature-specific PRDs.

---

## 🟡 P1: Important (Alpha Blockers)

### did:web Domain Verification & hostedByDID Auto-Population
**Added:** 2025-10-11 | **Updated:** 2025-10-16 | **Effort:** 2-3 days | **Priority:** ALPHA BLOCKER

**Problem:**
1. **Domain Impersonation**: Self-hosters can set `INSTANCE_DID=did:web:nintendo.com` without owning the domain, enabling attacks where communities appear hosted by trusted domains
2. **hostedByDID Spoofing**: Malicious instance operators can modify source code to claim communities are hosted by domains they don't own, enabling reputation hijacking and phishing

**Attack Scenarios:**
- Malicious instance sets `instanceDID="did:web:coves.social"` → communities show as hosted by official Coves
- Federation partners can't verify instance authenticity
- AppView pollution with fake hosting claims

**Solution:**
1. **Basic Validation (Phase 1)**: Verify `did:web:` domain matches configured `instanceDomain`
2. **Cryptographic Verification (Phase 2)**: Fetch `https://domain/.well-known/did.json` and verify:
   - DID document exists and is valid
   - Domain ownership proven via HTTPS hosting
   - DID document matches claimed `instanceDID`
3. **Auto-populate hostedByDID**: Remove from client API, derive from instance configuration in service layer

**Current Status:**
- ✅ Default changed from `coves.local` → `coves.social` (fixes `.local` TLD bug)
- ✅ TODO comment in [cmd/server/main.go:126-131](../cmd/server/main.go#L126-L131)
- ✅ hostedByDID removed from client requests (2025-10-16)
- ✅ Service layer auto-populates `hostedByDID` from `instanceDID` (2025-10-16)
- ✅ Handler rejects client-provided `hostedByDID` (2025-10-16)
- ✅ Basic validation: Logs warning if `did:web:` domain ≠ `instanceDomain` (2025-10-16)
- ⚠️ **REMAINING**: Full DID document verification (cryptographic proof of ownership)

**Implementation Notes:**
- Phase 1 complete: Basic validation catches config errors, logs warnings
- Phase 2 needed: Fetch `https://domain/.well-known/did.json` and verify ownership
- Add `SKIP_DID_WEB_VERIFICATION=true` for dev mode
- Full verification blocks startup if domain ownership cannot be proven

---

### ✅ Token Refresh Logic for Community Credentials - COMPLETE
**Added:** 2025-10-11 | **Completed:** 2025-10-17 | **Effort:** 1.5 days | **Status:** ✅ DONE

**Problem:** Community PDS access tokens expire (~2hrs). Updates fail until manual intervention.

**Solution Implemented:**
- ✅ Automatic token refresh before PDS operations (5-minute buffer before expiration)
- ✅ JWT expiration parsing without signature verification (`parseJWTExpiration`, `needsRefresh`)
- ✅ Token refresh using Indigo SDK (`atproto.ServerRefreshSession`)
- ✅ Password fallback when refresh tokens expire (~2 months) via `atproto.ServerCreateSession`
- ✅ Atomic credential updates (`UpdateCredentials` repository method)
- ✅ Concurrency-safe with per-community mutex locking
- ✅ Structured logging for monitoring (`[TOKEN-REFRESH]` events)
- ✅ Integration tests for token expiration detection and credential updates

**Files Created:**
- [internal/core/communities/token_utils.go](../internal/core/communities/token_utils.go) - JWT parsing utilities
- [internal/core/communities/token_refresh.go](../internal/core/communities/token_refresh.go) - Refresh and re-auth logic
- [tests/integration/token_refresh_test.go](../tests/integration/token_refresh_test.go) - Integration tests

**Files Modified:**
- [internal/core/communities/service.go](../internal/core/communities/service.go) - Added `ensureFreshToken` + concurrency control
- [internal/core/communities/interfaces.go](../internal/core/communities/interfaces.go) - Added `UpdateCredentials` interface
- [internal/db/postgres/community_repo.go](../internal/db/postgres/community_repo.go) - Implemented `UpdateCredentials`

**Documentation:** See [IMPLEMENTATION_TOKEN_REFRESH.md](../docs/IMPLEMENTATION_TOKEN_REFRESH.md) for full details

**Impact:** ✅ Communities can now be updated 24+ hours after creation without manual intervention

---

### ✅ Subscription Visibility Level (Feed Slider 1-5 Scale) - COMPLETE
**Added:** 2025-10-15 | **Completed:** 2025-10-16 | **Effort:** 1 day | **Status:** ✅ DONE

**Problem:** Users couldn't control how much content they see from each community. Lexicon had `contentVisibility` (1-5 scale) but code didn't use it.

**Solution Implemented:**
- ✅ Updated subscribe handler to accept `contentVisibility` parameter (1-5, default 3)
- ✅ Store in subscription record on PDS (`social.coves.community.subscription`)
- ✅ Migration 008 adds `content_visibility` column to database with CHECK constraint
- ✅ Clamping at all layers (handler, service, consumer) for defense in depth
- ✅ Atomic subscriber count updates (SubscribeWithCount/UnsubscribeWithCount)
- ✅ Idempotent operations (safe for Jetstream event replays)
- ✅ Fixed critical collection name bug (was using wrong namespace)
- ✅ Production Jetstream consumer now running
- ✅ 13 comprehensive integration tests - all passing

**Files Modified:**
- Lexicon: [subscription.json](../internal/atproto/lexicon/social/coves/community/subscription.json) ✅ Updated to atProto conventions
- Handler: [community/subscribe.go](../internal/api/handlers/community/subscribe.go) ✅ Accepts contentVisibility
- Service: [communities/service.go](../internal/core/communities/service.go) ✅ Clamps and passes to PDS
- Consumer: [community_consumer.go](../internal/atproto/jetstream/community_consumer.go) ✅ Extracts and indexes
- Repository: [community_repo_subscriptions.go](../internal/db/postgres/community_repo_subscriptions.go) ✅ All queries updated
- Migration: [008_add_content_visibility_to_subscriptions.sql](../internal/db/migrations/008_add_content_visibility_to_subscriptions.sql) ✅ Schema changes
- Tests: [subscription_indexing_test.go](../tests/integration/subscription_indexing_test.go) ✅ Comprehensive coverage

**Documentation:** See [IMPLEMENTATION_SUBSCRIPTION_INDEXING.md](../docs/IMPLEMENTATION_SUBSCRIPTION_INDEXING.md) for full details

**Impact:** ✅ Users can now adjust feed volume per community (key feature from DOMAIN_KNOWLEDGE.md enabled)

---

### Community Blocking
**Added:** 2025-10-15 | **Effort:** 1 day | **Priority:** ALPHA BLOCKER

**Problem:** Users have no way to block unwanted communities from their feeds.

**Solution:**
1. **Lexicon:** Extend `social.coves.actor.block` to support community DIDs (currently user-only)
2. **Service:** Implement `BlockCommunity(userDID, communityDID)` and `UnblockCommunity()`
3. **Handlers:** Add XRPC endpoints `social.coves.community.block` and `unblock`
4. **Repository:** Add methods to track blocked communities
5. **Feed:** Filter blocked communities from feed queries (beta work)

**Code:**
- Lexicon: [actor/block.json](../internal/atproto/lexicon/social/coves/actor/block.json) - Currently only supports user DIDs
- Service: New methods needed
- Handlers: New files needed

**Impact:** Users can't avoid unwanted content without blocking

---

## 🔴 P1.5: Federation Blockers (Beta Launch)

### Cross-PDS Write-Forward Support
**Added:** 2025-10-17 | **Effort:** 3-4 hours | **Priority:** FEDERATION BLOCKER (Beta)

**Problem:** Current write-forward implementation assumes all users are on the same PDS as the Coves instance. This breaks federation when users from external PDSs try to interact with communities.

**Current Behavior:**
- User on `pds.bsky.social` subscribes to community on `coves.social`
- Coves calls `s.pdsURL` (instance default: `http://localhost:3001`)
- Write goes to WRONG PDS → fails with 401/403

**Impact:**
- ✅ **Alpha**: Works fine (single PDS deployment)
- ❌ **Beta**: Breaks federation (users on different PDSs can't subscribe/interact)

**Root Cause:**
- [service.go:736](../internal/core/communities/service.go#L736): `createRecordOnPDSAs` hardcodes `s.pdsURL`
- [service.go:753](../internal/core/communities/service.go#L753): `putRecordOnPDSAs` hardcodes `s.pdsURL`
- [service.go:767](../internal/core/communities/service.go#L767): `deleteRecordOnPDSAs` hardcodes `s.pdsURL`

**Solution:**
1. Add identity resolver dependency to `CommunityService`
2. Before write-forward, resolve user's DID → extract PDS URL
3. Call user's actual PDS instead of `s.pdsURL`

**Implementation:**
```go
// Before write-forward to user's repo:
userIdentity, err := s.identityResolver.ResolveDID(ctx, userDID)
if err != nil {
    return fmt.Errorf("failed to resolve user PDS: %w", err)
}

// Use user's actual PDS URL
endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", userIdentity.PDSURL)
```

**Files to Modify:**
- `internal/core/communities/service.go` - Add resolver, modify write-forward methods
- `cmd/server/main.go` - Pass identity resolver to community service constructor
- Tests - Add cross-PDS scenarios

**Testing:**
- User on external PDS subscribes to community
- User on external PDS blocks community
- Community updates still work (communities ARE on instance PDS)

---

## 🟢 P2: Nice-to-Have

### Remove Categories from Community Lexicon
**Added:** 2025-10-15 | **Effort:** 30 minutes | **Priority:** Cleanup

**Problem:** Categories field exists in create/update lexicon but not in profile record. Adds complexity without clear value.

**Solution:**
- Remove `categories` from [create.json](../internal/atproto/lexicon/social/coves/community/create.json#L46-L54)
- Remove `categories` from [update.json](../internal/atproto/lexicon/social/coves/community/update.json#L51-L59)
- Remove from [community.go:91](../internal/core/communities/community.go#L91)
- Remove from service layer ([service.go:109-110](../internal/core/communities/service.go#L109-L110))

**Impact:** Simplifies lexicon, removes unused feature

---

### Improve .local TLD Error Messages
**Added:** 2025-10-11 | **Effort:** 1 hour

**Problem:** Generic error "TLD .local is not allowed" confuses developers.

**Solution:** Enhance `InvalidHandleError` to explain root cause and suggest fixing `INSTANCE_DID`.

---

### Self-Hosting Security Guide
**Added:** 2025-10-11 | **Effort:** 1 day

**Needed:** Document did:web setup, DNS config, secrets management, rate limiting, PostgreSQL hardening, monitoring.

---

### OAuth Session Cleanup Race Condition
**Added:** 2025-10-11 | **Effort:** 2 hours

**Problem:** Cleanup goroutine doesn't handle graceful shutdown, may orphan DB connections.

**Solution:** Pass cancellable context, handle SIGTERM, add cleanup timeout.

---

### Jetstream Consumer Race Condition
**Added:** 2025-10-11 | **Effort:** 1 hour

**Problem:** Multiple goroutines can call `close(done)` concurrently in consumer shutdown.

**Solution:** Use `sync.Once` for channel close or atomic flag for shutdown state.

**Code:** TODO in [jetstream/user_consumer.go:114](../internal/atproto/jetstream/user_consumer.go#L114)

---

## 🔵 P3: Technical Debt

### Consolidate Environment Variable Validation
**Added:** 2025-10-11 | **Effort:** 2-3 hours

Create `internal/config` package with structured config validation. Fail fast with clear errors.

---

### Add Connection Pooling for PDS HTTP Clients
**Added:** 2025-10-11 | **Effort:** 2 hours

Create shared `http.Client` with connection pooling instead of new client per request.

---

### Architecture Decision Records (ADRs)
**Added:** 2025-10-11 | **Effort:** Ongoing

Document: did:plc choice, pgcrypto encryption, Jetstream vs firehose, write-forward pattern, single handle field.

---

### Replace log Package with Structured Logger
**Added:** 2025-10-11 | **Effort:** 1 day

**Problem:** Using standard `log` package. Need structured logging (JSON) with levels.

**Solution:** Switch to `slog`, `zap`, or `zerolog`. Add request IDs, context fields.

**Code:** TODO in [community/errors.go:46](../internal/api/handlers/community/errors.go#L46)

---

### PDS URL Resolution from DID
**Added:** 2025-10-11 | **Effort:** 2-3 hours

**Problem:** User consumer doesn't resolve PDS URL from DID document when missing.

**Solution:** Query PLC directory for DID document, extract `serviceEndpoint`.

**Code:** TODO in [jetstream/user_consumer.go:203](../internal/atproto/jetstream/user_consumer.go#L203)

---

### PLC Directory Registration (Production)
**Added:** 2025-10-11 | **Effort:** 1 day

**Problem:** DID generator creates did:plc but doesn't register in prod mode.

**Solution:** Implement PLC registration API call when `isDevEnv=false`.

**Code:** TODO in [did/generator.go:46](../internal/atproto/did/generator.go#L46)

---

## Recent Completions

### ✅ Token Refresh for Community Credentials (2025-10-17)
**Completed:** Automatic token refresh prevents communities from breaking after 2 hours

**Implementation:**
- ✅ JWT expiration parsing and refresh detection (5-minute buffer)
- ✅ Token refresh using Indigo SDK (`atproto.ServerRefreshSession`)
- ✅ Password fallback when refresh tokens expire (`atproto.ServerCreateSession`)
- ✅ Atomic credential updates in database (`UpdateCredentials`)
- ✅ Concurrency-safe with per-community mutex locking
- ✅ Structured logging for monitoring (`[TOKEN-REFRESH]` events)
- ✅ Integration tests for expiration detection and credential updates

**Files Created:**
- [internal/core/communities/token_utils.go](../internal/core/communities/token_utils.go)
- [internal/core/communities/token_refresh.go](../internal/core/communities/token_refresh.go)
- [tests/integration/token_refresh_test.go](../tests/integration/token_refresh_test.go)

**Files Modified:**
- [internal/core/communities/service.go](../internal/core/communities/service.go) - Added `ensureFreshToken` method
- [internal/core/communities/interfaces.go](../internal/core/communities/interfaces.go) - Added `UpdateCredentials` interface
- [internal/db/postgres/community_repo.go](../internal/db/postgres/community_repo.go) - Implemented `UpdateCredentials`

**Documentation:** [IMPLEMENTATION_TOKEN_REFRESH.md](../docs/IMPLEMENTATION_TOKEN_REFRESH.md)

**Impact:** Communities now work indefinitely without manual token management

---

### ✅ OAuth Authentication for Community Actions (2025-10-16)
**Completed:** Full OAuth JWT authentication flow for protected endpoints

**Implementation:**
- ✅ JWT parser compatible with atProto PDS tokens (aud/iss handling)
- ✅ Auth middleware protecting create/update/subscribe/unsubscribe endpoints
- ✅ Handler-level DID extraction from JWT tokens via `middleware.GetUserDID(r)`
- ✅ Removed all X-User-DID header placeholders
- ✅ E2E tests validate complete OAuth flow with real PDS tokens
- ✅ Security: Issuer validation supports both HTTPS URLs and DIDs

**Files Modified:**
- [internal/atproto/auth/jwt.go](../internal/atproto/auth/jwt.go) - JWT parsing with atProto compatibility
- [internal/api/middleware/auth.go](../internal/api/middleware/auth.go) - Auth middleware
- [internal/api/handlers/community/](../internal/api/handlers/community/) - All handlers updated
- [tests/integration/community_e2e_test.go](../tests/integration/community_e2e_test.go) - OAuth E2E tests

**Related:** Also implemented `hostedByDID` auto-population for security (see P1 item above)

---

### ✅ Fix .local TLD Bug (2025-10-11)
Changed default `INSTANCE_DID` from `did:web:coves.local` → `did:web:coves.social`. Fixed community creation failure due to disallowed `.local` TLD.

---

## Prioritization

- **P0:** Security vulns, data loss, prod blockers
- **P1:** Major UX/reliability issues
- **P2:** QOL improvements, minor bugs, docs
- **P3:** Refactoring, code quality
