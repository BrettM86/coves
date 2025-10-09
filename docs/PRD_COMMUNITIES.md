# Communities PRD: Federated Forum System

**Status:** Draft
**Owner:** Platform Team
**Last Updated:** 2025-10-07

## Overview

Coves communities are federated, instance-scoped forums built on atProto. Each community is identified by a scoped handle (`!gaming@coves.social`) and owned by a DID, enabling future portability and community governance.

## Vision

**V1 (MVP):** Instance-owned communities with scoped handles
**V2 (Post-Launch):** Cross-instance discovery and moderation signal federation
**V3 (Future):** Community-owned DIDs with migration capabilities via community voting

## Core Principles

1. **Scoped by default:** All communities use `!name@instance.com` format
2. **DID-based ownership:** Communities are owned by DIDs (initially instance, eventually community)
3. **Web DID compatible:** Communities can use `did:web` for custom domains (e.g., `!photography@lens.club`)
4. **Federation-ready:** Design for cross-instance discovery and moderation from day one
5. **Community sovereignty:** Future path to community ownership and migration

## Identity & Namespace

### Community Handle Format

```
!{name}@{instance}

Examples:
!gaming@coves.social
!photography@lens.club
!golang@dev.forums
!my-book-club@personal.coves.io
```

### DID Ownership

**V1: Instance-Owned**
```json
{
  "community": {
    "handle": "!gaming@coves.social",
    "did": "did:web:coves.social:community:gaming",
    "owner": "did:web:coves.social",
    "createdBy": "did:plc:user123",
    "hostedBy": "did:web:coves.social",
    "created": "2025-10-07T12:00:00Z"
  }
}
```

**Future: Community-Owned**
```json
{
  "community": {
    "handle": "!gaming@coves.social",
    "did": "did:web:gaming.community",
    "owner": "did:web:gaming.community",
    "createdBy": "did:plc:user123",
    "hostedBy": "did:web:coves.social",
    "governance": {
      "type": "multisig",
      "votingEnabled": true
    }
  }
}
```

### Why Scoped Names?

- **No namespace conflicts:** Each instance controls its own namespace
- **Clear ownership:** `@instance` shows who hosts it
- **Decentralized:** No global registry required
- **Web DID ready:** Communities can become `did:web` and use custom domains
- **Fragmentation handled socially:** Community governance and moderation quality drives membership

## Visibility & Discoverability

### Visibility Tiers

**Public (Default)**
- Indexed by home instance
- Appears in search results
- Listed in community directory
- Can be federated to other instances

**Unlisted**
- Accessible via direct link
- Not in search results
- Not in public directory
- Members can invite others

**Private**
- Invite-only
- Not discoverable
- Not federated
- Requires approval to join

### Discovery Configuration

```go
type CommunityVisibility struct {
    Level                  string   // "public", "unlisted", "private"
    AllowExternalDiscovery bool     // Can other instances index this?
    AllowedInstances       []string // Whitelist (empty = all if public)
}
```

**Examples:**
```json
// Public gaming community, federate everywhere
{
  "visibility": "public",
  "allowExternalDiscovery": true,
  "allowedInstances": []
}

// Book club, public on home instance only
{
  "visibility": "public",
  "allowExternalDiscovery": false,
  "allowedInstances": []
}

// Private beta testing community
{
  "visibility": "private",
  "allowExternalDiscovery": false,
  "allowedInstances": ["coves.social", "trusted.instance"]
}
```

## Moderation & Federation

### Moderation Actions (Local Only)

Communities can be moderated locally by the hosting instance:

```go
type ModerationAction struct {
    CommunityDID string
    Action       string // "delist", "quarantine", "remove"
    Reason       string
    Instance     string
    Timestamp    time.Time
    BroadcastSignal bool // Share with network?
}
```

**Action Types:**

**Delist**
- Removed from search/directory
- Existing members can still access
- Not deleted, just hidden

**Quarantine**
- Visible with warning label
- "This community may violate guidelines"
- Can still be accessed with acknowledgment

**Remove**
- Community hidden from instance AppView
- Data still exists in firehose
- Other instances can choose to ignore removal

