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

## Recent Completions

### ✅ Fix .local TLD Bug (2025-10-11)
Changed default `INSTANCE_DID` from `did:web:coves.local` → `did:web:coves.social`. Fixed community creation failure due to disallowed `.local` TLD.

---

## Prioritization

- **P0:** Security vulns, data loss, prod blockers
- **P1:** Major UX/reliability issues
- **P2:** QOL improvements, minor bugs, docs
- **P3:** Refactoring, code quality
