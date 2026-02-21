# End-to-End Testing Guide

## Overview

Coves supports full E2E testing with a local atProto stack:

```
Third-party Client → Coves XRPC → PDS → Jetstream → Coves AppView → PostgreSQL
```

**Why Jetstream?**
- PDS emits raw CBOR-encoded firehose (binary, hard to parse)
- Jetstream converts CBOR → clean JSON (same format as production)
- Tests exactly match production behavior

---

## Quick Start

### 1. Start Development Stack

```bash
make dev-up
```

This starts:
- **PostgreSQL** (port 5433) - Coves database
- **PDS** (port 3001) - Local atProto server
- **Jetstream** (port 6008) - CBOR → JSON converter (always runs for read-forward)

> **Note:** Jetstream is now part of `dev-up` since read-forward architecture requires it

### 2. Start AppView

```bash
# In another terminal
make run  # Starts AppView (auto-runs migrations)
```

AppView will connect to `ws://localhost:6008/subscribe` (configured in `.env.dev`)

### 3. Run Automated E2E Tests

```bash
make e2e-test
```

This runs the full test suite:
- Creates accounts via XRPC endpoint
- Verifies PDS account creation
- Validates Jetstream indexing
- Confirms database storage

### 4. Manual Testing (Optional)

#### Create User via Coves XRPC

```bash
curl -X POST http://localhost:8081/xrpc/social.coves.actor.signup \
  -H "Content-Type: application/json" \
  -d '{
    "handle": "alice.local.coves.dev",
    "email": "alice@test.com",
    "password": "test1234"
  }'
```

**Response:**
```json
{
  "did": "did:plc:xyz123...",
  "handle": "alice.local.coves.dev",
  "accessJwt": "eyJ...",
  "refreshJwt": "eyJ..."
}
```

**What happens:**
1. Coves XRPC handler receives signup request
2. Calls PDS `com.atproto.server.createAccount`
3. PDS creates account → emits to firehose
4. Jetstream converts event → JSON
5. AppView receives JSON → indexes user
6. User appears in PostgreSQL `users` table

### 5. Verify Indexing

Check AppView logs:
```
2025/01/15 12:00:00 Identity event: did:plc:xyz123 → alice.local.coves.dev
2025/01/15 12:00:00 Indexed new user: alice.local.coves.dev (did:plc:xyz123)
```

Query via API:
```bash
curl "http://localhost:8081/xrpc/social.coves.actor.getProfile?actor=alice.local.coves.dev"
```

Expected response:
```json
{
  "did": "did:plc:xyz123...",
  "profile": {
    "handle": "alice.local.coves.dev",
    "createdAt": "2025-01-15T12:00:00Z"
  }
}
```

---

## Workflow Summary

### Daily Development
```bash
make dev-up    # Start PDS + PostgreSQL + Jetstream (once)
make run       # Start AppView (in another terminal)
make test      # Run fast tests during development
```

### Before Commits/PRs
```bash
make e2e-test  # Run full E2E test suite
```

### Reset Everything
```bash
make dev-reset  # Nuclear option - deletes all data
make dev-up     # Start fresh
```

---

## Testing Scenarios

### Automated E2E Test Suite

```bash
make e2e-test
```

This tests:
- ✅ Single account creation via XRPC
- ✅ Idempotent duplicate event handling
- ✅ Multiple concurrent user indexing

### Manual: User Registration via XRPC

```bash
curl -X POST http://localhost:8081/xrpc/social.coves.actor.signup \
  -H "Content-Type: application/json" \
  -d '{"handle":"bob.local.coves.dev","email":"bob@test.com","password":"pass1234"}'
```

### Manual: Federated User (Direct PDS)

```bash
# Simulates a user on another PDS
curl -X POST http://localhost:3001/xrpc/com.atproto.server.createAccount \
  -H "Content-Type: application/json" \
  -d '{"handle":"charlie.local.coves.dev","email":"charlie@test.com","password":"pass1234"}'

# Coves AppView will still index via Jetstream (read-forward)
```

---

## Next Steps

1. ✅ E2E testing infrastructure complete
2. ✅ Automated E2E test suite implemented
3. ✅ XRPC signup endpoint for third-party clients
4. 🔨 TODO: Add handle update support
5. 🔨 TODO: Add CI/CD E2E tests