### Federation Reality

**What you can control:**
- What YOUR AppView indexes
- What moderation signals you broadcast
- What other instances' signals you honor

**What you cannot control:**
- Self-hosted PDS/AppView can index anything
- Other instances may ignore your moderation
- Community data lives in firehose regardless

**Moderation is local AppView filtering, not network-wide censorship.**

### Moderation Signal Federation (V2)

Instances can subscribe to each other's moderation feeds:

```json
{
  "moderationFeed": "did:web:coves.social:moderation",
  "action": "remove",
  "target": "did:web:coves.social:community:hate-speech",
  "reason": "Violates community guidelines",
  "timestamp": "2025-10-07T14:30:00Z",
  "evidence": "https://coves.social/moderation/case/123"
}
```

Other instances can:
- Auto-apply trusted instance moderation
- Show warnings based on signals
- Ignore signals entirely

## MVP (V1) Scope

### ✅ Completed (2025-10-08)

**Core Functionality:**
- [x] Create communities (instance-owned DID)
- [x] Scoped handle format (`!name@instance`)
- [x] Three visibility levels (public, unlisted, private)
- [x] Basic community metadata (name, description, rules)
- [x] Write-forward to PDS (communities as atProto records)
- [x] Jetstream consumer (index communities from firehose)

**Technical Infrastructure:**
- [x] Lexicon: `social.coves.community.profile` with `did` field (atProto compliant!)
- [x] DID format: `did:plc:xxx` (portable, federated)
- [x] PostgreSQL indexing for local communities
- [x] Service layer (business logic)
- [x] Repository layer (database)
- [x] Consumer layer (firehose indexing)
- [x] Environment config (`IS_DEV_ENV`, `PLC_DIRECTORY_URL`)

**Critical Fixes:**
- [x] Fixed `record_uri` bug (now points to correct repository location)
- [x] Added required `did` field to lexicon (atProto compliance)
- [x] Consumer correctly separates community DID from repository DID
- [x] E2E test passes (PDS write → firehose → AppView indexing)

### 🚧 In Progress

**API Endpoints (XRPC):**
- [x] `social.coves.community.create` (handler exists, needs testing)
- [ ] `social.coves.community.get` (handler exists, needs testing)
- [ ] `social.coves.community.list` (handler exists, needs testing)
- [ ] `social.coves.community.search` (handler exists, needs testing)
- [x] `social.coves.community.subscribe` (handler exists)
- [x] `social.coves.community.unsubscribe` (handler exists)

**Subscriptions & Memberships:**
- [x] Database schema (subscriptions, memberships tables)
- [x] Repository methods (subscribe, unsubscribe, list)
- [ ] Consumer processing (index subscription events from firehose)
- [ ] Membership tracking (convert subscription → membership on first post?)

### ⏳ TODO Before V1 Launch

**Critical Path:**
- [ ] Test all XRPC endpoints end-to-end
- [ ] Implement OAuth middleware (protect create/update endpoints)
- [ ] Add authorization checks (who can create/update/delete?)
- [ ] Handle validation (prevent duplicate handles, validate DIDs)
- [ ] Rate limiting (prevent community spam)

**Community Discovery:**
- [ ] Community list endpoint (pagination, filtering)
- [ ] Community search (full-text search on name/description)
- [ ] Visibility enforcement (respect public/unlisted/private)
- [ ] Federation config (respect `allowExternalDiscovery`)

**Posts in Communities:**
- [ ] Extend `social.coves.post` lexicon with `community` field
- [ ] Create post endpoint (require community membership?)
- [ ] Feed generation (show posts in community)
- [ ] Post consumer (index community posts from firehose)

**Moderation (Basic):**
- [ ] Remove community from AppView (delist)
- [ ] Quarantine community (show warning)
- [ ] Moderation audit log
- [ ] Admin endpoints (for instance operators)

**Testing & Documentation:**
- [ ] Integration tests for all flows
- [ ] API documentation (XRPC endpoints)
- [ ] Deployment guide (PDS setup, environment config)
- [ ] Migration guide (how to upgrade from test to production)

### Out of Scope (V2+)

