# Aggregator Setup Guide

This guide explains how to set up and register an aggregator with Coves instances.

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Detailed Setup Steps](#detailed-setup-steps)
- [Authorization Process](#authorization-process)
- [Posting to Communities](#posting-to-communities)
- [Rate Limits](#rate-limits)
- [Security Best Practices](#security-best-practices)
- [Troubleshooting](#troubleshooting)
- [API Reference](#api-reference)

## Overview

**Aggregators** are automated services that post content to Coves communities. They are similar to Bluesky's feed generators and labelers - self-managed external services that integrate with the platform.

**Key characteristics**:
- Self-owned: You create and manage your own PDS account
- Domain-verified: Prove ownership via `.well-known/atproto-did`
- Community-authorized: Moderators grant posting permission per-community
- Rate-limited: 10 posts per hour per community

**Example use cases**:
- RSS feed aggregators (tech news, blog posts)
- Social media cross-posters (Twitter → Coves)
- Event notifications (GitHub releases, weather alerts)
- Content curation bots (daily links, summaries)

## Architecture

### Data Flow

```
┌──────────────────────────────────────────────────────────┐
│ 1. One-Time Setup                                        │
├──────────────────────────────────────────────────────────┤
│ Aggregator creates PDS account                           │
│   ↓                                                       │
│ Proves domain ownership (.well-known)                    │
│   ↓                                                       │
│ Registers with Coves (enters users table)                │
│   ↓                                                       │
│ Writes service declaration                               │
│   ↓                                                       │
│ Jetstream indexes into aggregators table                 │
└──────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────┐
│ 2. Per-Community Authorization                           │
├──────────────────────────────────────────────────────────┤
│ Moderator writes authorization record                    │
│   ↓                                                       │
│ Jetstream indexes into aggregator_authorizations         │
└──────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────┐
│ 3. Posting (Ongoing)                                     │
├──────────────────────────────────────────────────────────┤
│ Aggregator calls post creation endpoint                  │
│   ↓                                                       │
│ Handler validates:                                        │
│   - Author in users table ✓                              │
│   - Author in aggregators table ✓                        │
│   - Authorization exists ✓                               │
│   - Rate limit not exceeded ✓                            │
│   ↓                                                       │
│ Post written to community's PDS                          │
│   ↓                                                       │
│ Jetstream indexes post                                   │
└──────────────────────────────────────────────────────────┘
```

### Database Tables

**users** - All actors (users, communities, aggregators)
```sql
CREATE TABLE users (
    did TEXT PRIMARY KEY,
    handle TEXT NOT NULL,
    pds_url TEXT,
    indexed_at TIMESTAMPTZ
);
```

**aggregators** - Aggregator-specific metadata
```sql
CREATE TABLE aggregators (
    did TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    description TEXT,
    avatar_url TEXT,
    config_schema JSONB,
    source_url TEXT,
    maintainer_did TEXT,
    record_uri TEXT NOT NULL UNIQUE,
    record_cid TEXT NOT NULL,
    created_at TIMESTAMPTZ,
    indexed_at TIMESTAMPTZ
);
```

**aggregator_authorizations** - Community authorizations
```sql
CREATE TABLE aggregator_authorizations (
    id BIGSERIAL PRIMARY KEY,
    aggregator_did TEXT NOT NULL,
    community_did TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    config JSONB,
    created_by TEXT,
    record_uri TEXT NOT NULL UNIQUE,
    record_cid TEXT NOT NULL,
    UNIQUE(aggregator_did, community_did)
);
```

## Prerequisites

1. **Domain ownership**: You must own a domain where you can host static files over HTTPS
2. **Web server**: Ability to serve the `.well-known/atproto-did` file
3. **Development tools**: `curl`, `jq`, basic shell scripting knowledge
4. **Email address**: For creating the PDS account

**Optional**:
- Custom avatar image (PNG/JPEG/WebP, max 1MB)
- GitHub repository for source code transparency

## Quick Start

We provide automated setup scripts:

```bash
cd scripts/aggregator-setup

# Make scripts executable
chmod +x *.sh

# Run setup scripts in order
./1-create-pds-account.sh
./2-setup-wellknown.sh
# (Upload .well-known to your web server)
./3-register-with-coves.sh
./4-create-service-declaration.sh
```

See [scripts/aggregator-setup/README.md](../../scripts/aggregator-setup/README.md) for detailed script documentation.

## Detailed Setup Steps

### Step 1: Create PDS Account

Your aggregator needs its own atProto identity (DID). The easiest way is to create an account on an existing PDS.

**Using an existing PDS (recommended)**:

```bash
curl -X POST https://bsky.social/xrpc/com.atproto.server.createAccount \
  -H "Content-Type: application/json" \
  -d '{
    "handle": "mynewsbot.bsky.social",
    "email": "bot@example.com",
    "password": "secure-password-here"
  }'
```

**Response**:
```json
{
  "accessJwt": "eyJ...",
  "refreshJwt": "eyJ...",
  "handle": "mynewsbot.bsky.social",
  "did": "did:plc:abc123...",
  "didDoc": {...}
}
```

**Save these credentials securely!** You'll need the DID and access token for all subsequent operations.

**Alternative**: Run your own PDS or use `did:web` (advanced).

### Step 2: Prove Domain Ownership

To register with Coves, you must prove you own a domain by serving your DID at `https://yourdomain.com/.well-known/atproto-did`.

**Create the file**:

```bash
mkdir -p .well-known
echo "did:plc:abc123..." > .well-known/atproto-did
```

**Upload to your web server** so it's accessible at:
```
https://rss-bot.example.com/.well-known/atproto-did
```

**Verify it works**:
```bash
curl https://rss-bot.example.com/.well-known/atproto-did
# Should return: did:plc:abc123...
```

**Nginx configuration example**:
```nginx
location /.well-known/atproto-did {
    alias /var/www/.well-known/atproto-did;
    default_type text/plain;
    add_header Access-Control-Allow-Origin *;
}
```

### Step 3: Register with Coves

Call the registration endpoint to register your aggregator DID with the Coves instance.

**Endpoint**: `POST /xrpc/social.coves.aggregator.register`

**Request**:
```bash
curl -X POST https://api.coves.social/xrpc/social.coves.aggregator.register \
  -H "Content-Type: application/json" \
  -d '{
    "did": "did:plc:abc123...",
    "domain": "rss-bot.example.com"
  }'
```

**Response** (Success):
```json
{
  "did": "did:plc:abc123...",
  "handle": "mynewsbot.bsky.social",
  "message": "Aggregator registered successfully. Next step: create a service declaration record at at://did:plc:abc123.../social.coves.aggregator.service/self"
}
```

**What happens**:
1. Coves fetches `https://rss-bot.example.com/.well-known/atproto-did`
2. Verifies it contains your DID
3. Resolves your DID to get handle and PDS URL
4. Inserts you into the `users` table

**You're now registered!** But you need to create a service declaration next.

### Step 4: Create Service Declaration

Write a `social.coves.aggregator.service` record to your repository. This contains metadata about your aggregator and gets indexed by Coves' Jetstream consumer.

**Endpoint**: `POST https://your-pds.com/xrpc/com.atproto.repo.createRecord`

**Request**:
```bash
curl -X POST https://bsky.social/xrpc/com.atproto.repo.createRecord \
  -H "Authorization: Bearer YOUR_ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "did:plc:abc123...",
    "collection": "social.coves.aggregator.service",
    "rkey": "self",
    "record": {
      "$type": "social.coves.aggregator.service",
      "did": "did:plc:abc123...",
      "displayName": "RSS News Aggregator",
      "description": "Aggregates tech news from various RSS feeds",
      "sourceUrl": "https://github.com/yourname/rss-aggregator",
      "maintainer": "did:plc:your-personal-did",
      "createdAt": "2024-01-15T12:00:00Z"
    }
  }'
```

**Response**:
```json
{
  "uri": "at://did:plc:abc123.../social.coves.aggregator.service/self",
  "cid": "bafyrei..."
}
```

**Optional fields**:
- `avatar`: Blob reference to avatar image
- `configSchema`: JSON Schema for community-specific configuration

**Wait 5-10 seconds** for Jetstream to index your service declaration into the `aggregators` table.

## Authorization Process

Before you can post to a community, a moderator must authorize your aggregator.

### How Authorization Works

1. **Moderator decision**: Community moderator evaluates your aggregator
2. **Authorization record**: Moderator writes `social.coves.aggregator.authorization` to community's repo
3. **Jetstream indexing**: Record gets indexed into `aggregator_authorizations` table
4. **Posting enabled**: You can now post to that community

### Authorization Record Structure

**Location**: `at://{community_did}/social.coves.aggregator.authorization/{rkey}`

**Example**:
```json
{
  "$type": "social.coves.aggregator.authorization",
  "aggregatorDid": "did:plc:abc123...",
  "communityDid": "did:plc:community123...",
  "enabled": true,
  "createdBy": "did:plc:moderator...",
  "createdAt": "2024-01-15T12:00:00Z",
  "config": {
    "maxPostsPerHour": 5,
    "allowedCategories": ["tech", "news"]
  }
}
```

### Checking Your Authorizations

**Endpoint**: `GET /xrpc/social.coves.aggregator.getAuthorizations`

```bash
curl "https://api.coves.social/xrpc/social.coves.aggregator.getAuthorizations?aggregatorDid=did:plc:abc123...&enabledOnly=true"
```

**Response**:
```json
{
  "authorizations": [
    {
      "aggregatorDid": "did:plc:abc123...",
      "communityDid": "did:plc:community123...",
      "communityHandle": "~tech@coves.social",
      "enabled": true,
      "createdAt": "2024-01-15T12:00:00Z",
      "config": {...}
    }
  ]
}
```

## Posting to Communities

Once authorized, you can post to communities using the standard post creation endpoint.

### Create Post

**Endpoint**: `POST /xrpc/social.coves.community.post.create`

**Request**:
```bash
curl -X POST https://api.coves.social/xrpc/social.coves.community.post.create \
  -H "Authorization: Bearer YOUR_ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "communityDid": "did:plc:community123...",
    "post": {
      "text": "New blog post: Understanding atProto Identity\nhttps://example.com/post",
      "createdAt": "2024-01-15T12:00:00Z",
      "facets": [
        {
          "index": { "byteStart": 50, "byteEnd": 75 },
          "features": [
            {
              "$type": "social.coves.richtext.facet#link",
              "uri": "https://example.com/post"
            }
          ]
        }
      ]
    }
  }'
```

**Response**:
```json
{
  "uri": "at://did:plc:abc123.../social.coves.community.post/3k...",
  "cid": "bafyrei..."
}
```

### Post Validation

The handler validates:
1. **Authentication**: Valid JWT token
2. **Author exists**: DID in `users` table
3. **Is aggregator**: DID in `aggregators` table
4. **Authorization**: Active authorization for (aggregator, community)
5. **Rate limit**: Less than 10 posts/hour to this community
6. **Content**: Valid post structure per lexicon

### Rate Limits

**Per-community rate limit**: 10 posts per hour

This is tracked in the `aggregator_posts` table and enforced at the handler level.

**Why?**: Prevents spam while allowing useful bot activity.

**Best practices**:
- Batch similar content
- Post only high-quality content
- Respect community guidelines
- Monitor your posting rate

## Security Best Practices

### Credential Management

✅ **DO**:
- Store credentials in environment variables or secret management
- Use HTTPS for all API calls
- Rotate access tokens regularly (use refresh tokens)
- Keep `aggregator-config.env` out of version control

❌ **DON'T**:
- Hardcode credentials in source code
- Commit credentials to Git
- Share access tokens publicly
- Reuse personal credentials for bots

### Domain Security

✅ **DO**:
- Use HTTPS for `.well-known` endpoint
- Keep domain under your control
- Monitor for unauthorized changes
- Use DNSSEC if possible

❌ **DON'T**:
- Use HTTP (will fail verification)
- Use shared/untrusted hosting
- Allow others to modify `.well-known` files
- Use expired SSL certificates

### Content Security

✅ **DO**:
- Validate all external content before posting
- Sanitize URLs and text
- Rate-limit your own posting
- Implement circuit breakers for failures

❌ **DON'T**:
- Post unvalidated user input
- Include malicious links
- Spam communities
- Bypass rate limits

## Troubleshooting

### Registration Errors

#### Error: "DomainVerificationFailed"

**Cause**: `.well-known/atproto-did` not accessible or contains wrong DID

**Solutions**:
1. Verify file is accessible: `curl https://yourdomain.com/.well-known/atproto-did`
2. Check content matches your DID exactly (no extra whitespace)
3. Ensure HTTPS is working (not HTTP)
4. Check web server logs for access errors
5. Verify firewall rules allow HTTPS traffic

#### Error: "AlreadyRegistered"

**Cause**: This DID is already registered with this Coves instance

**Solutions**:
- This is safe to ignore if you're re-running setup
- If you need to update info, just create a new service declaration
- Contact instance admin if you need to remove registration

#### Error: "DIDResolutionFailed"

**Cause**: Could not resolve DID document from PLC directory

**Solutions**:
1. Verify DID exists: `curl https://plc.directory/{your-did}`
2. Wait 30 seconds and retry (PLC propagation delay)
3. Check PDS is accessible
4. Verify DID format is correct (must start with `did:plc:` or `did:web:`)

### Posting Errors

#### Error: "NotAuthorized"

**Cause**: No active authorization for this (aggregator, community) pair

**Solutions**:
1. Check authorizations: `GET /xrpc/social.coves.aggregator.getAuthorizations`
2. Contact community moderator to request authorization
3. Verify authorization wasn't disabled
4. Wait for Jetstream to index authorization (5-10 seconds)

#### Error: "RateLimitExceeded"

**Cause**: Exceeded 10 posts/hour to this community

**Solutions**:
1. Wait for the rate limit window to reset
2. Batch posts to stay under limit
3. Distribute posts across multiple communities
4. Implement posting queue in your aggregator

### Service Declaration Not Appearing

**Symptoms**: Service declaration created but not in `aggregators` table

**Solutions**:
1. Wait 5-10 seconds for Jetstream to index
2. Check Jetstream consumer logs for errors
3. Verify record was created: Check PDS at `at://your-did/social.coves.aggregator.service/self`
4. Verify `$type` field is exactly `"social.coves.aggregator.service"`
5. Check `displayName` is not empty (required field)

## API Reference

### Registration Endpoint

**`POST /xrpc/social.coves.aggregator.register`**

**Input**:
```typescript
{
  did: string      // DID of aggregator (did:plc or did:web)
  domain: string   // Domain serving .well-known/atproto-did
}
```

**Output**:
```typescript
{
  did: string      // Registered DID
  handle: string   // Handle from DID document
  message: string  // Next steps message
}
```

**Errors**:
- `InvalidDID`: DID format invalid
- `DomainVerificationFailed`: .well-known verification failed
- `AlreadyRegistered`: DID already registered
- `DIDResolutionFailed`: Could not resolve DID

### Query Endpoints

**`GET /xrpc/social.coves.aggregator.getServices`**

Get aggregator service details.

**Parameters**:
- `dids`: Array of DIDs (comma-separated)

**`GET /xrpc/social.coves.aggregator.getAuthorizations`**

List communities that authorized an aggregator.

**Parameters**:
- `aggregatorDid`: Aggregator DID
- `enabledOnly`: Filter to enabled only (default: false)

**`GET /xrpc/social.coves.aggregator.listForCommunity`**

List aggregators authorized by a community.

**Parameters**:
- `communityDid`: Community DID
- `enabledOnly`: Filter to enabled only (default: false)

## Further Reading

- [Aggregator PRD](PRD_AGGREGATORS.md) - Architecture and design decisions
- [atProto Guide](../../ATPROTO_GUIDE.md) - atProto fundamentals
- [Communities PRD](../PRD_COMMUNITIES.md) - Community system overview
- [Setup Scripts README](../../scripts/aggregator-setup/README.md) - Script documentation

## Support

For issues or questions:

1. Check this guide's troubleshooting section
2. Review the PRD and architecture docs
3. Check Coves GitHub issues
4. Ask in Coves developer community
