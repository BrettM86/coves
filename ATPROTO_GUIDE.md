# AT Protocol Implementation Guide

This guide provides comprehensive information about implementing AT Protocol (atproto) in the Coves platform.

## Table of Contents
- [Core Concepts](#core-concepts)
- [Architecture Overview](#architecture-overview)
- [Lexicons](#lexicons)
- [XRPC](#xrpc)
- [Data Storage](#data-storage)
- [Identity & Authentication](#identity--authentication)
- [Firehose & Sync](#firehose--sync)
- [Go Implementation Patterns](#go-implementation-patterns)
- [Best Practices](#best-practices)

## Core Concepts

### What is AT Protocol?
AT Protocol is a federated social networking protocol that enables:
- **Decentralized identity** - Users own their identity (DID) and can move between providers
- **Data portability** - Users can export and migrate their full social graph and content
- **Interoperability** - Different apps can interact with the same underlying data

### Key Components
1. **DIDs (Decentralized Identifiers)** - Persistent user identifiers (e.g., `did:plc:xyz123`)
2. **Handles** - Human-readable names that resolve to DIDs (e.g., `alice.bsky.social`)
3. **Repositories** - User data stored in PDS'
4. **Lexicons** - Schema definitions for data types and API methods
5. **XRPC** - The RPC protocol for client-server communication
6. **Firehose** - Real-time event stream of repository changes

## Architecture Overview

### Coves Architecture Pattern
Coves uses a simplified, single-database architecture that leverages existing atProto infrastructure:

#### Components

1. **PDS (Personal Data Server)**
   - Managed by Bluesky's official PDS implementation
   - Handles user repositories, DIDs, and CAR file storage
   - Users can use our PDS or any external PDS (federated)
   - Emits events to the Relay/firehose

2. **Relay (BigSky)**
   - Aggregates firehose events from multiple PDSs
   - For development: subscribes only to local dev PDS
   - For production: can subscribe to multiple PDSs or public relay

3. **AppView Database (Single PostgreSQL)**
   - **Purpose**: Denormalized, indexed data optimized for Coves queries
   - **Storage**: PostgreSQL with Coves-specific schema
   - **Contains**:
     - Indexed posts, communities, feeds
     - User read states and preferences
     - PDS metadata and record references
   - **Properties**:
     - Eventually consistent with PDS repositories
     - Can be rebuilt from firehose replay
     - Application-specific aggregations

4. **Coves AppView (Go Application)**
   - Subscribes to Relay firehose
   - Indexes relevant records into PostgreSQL
   - Serves XRPC queries for Coves features
   - Implements custom feed algorithms

### Data Flow

```
Write Path:
Client → PDS (via XRPC) → Repository Record Created
                                    ↓
                             Firehose Event
                                    ↓
                          Relay aggregates events
                                    ↓
                       Coves AppView subscribes
                                    ↓
                          Index in PostgreSQL

Read Path:
Client → Coves AppView (via XRPC) → PostgreSQL Query → Response
```

**Key Point**: Coves AppView only reads from the firehose and indexes data. It does NOT write to CAR files or manage repositories directly - the PDS handles that.

## Lexicons

### What are Lexicons?
Lexicons are JSON schema files that define:
- Data types (records stored in repositories)
- API methods (queries and procedures)
- Input/output schemas for API calls

### Lexicon Structure
```json
{
  "lexicon": 1,
  "id": "social.coves.community.profile",
  "defs": {
    "main": {
      "type": "record",
      "key": "self",
      "record": {
        "type": "object",
        "required": ["name", "createdAt"],
        "properties": {
          "name": {"type": "string", "maxLength": 64},
          "description": {"type": "string", "maxLength": 256},
          "rules": {"type": "array", "items": {"type": "string"}},
          "createdAt": {"type": "string", "format": "datetime"}
        }
      }
    }
  }
}
```

### Lexicon Types

#### 1. Record Types
Define data structures stored in user repositories:
```json
{
  "type": "record",
  "key": "tid|rkey|literal",
  "record": { /* schema */ }
}
```

#### 2. Query Types (Read-only)
Define read operations that don't modify state:
```json
{
  "type": "query",
  "parameters": { /* input schema */ },
  "output": { /* response schema */ }
}
```

#### 3. Procedure Types (Write)
Define operations that modify repositories:
```json
{
  "type": "procedure", 
  "input": { /* request body schema */ },
  "output": { /* response schema */ }
}
```

### Naming Conventions
- Use reverse-DNS format: `social.coves.community.profile`
- Queries often start with `get`, `list`, or `search`
- Procedures often start with `create`, `update`, or `delete`
- Keep names descriptive but concise


## Identity & Authentication

### DIDs (Decentralized Identifiers)
- Permanent, unique identifiers for users
- Two types supported:
  - `did:plc:*` - Hosted by PLC Directory
  - `did:web:*` - Self-hosted

### Handle Resolution
Handles resolve to DIDs via:
1. DNS TXT record: `_atproto.alice.com → did:plc:xyz`
2. HTTPS well-known: `https://alice.com/.well-known/atproto-did`

### Authentication Flow
1. Client creates session with OAuth

## Firehose & Sync

### Firehose Events
Real-time stream of repository changes:
- Commit events (creates, updates, deletes)
- Identity events (handle changes)
- Account events (status changes)

### Subscribing to Firehose
Connect via WebSocket to `com.atproto.sync.subscribeRepos`:
```
wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos
```

### Processing Events
- Events include full record data and operation type
- Process events to update AppView database
- Handle out-of-order events with sequence numbers

## Go Implementation Patterns

### Using Indigo Library
Bluesky's official Go implementation provides:
- Lexicon code generation
- XRPC client/server
- Firehose subscription

### Code Generation
Generate Go types from Lexicons:
```bash
go run github.com/bluesky-social/indigo/cmd/lexgen \
  --package coves \
  --prefix social.coves \
  --outdir api/coves \
  lexicons/social/coves/*.json
```

### Repository Operations
```go
// Write to repository
rkey := models.GenerateTID()
err := repoStore.CreateRecord(ctx, userDID, "social.coves.post", rkey, &Post{
    Text:      "Hello",
    CreatedAt: time.Now().Format(time.RFC3339),
})

// Read from repository  
records, err := repoStore.ListRecords(ctx, userDID, "social.coves.post", limit, cursor)
```

### XRPC Handler Pattern
```go
func (s *Server) HandleGetCommunity(ctx context.Context) error {
    // 1. Parse and validate input
    id := xrpc.QueryParam(ctx, "id")
    
    // 2. Call service layer
    community, err := s.communityService.GetByID(ctx, id)
    if err != nil {
        return err
    }
    
    // 3. Return response
    return xrpc.WriteJSONResponse(ctx, community)
}
```

## Best Practices

### 1. Lexicon Design
- Keep schemas focused and single-purpose
- Use references (`$ref`) for shared types
- Version carefully - Lexicons are contracts
- Document thoroughly with descriptions

### 2. Data Modeling
- Store minimal data in repositories
- Denormalize extensively in AppView
- Use record keys that are meaningful
- Plan for data portability

### 3. Performance
- Batch firehose processing
- Use database transactions wisely
- Index AppView tables appropriately
- Cache frequently accessed data

### 4. Error Handling
- Use standard XRPC error codes
- Provide meaningful error messages
- Handle network failures gracefully
- Implement proper retry logic

### 5. Security
- Validate all inputs against Lexicons
- Verify signatures on repository data
- Rate limit API endpoints
- Sanitize user-generated content

### 6. Federation
- Design for multi-instance deployment
- Handle remote user identities
- Respect instance-specific policies
- Plan for cross-instance data sync

## Common Patterns

### Handling User Content
- Always validate against Lexicon schemas
- Store in user's repository via CAR files
- Index in AppView for efficient queries
- Emit firehose events for subscribers

## Resources

### Official Documentation
- [ATProto Specifications](https://atproto.com/specs)
- [Lexicon Documentation](https://atproto.com/specs/lexicon)
- [XRPC Specification](https://atproto.com/specs/xrpc)

### Reference Implementations
- [Indigo (Go)](https://github.com/bluesky-social/indigo)
- [ATProto SDK (TypeScript)](https://github.com/bluesky-social/atproto)

### Tools
- [Lexicon CLI](https://github.com/bluesky-social/atproto/tree/main/packages/lex-cli)
- [goat CLI](https://github.com/bluesky-social/indigo/tree/main/cmd/goat)