- [ ] Moderation signal federation
- [ ] Community-owned DIDs
- [ ] Migration/portability
- [ ] Governance voting
- [ ] Custom domain DIDs

## Phase 2: Federation & Discovery

**Goals:**
- Cross-instance community search
- Federated moderation signals
- Trust networks between instances

**Features:**
```go
// Cross-instance discovery
type FederationConfig struct {
    DiscoverPeers        []string // Other Coves instances to index
    TrustModerationFrom  []string // Auto-apply moderation signals
    ShareCommunitiesWith []string // Allow these instances to index ours
}

// Moderation trust network
type ModerationTrust struct {
    InstanceDID string
    TrustLevel  string // "auto-apply", "show-warning", "ignore"
    Categories  []string // Which violations to trust ("spam", "nsfw", etc)
}
```

**User Experience:**
```
Search: "golang"

Results:
!golang@coves.social (45k members)
  Hosted on coves.social
  [Join]

!golang@dev.forums (12k members)
  Hosted on dev.forums
  Focused on systems programming
  [Join]

!go@programming.zone (3k members)
  Hosted on programming.zone
  ⚠️ Flagged by trusted moderators
  [View Details]
```

## Implementation Log

### 2025-10-08: DID Architecture & atProto Compliance

**Major Decisions:**

1. **Migrated from `did:coves` to `did:plc`**
   - Communities now use proper PLC DIDs (portable across instances)
   - Added `IS_DEV_ENV` flag (dev = generate without PLC registration, prod = register)
   - Matches Bluesky's feed generator pattern

2. **Fixed Critical `record_uri` Bug**
   - Problem: Consumer was setting community DID as repository owner
   - Fix: Correctly separate community DID (entity) from repository DID (storage)
   - Result: URIs now point to actual data location (federation works!)

3. **Added Required `did` Field to Lexicon**
   - atProto research revealed communities MUST have their own DID field
   - Matches `app.bsky.feed.generator` pattern (service has DID, record stored elsewhere)
   - Enables future migration to community-owned repositories

**Architecture Insights:**

```
User Profile (Bluesky):
  at://did:plc:user123/app.bsky.actor.profile/self
  ↑ Repository location IS the identity
  No separate "did" field needed

Feed Generator (Bluesky):
  at://did:plc:creator456/app.bsky.feed.generator/cool-feed
  Record contains: {"did": "did:web:feedgen.service", ...}
  ↑ Service has own DID, record stored in creator's repo

Community (Coves V1):
  at://did:plc:instance123/social.coves.community.profile/rkey
  Record contains: {"did": "did:plc:community789", ...}
  ↑ Community has own DID, record stored in instance repo

Community (Coves V2 - Future):
  at://did:plc:community789/social.coves.community.profile/self
  Record contains: {"owner": "did:plc:instance123", ...}
  ↑ Community owns its own repo, instance manages it
```

**Key Findings:**

1. **Keypair Management**: Coves can manage community keypairs (like Bluesky manages user keys)
2. **PDS Authentication**: Can create PDS accounts for communities, Coves stores credentials
3. **Migration Path**: Current V1 enables future V2 without breaking changes

**Trade-offs:**

- V1 (Current): Simple, ships fast, limited portability
- V2 (Future): Complex, true portability, matches atProto entity model

**Decision: Ship V1 now, plan V2 migration.**

---

## CRITICAL: DID Architecture Decision (2025-10-08)

### Current State: Hybrid Approach

**V1 Implementation (Current):**
```
Community DID:     did:plc:community789  (portable identity)
Repository:        at://did:plc:instance123/social.coves.community.profile/rkey
Owner:             did:plc:instance123   (instance manages it)

Record structure:
{
  "did": "did:plc:community789",        // Community's portable DID
  "owner": "did:plc:instance123",       // Instance owns the repository
  "hostedBy": "did:plc:instance123",    // Where it's currently hosted
  "createdBy": "did:plc:user456"        // User who created it
}
```

**Why this matters:**
- ✅ Community has portable DID (can be referenced across network)
- ✅ Record URI points to actual data location (federation works)
- ✅ Clear separation: community identity ≠ storage location
- ⚠️ Limited portability: Moving instances requires deleting/recreating record

