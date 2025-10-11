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

## 🟡 P1: Important

### Token Refresh Logic for Community Credentials
**Added:** 2025-10-11 | **Effort:** 1-2 days

**Problem:** Community PDS access tokens expire (~2hrs). Updates fail until manual intervention.

**Solution:** Auto-refresh tokens before PDS operations. Parse JWT exp claim, use refresh token when expired, update DB.

**Code:** TODO in [communities/service.go:123](../internal/core/communities/service.go#L123)

---

### OAuth Authentication for Community Actions
**Added:** 2025-10-11 | **Effort:** 2-3 days

**Problem:** Subscribe/unsubscribe and community creation need authenticated user DID. Currently using placeholder.

**Solution:** Extract authenticated DID from OAuth session context. Requires OAuth middleware integration.

**Code:** Multiple TODOs in [community/subscribe.go](../internal/api/handlers/community/subscribe.go#L46), [community/create.go](../internal/api/handlers/community/create.go#L38)

---

## 🟢 P2: Nice-to-Have

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
