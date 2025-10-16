# Backlog PRD: Platform Improvements & Technical Debt

**Status:** Ongoing
**Owner:** Platform Team
**Last Updated:** 2025-10-11

## Overview

Miscellaneous platform improvements, bug fixes, and technical debt that don't fit into feature-specific PRDs.

---

## 🔴 P0: Critical Security

### did:web Domain Verification
**Added:** 2025-10-11 | **Effort:** 2-3 days | **Severity:** Medium

**Problem:** Self-hosters can set `INSTANCE_DID=did:web:nintendo.com` without owning the domain, enabling domain impersonation attacks (e.g., `mario.communities.nintendo.com` on malicious instance).

**Solution:** Implement did:web verification per [atProto spec](https://atproto.com/specs/did-web) - fetch `https://domain/.well-known/did.json` on startup and verify it matches claimed DID. Add `SKIP_DID_WEB_VERIFICATION=true` for dev mode.

**Current Status:**
- ✅ Default changed from `coves.local` → `coves.social` (fixes `.local` TLD bug)
- ✅ TODO comment in [cmd/server/main.go:126-131](../cmd/server/main.go#L126-L131)
- ⚠️ Verification not implemented

---

## 🟡 P1: Important (Alpha Blockers)

### Token Refresh Logic for Community Credentials
**Added:** 2025-10-11 | **Effort:** 1-2 days | **Priority:** ALPHA BLOCKER

**Problem:** Community PDS access tokens expire (~2hrs). Updates fail until manual intervention.

**Solution:** Auto-refresh tokens before PDS operations. Parse JWT exp claim, use refresh token when expired, update DB.

**Code:** TODO in [communities/service.go:123](../internal/core/communities/service.go#L123)

---

### OAuth Authentication for Community Actions
**Added:** 2025-10-11 | **Effort:** 2-3 days | **Priority:** ALPHA BLOCKER

**Problem:** Subscribe/unsubscribe and community creation need authenticated user DID. Currently using placeholder.

**Solution:** Extract authenticated DID from OAuth session context. Requires OAuth middleware integration.

**Code:** Multiple TODOs in [community/subscribe.go](../internal/api/handlers/community/subscribe.go#L46), [community/create.go](../internal/api/handlers/community/create.go#L38), [community/update.go](../internal/api/handlers/community/update.go#L47)

---

### Subscription Visibility Level (Feed Slider 1-5 Scale)
**Added:** 2025-10-15 | **Effort:** 4-6 hours | **Priority:** ALPHA BLOCKER

**Problem:** Users can't control how much content they see from each community. Lexicon has `contentVisibility` (1-5 scale) but code doesn't use it.

**Solution:**
- Update subscribe handler to accept `contentVisibility` parameter (1-5, default 3)
- Store in subscription record on PDS
- Update feed generation to respect visibility level (beta work, but data structure needed now)

**Code:**
- Lexicon: [subscription.json:28-34](../internal/atproto/lexicon/social/coves/actor/subscription.json#L28-L34) ✅ Ready
- Handler: [community/subscribe.go](../internal/api/handlers/community/subscribe.go) - Add parameter
- Service: [communities/service.go:373-376](../internal/core/communities/service.go#L373-L376) - Add to record

**Impact:** Without this, users have no way to adjust feed volume per community (key feature from DOMAIN_KNOWLEDGE.md)

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

### ✅ Fix .local TLD Bug (2025-10-11)
Changed default `INSTANCE_DID` from `did:web:coves.local` → `did:web:coves.social`. Fixed community creation failure due to disallowed `.local` TLD.

---

## Prioritization

- **P0:** Security vulns, data loss, prod blockers
- **P1:** Major UX/reliability issues
- **P2:** QOL improvements, minor bugs, docs
- **P3:** Refactoring, code quality