### V2 Option: True Community Repositories

**Future Architecture (under consideration):**
```
Community DID:     did:plc:community789
Repository:        at://did:plc:community789/social.coves.community.profile/self
Owner:             did:plc:instance123 (in metadata, not repo owner)

Community gets:
- Own PDS account (managed by Coves backend)
- Own signing keypair (stored by Coves, like Bluesky stores user keys)
- Own repository (true data portability)
```

**Benefits:**
- ✅ True portability: URI never changes when migrating
- ✅ Matches atProto entity model (feed generators, labelers)
- ✅ Community can move between instances via DID document update

**Complexity:**
- Coves must generate keypairs for each community
- Coves must create PDS accounts for each community
- Coves must securely store community credentials
- More infrastructure to manage

**Decision:** Start with V1 (current), plan for V2 migration path.

### Migration Path V1 → V2

When ready for true portability:
1. Generate keypair for existing community
2. Register community's DID document with PLC
3. Create PDS account for community (Coves manages credentials)
4. Migrate record from instance repo to community repo
5. Update AppView to index from new location

The `did` field in records makes this migration possible!

## Phase 3: Community Ownership

**Goals:**
- Transfer ownership from instance to community
- Enable community governance
- Allow migration between instances

**Features:**

**Governance System:**
```go
type CommunityGovernance struct {
    Enabled       bool
    VotingPower   string // "one-person-one-vote", "reputation-weighted"
    QuorumPercent int    // % required for votes to pass
    Moderators    []string // DIDs with mod powers
}
```

**Migration Flow:**
```
1. Community votes on migration (e.g., from coves.social to gaming.forum)
2. Vote passes (66% threshold)
3. Community DID ownership transfers
4. New instance re-indexes community data from firehose
5. Handle updates: !gaming@gaming.forum
6. Old instance can keep archive or redirect
```

**DID Transfer:**
```json
{
  "community": "!gaming@gaming.forum",
  "did": "did:web:gaming.community",
  "previousHost": "did:web:coves.social",
  "currentHost": "did:web:gaming.forum",
  "transferredAt": "2025-12-15T10:00:00Z",
  "governanceSignatures": ["sig1", "sig2", "sig3"]
}
```

## Lexicon Design

### `social.coves.community`

```json
{
  "lexicon": 1,
  "id": "social.coves.community",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": ["handle", "name", "createdAt"],
        "properties": {
          "handle": {
            "type": "string",
            "description": "Scoped handle (!name@instance)"
          },
          "name": {
            "type": "string",
            "maxLength": 64,
            "description": "Display name"
          },
          "description": {
            "type": "string",
            "maxLength": 3000
          },
          "rules": {
            "type": "array",
            "items": {"type": "string"}
          },
          "visibility": {
            "type": "string",
            "enum": ["public", "unlisted", "private"],
            "default": "public"
          },
          "federation": {
            "type": "object",
            "properties": {
              "allowExternalDiscovery": {"type": "boolean", "default": true},
              "allowedInstances": {
                "type": "array",
                "items": {"type": "string"}
              }
            }
          },
          "owner": {
            "type": "string",
            "description": "DID of community owner"
          },
          "createdBy": {
            "type": "string",
            "description": "DID of user who created community"
          },
          "hostedBy": {
            "type": "string",
            "description": "DID of hosting instance"
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          }
        }
      }
    }
  }
}
```

### `social.coves.post` (Community Extension)

```json
{
  "properties": {
    "community": {
      "type": "string",
      "description": "DID of community this post belongs to"
    }
  }
}
```

## Technical Architecture

### Data Flow

```
User creates community
  ↓
PDS creates community record
  ↓
Firehose broadcasts creation
  ↓
AppView indexes community (if allowed)
  ↓
PostgreSQL stores community metadata
  ↓
Community appears in local search/directory
```

### Database Schema (AppView)

```sql
CREATE TABLE communities (
    id SERIAL PRIMARY KEY,
    did TEXT UNIQUE NOT NULL,
    handle TEXT UNIQUE NOT NULL, -- !name@instance
    name TEXT NOT NULL,
    description TEXT,
    rules JSONB,
    visibility TEXT NOT NULL DEFAULT 'public',
    federation_config JSONB,
    owner_did TEXT NOT NULL,
    created_by_did TEXT NOT NULL,
    hosted_by_did TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    member_count INTEGER DEFAULT 0,
    post_count INTEGER DEFAULT 0
);

CREATE INDEX idx_communities_handle ON communities(handle);
CREATE INDEX idx_communities_visibility ON communities(visibility);
CREATE INDEX idx_communities_hosted_by ON communities(hosted_by_did);

CREATE TABLE community_moderation (
    id SERIAL PRIMARY KEY,
    community_did TEXT NOT NULL REFERENCES communities(did),
    action TEXT NOT NULL, -- 'delist', 'quarantine', 'remove'
    reason TEXT,
    instance_did TEXT NOT NULL,
    broadcast BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL
);
```

## API Endpoints (XRPC)

### V1 (MVP)

```
social.coves.community.create
social.coves.community.get
social.coves.community.update
social.coves.community.list
social.coves.community.search
social.coves.community.join
social.coves.community.leave
```


### V3 (Governance)

```
social.coves.community.transferOwnership
social.coves.community.proposeVote
social.coves.community.castVote
social.coves.community.migrate
```

## Success Metrics

### V1 (MVP)
- [ ] Communities can be created with scoped handles
- [ ] Posts can be made to communities
- [ ] Community discovery works on local instance
- [ ] All three visibility levels function correctly
- [ ] Basic moderation (delist/remove) works

### V2 (Federation)
- [ ] Cross-instance community search returns results
- [ ] Moderation signals are broadcast and received
- [ ] Trust networks prevent spam communities

### V3 (Governance)
- [ ] Community ownership can be transferred
- [ ] Voting system enables community decisions
- [ ] Communities can migrate between instances

## Security Considerations

### Every Operation Must:
- [ ] Validate DID ownership
- [ ] Check community visibility settings
- [ ] Verify instance authorization
- [ ] Use parameterized queries
- [ ] Rate limit community creation
- [ ] Log moderation actions

### Risks & Mitigations:

**Community Squatting**
- Risk: Instance creates popular names and sits on them
- Mitigation: Activity requirements (auto-archive inactive communities)

**Spam Communities**
- Risk: Bad actors create thousands of spam communities
- Mitigation: Rate limits, moderation signals, trust networks

**Migration Abuse**
- Risk: Community ownership stolen via fake votes
- Mitigation: Governance thresholds, time locks, signature verification

**Privacy Leaks**
- Risk: Private communities discovered via firehose
- Mitigation: Encrypt sensitive metadata, only index allowed instances

## Open Questions

1. **Should we support community aliases?** (e.g., `!gaming` → `!videogames`)
2. **What's the minimum member count for community creation?** (prevent spam)
3. **How do we handle abandoned communities?** (creator leaves, no mods)
4. **Should communities have their own PDS?** (advanced self-hosting)
5. **Cross-posting between communities?** (one post in multiple communities)

## Migration from V1 → V2 → V3

### V1 to V2 (Adding Federation)
- Backward compatible: All V1 communities work in V2
- New fields added to lexicon (optional)
- Existing communities opt-in to federation

### V2 to V3 (Community Ownership)
- Instance can propose ownership transfer to community
- Community votes to accept
- DID ownership updates
- No breaking changes to existing communities

## References

- atProto Lexicon Spec: https://atproto.com/specs/lexicon
- DID Web Spec: https://w3c-ccg.github.io/did-method-web/
- Bluesky Handle System: https://atproto.com/specs/handle
- Coves Builder Guide: `/docs/CLAUDE-BUILD.md`

## Approval & Sign-Off

- [ ] Product Lead Review
- [ ] Engineering Lead Review
- [ ] Security Review
- [ ] Legal/Policy Review (especially moderation aspects)

---

**Next Steps:**
1. Review and approve PRD
2. Create V1 implementation tickets
3. Design lexicon schema
4. Build community creation flow
5. Implement local discovery
6. Write integration tests
