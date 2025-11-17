# Federation PRD: Cross-Instance Posting (Beta)

**Status:** Planning - Beta
**Target:** Beta Release
**Owner:** TBD
**Last Updated:** 2025-11-16

---

## Overview

Enable Lemmy-style federation where users on any Coves instance can post to communities hosted on other instances, while maintaining community ownership and moderation control.

### Problem Statement

**Current (Alpha):**
- Posts to communities require community credentials
- Users can only post to communities on their home instance
- No true federation across instances

**Desired (Beta):**
- User A@coves.social can post to !gaming@covesinstance.com
- Communities maintain full moderation control
- Content lives in community repositories (not user repos)
- Seamless UX - users don't think about federation

---

## Goals

### Primary Goals
1. **Enable cross-instance posting** - Users can post to any community on any federated instance
2. **Preserve community ownership** - Posts live in community repos, not user repos
3. **atProto-native implementation** - Use `com.atproto.server.getServiceAuth` pattern
4. **Maintain security** - No compromise on auth, validation, or moderation

### Non-Goals (Future Versions)
- Automatic instance discovery (Beta: manual allowlist)
- Cross-instance moderation delegation
- Content mirroring/replication
- User migration between instances

---

## Technical Approach

### Architecture: atProto Service Auth

Use atProto's native service authentication delegation pattern:

```
┌─────────────┐         ┌──────────────────┐         ┌─────────────┐
│  User A     │         │  coves.social    │         │ covesinstance│
│ @coves.soc  │────────▶│   AppView        │────────▶│  .com PDS   │
└─────────────┘  (1)    └──────────────────┘  (2)    └─────────────┘
                JWT auth    Request Service Auth         Validate
                                     │                       │
                                     │◀──────────────────────┘
                                     │        (3) Scoped Token
                                     │
                                     ▼
                            ┌──────────────────┐
                            │ covesinstance    │
                            │  .com PDS        │
                            │ Write Post       │
                            └──────────────────┘
                                     │
                                     ▼
                            ┌──────────────────┐
                            │   Firehose       │
                            │  (broadcasts)    │
                            └──────────────────┘
                                     │
                        ┌────────────┴────────────┐
                        ▼                         ▼
                ┌──────────────┐        ┌──────────────┐
                │ coves.social │        │covesinstance │
                │   AppView    │        │  .com AppView│
                │  (indexes)   │        │   (indexes)  │
                └──────────────┘        └──────────────┘
```

### Flow Breakdown

**Step 1: User Authentication (Unchanged)**
- User authenticates with their home instance (coves.social)
- Receives JWT token for API requests

**Step 2: Service Auth Request (New)**
- When posting to remote community, AppView requests service auth token
- Endpoint: `POST {remote-pds}/xrpc/com.atproto.server.getServiceAuth`
- Payload:
  ```json
  {
    "aud": "did:plc:community123",  // Community DID
    "exp": 1234567890,               // Token expiration
    "lxm": "social.coves.community.post.create"  // Authorized method
  }
  ```

**Step 3: Service Auth Validation (New - PDS Side)**
- Remote PDS validates request:
  - Is requesting service trusted? (instance allowlist)
  - Is user banned from community?
  - Does community allow remote posts?
  - Rate limiting checks
- Returns scoped token valid for specific community + operation

**Step 4: Post Creation (Modified)**
- AppView uses service auth token to write to remote PDS
- Same `com.atproto.repo.createRecord` endpoint as current implementation
- Post record written to community's repository

**Step 5: Indexing (Unchanged)**
- PDS broadcasts to firehose
- All AppViews index via Jetstream consumers

---

## Implementation Details

### Phase 1: Service Detection (Local vs Remote)

**File:** `internal/core/posts/service.go`

```go
func (s *postService) CreatePost(ctx context.Context, req CreatePostRequest) (*CreatePostResponse, error) {
    // ... existing validation ...

    community, err := s.communityService.GetByDID(ctx, communityDID)
    if err != nil {
        return nil, err
    }

    // NEW: Route based on community location
    if s.isLocalCommunity(community) {
        return s.createLocalPost(ctx, community, req)
    }
    return s.createFederatedPost(ctx, community, req)
}

func (s *postService) isLocalCommunity(community *communities.Community) bool {
    localPDSHost := extractHost(s.pdsURL)
    communityPDSHost := extractHost(community.PDSURL)
    return localPDSHost == communityPDSHost
}
```

### Phase 2: Service Auth Client

**New File:** `internal/atproto/service_auth/client.go`

```go
type ServiceAuthClient interface {
    // RequestServiceAuth obtains a scoped token for writing to remote community
    RequestServiceAuth(ctx context.Context, opts ServiceAuthOptions) (*ServiceAuthToken, error)
}

type ServiceAuthOptions struct {
    RemotePDSURL  string    // Remote PDS endpoint
    CommunityDID  string    // Target community DID
    UserDID       string    // Author DID (for validation)
    Method        string    // "social.coves.community.post.create"
    ExpiresIn     int       // Token lifetime (seconds)
}

type ServiceAuthToken struct {
    Token     string    // JWT token for auth
    ExpiresAt time.Time // When token expires
}

func (c *serviceAuthClient) RequestServiceAuth(ctx context.Context, opts ServiceAuthOptions) (*ServiceAuthToken, error) {
    endpoint := fmt.Sprintf("%s/xrpc/com.atproto.server.getServiceAuth", opts.RemotePDSURL)

    payload := map[string]interface{}{
        "aud": opts.CommunityDID,
        "exp": time.Now().Add(time.Duration(opts.ExpiresIn) * time.Second).Unix(),
        "lxm": opts.Method,
    }

    // Sign request with our instance DID credentials
    signedReq, err := c.signRequest(payload)
    if err != nil {
        return nil, fmt.Errorf("failed to sign service auth request: %w", err)
    }

    resp, err := c.httpClient.Post(endpoint, signedReq)
    if err != nil {
        return nil, fmt.Errorf("service auth request failed: %w", err)
    }

    return parseServiceAuthResponse(resp)
}
```

### Phase 3: Federated Post Creation

**File:** `internal/core/posts/service.go`

```go
func (s *postService) createFederatedPost(ctx context.Context, community *communities.Community, req CreatePostRequest) (*CreatePostResponse, error) {
    // 1. Request service auth token from remote PDS
    token, err := s.serviceAuthClient.RequestServiceAuth(ctx, service_auth.ServiceAuthOptions{
        RemotePDSURL: community.PDSURL,
        CommunityDID: community.DID,
        UserDID:      req.AuthorDID,
        Method:       "social.coves.community.post.create",
        ExpiresIn:    300, // 5 minutes
    })
    if err != nil {
        // Handle specific errors
        if isUnauthorized(err) {
            return nil, ErrNotAuthorizedRemote
        }
        if isBanned(err) {
            return nil, ErrBannedRemote
        }
        return nil, fmt.Errorf("failed to obtain service auth: %w", err)
    }

    // 2. Build post record (same as local)
    postRecord := PostRecord{
        Type:      "social.coves.community.post",
        Community: community.DID,
        Author:    req.AuthorDID,
        Title:     req.Title,
        Content:   req.Content,
        // ... other fields ...
        CreatedAt: time.Now().UTC().Format(time.RFC3339),
    }

    // 3. Write to remote PDS using service auth token
    uri, cid, err := s.createPostOnRemotePDS(ctx, community.PDSURL, community.DID, postRecord, token.Token)
    if err != nil {
        return nil, fmt.Errorf("failed to write to remote PDS: %w", err)
    }

    log.Printf("[FEDERATION] User %s posted to remote community %s: %s",
        req.AuthorDID, community.DID, uri)

    return &CreatePostResponse{
        URI: uri,
        CID: cid,
    }, nil
}

func (s *postService) createPostOnRemotePDS(
    ctx context.Context,
    pdsURL string,
    communityDID string,
    record PostRecord,
    serviceAuthToken string,
) (uri, cid string, err error) {
    endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", pdsURL)

    payload := map[string]interface{}{
        "repo":       communityDID,
        "collection": "social.coves.community.post",
        "record":     record,
    }

    jsonData, _ := json.Marshal(payload)
    req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))

    // Use service auth token instead of community credentials
    req.Header.Set("Authorization", "Bearer "+serviceAuthToken)
    req.Header.Set("Content-Type", "application/json")

    // ... execute request, parse response ...
    return uri, cid, nil
}
```

### Phase 4: PDS Service Auth Validation (PDS Extension)

**Note:** This requires extending the PDS. Options:
1. Contribute to official atproto PDS
2. Run modified PDS fork
3. Use PDS middleware/proxy

**Conceptual Implementation:**

```go
// PDS validates service auth requests before issuing tokens
func (h *ServiceAuthHandler) HandleGetServiceAuth(w http.ResponseWriter, r *http.Request) {
    var req ServiceAuthRequest
    json.NewDecoder(r.Body).Decode(&req)

    // 1. Verify requesting service is trusted
    requestingDID := extractDIDFromJWT(r.Header.Get("Authorization"))
    if !h.isTrustedInstance(requestingDID) {
        writeError(w, http.StatusForbidden, "UntrustedInstance", "Instance not in allowlist")
        return
    }

    // 2. Validate community exists on this PDS
    community, err := h.getCommunityByDID(req.Aud)
    if err != nil {
        writeError(w, http.StatusNotFound, "CommunityNotFound", "Community not hosted here")
        return
    }

    // 3. Check user not banned (query from AppView or local moderation records)
    if h.isUserBanned(req.UserDID, req.Aud) {
        writeError(w, http.StatusForbidden, "Banned", "User banned from community")
        return
    }

    // 4. Check community settings (allows remote posts?)
    if !community.AllowFederatedPosts {
        writeError(w, http.StatusForbidden, "FederationDisabled", "Community doesn't accept federated posts")
        return
    }

    // 5. Rate limiting (per user, per community, per instance)
    if h.exceedsRateLimit(req.UserDID, req.Aud, requestingDID) {
        writeError(w, http.StatusTooManyRequests, "RateLimited", "Too many requests")
        return
    }

    // 6. Generate scoped token
    token := h.issueServiceAuthToken(ServiceAuthTokenOptions{
        Audience:    req.Aud,           // Community DID
        Subject:     requestingDID,     // Requesting instance DID
        Method:      req.Lxm,           // Authorized method
        ExpiresAt:   time.Unix(req.Exp, 0),
        Scopes:      []string{"write:posts"},
    })

    json.NewEncoder(w).Encode(map[string]string{
        "token": token,
    })
}
```

---

## Database Schema Changes

### New Table: `instance_federation`

Tracks trusted instances and federation settings:

```sql
CREATE TABLE instance_federation (
    id SERIAL PRIMARY KEY,
    instance_did TEXT NOT NULL UNIQUE,
    instance_domain TEXT NOT NULL,
    trust_level TEXT NOT NULL,  -- 'trusted', 'limited', 'blocked'
    allowed_methods TEXT[] NOT NULL DEFAULT '{}',
    rate_limit_posts_per_hour INTEGER NOT NULL DEFAULT 100,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes TEXT
);

CREATE INDEX idx_instance_federation_did ON instance_federation(instance_did);
CREATE INDEX idx_instance_federation_trust ON instance_federation(trust_level);
```

### New Table: `federation_rate_limits`

Track federated post rate limits:

```sql
CREATE TABLE federation_rate_limits (
    id SERIAL PRIMARY KEY,
    user_did TEXT NOT NULL,
    community_did TEXT NOT NULL,
    instance_did TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    post_count INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE(user_did, community_did, instance_did, window_start)
);

CREATE INDEX idx_federation_rate_limits_lookup
    ON federation_rate_limits(user_did, community_did, instance_did, window_start);
```

### Update Table: `communities`

Add federation settings:

```sql
ALTER TABLE communities
ADD COLUMN allow_federated_posts BOOLEAN NOT NULL DEFAULT true,
ADD COLUMN federation_mode TEXT NOT NULL DEFAULT 'open';
-- federation_mode: 'open' (any instance), 'allowlist' (trusted only), 'local' (no federation)
```

---

## Security Considerations

### 1. Instance Trust Model

**Allowlist Approach (Beta):**
- Manual approval of federated instances
- Admin UI to manage instance trust levels
- Default: block all, explicit allow

**Trust Levels:**
- `trusted` - Full federation, normal rate limits
- `limited` - Federation allowed, strict rate limits
- `blocked` - No federation

### 2. User Ban Synchronization

**Challenge:** Remote instance needs to check local bans

**Options:**
1. **Service auth validation** - PDS queries AppView for ban status
2. **Ban records in PDS** - Moderation records stored in community repo
3. **Cached ban list** - Remote instances cache ban lists (with TTL)

**Beta Approach:** Option 1 (service auth validation queries AppView)

### 3. Rate Limiting

**Multi-level rate limits:**
- Per user per community: 10 posts/hour
- Per instance per community: 100 posts/hour
- Per user across all communities: 50 posts/hour

**Implementation:** In-memory + PostgreSQL for persistence

### 4. Content Validation

**Same validation as local posts:**
- Lexicon validation
- Content length limits
- Embed validation
- Label validation

**Additional federation checks:**
- Verify author DID is valid
- Verify requesting instance signature
- Verify token scopes match operation

---

## API Changes

### New Endpoint: `social.coves.federation.getTrustedInstances`

**Purpose:** List instances this instance federates with

**Lexicon:**
```json
{
  "lexicon": 1,
  "id": "social.coves.federation.getTrustedInstances",
  "defs": {
    "main": {
      "type": "query",
      "output": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": ["instances"],
          "properties": {
            "instances": {
              "type": "array",
              "items": { "$ref": "#instanceView" }
            }
          }
        }
      }
    },
    "instanceView": {
      "type": "object",
      "required": ["did", "domain", "trustLevel"],
      "properties": {
        "did": { "type": "string" },
        "domain": { "type": "string" },
        "trustLevel": { "type": "string" },
        "allowedMethods": { "type": "array", "items": { "type": "string" } }
      }
    }
  }
}
```

### Modified Endpoint: `social.coves.community.post.create`

**Changes:**
- No API contract changes
- Internal routing: local vs federated
- New error codes:
  - `FederationFailed` - Remote instance unreachable
  - `RemoteNotAuthorized` - Remote instance rejected auth
  - `RemoteBanned` - User banned on remote community

---

## User Experience

### Happy Path: Cross-Instance Post

1. User on coves.social navigates to !gaming@covesinstance.com
2. Clicks "Create Post"
3. Fills out post form (title, content, etc.)
4. Clicks "Submit"
5. **Behind the scenes:**
   - coves.social requests service auth from covesinstance.com
   - covesinstance.com validates and issues token
   - coves.social writes post using token
   - Post appears in feed within seconds (via firehose)
6. **User sees:** Post published successfully
7. Post appears in:
   - covesinstance.com feeds (native community)
   - coves.social discover/all feeds (indexed via firehose)
   - User's profile on coves.social

### Error Cases

**User Banned:**
- Error: "You are banned from !gaming@covesinstance.com"
- Suggestion: "Contact community moderators for more information"

**Instance Blocked:**
- Error: "This community does not accept posts from your instance"
- Suggestion: "Contact community administrators or create a local account"

**Federation Unavailable:**
- Error: "Unable to connect to covesinstance.com. Try again later."
- Fallback: Allow saving as draft (future feature)

**Rate Limited:**
- Error: "You're posting too quickly. Please wait before posting again."
- Show: Countdown until next post allowed

---

## Testing Requirements

### Unit Tests

1. **Service Detection:**
   - `isLocalCommunity()` correctly identifies local vs remote
   - Handles edge cases (different ports, subdomains)

2. **Service Auth Client:**
   - Correctly formats service auth requests
   - Handles token expiration
   - Retries on transient failures

3. **Federated Post Creation:**
   - Uses service auth token instead of community credentials
   - Falls back gracefully on errors
   - Logs federation events

### Integration Tests

1. **Local Post (Regression):**
   - Posting to local community still works
   - No performance degradation

2. **Federated Post:**
   - User can post to remote community
   - Service auth token requested correctly
   - Post written to remote PDS
   - Post indexed by both AppViews

3. **Authorization Failures:**
   - Banned users rejected at service auth stage
   - Untrusted instances rejected
   - Expired tokens rejected

4. **Rate Limiting:**
   - Per-user rate limits enforced
   - Per-instance rate limits enforced
   - Rate limit resets correctly

### End-to-End Tests

1. **Cross-Instance User Journey:**
   - Set up two instances (instance-a, instance-b)
   - Create community on instance-b
   - User on instance-a posts to instance-b community
   - Verify post appears on both instances

2. **Moderation Enforcement:**
   - Ban user on remote instance
   - Verify user can't post from any instance
   - Unban user
   - Verify user can post again

3. **Instance Blocklist:**
   - Block instance-a on instance-b
   - Verify users from instance-a can't post to instance-b communities
   - Unblock instance-a
   - Verify posting works again

---

## Migration Path (Alpha → Beta)

### Phase 1: Backend Implementation (No User Impact)
1. Add service auth client
2. Add local vs remote detection
3. Deploy with feature flag `ENABLE_FEDERATION=false`

### Phase 2: Database Migration
1. Add federation tables
2. Seed with initial trusted instances (manual)
3. Add community federation flags (default: allow)

### Phase 3: Soft Launch
1. Enable federation for single test instance
2. Monitor service auth requests/errors
3. Validate rate limiting works

### Phase 4: Beta Rollout
1. Enable `ENABLE_FEDERATION=true` for all instances
2. Admin UI for managing trusted instances
3. Community settings for federation preferences

### Phase 5: Documentation & Onboarding
1. Instance operator guide: "How to federate with other instances"
2. Community moderator guide: "Federation settings"
3. User guide: "Posting across instances"

---

## Metrics & Success Criteria

### Performance Metrics
- Service auth request latency: p95 < 200ms
- Federated post creation time: p95 < 2 seconds (vs 500ms local)
- Service auth token cache hit rate: > 80%

### Adoption Metrics
- % of posts that are federated: Target 20% by end of Beta
- Number of federated instances: Target 5+ by end of Beta
- Cross-instance engagement (comments, votes): Monitor trend

### Reliability Metrics
- Service auth success rate: > 99%
- Federated post success rate: > 95%
- Service auth token validation errors: < 1%

### Security Metrics
- Unauthorized access attempts: Monitor & alert
- Rate limit triggers: Track per instance
- Ban evasion attempts: Zero tolerance

---

## Rollback Plan

If federation causes critical issues:

1. **Immediate:** Set `ENABLE_FEDERATION=false` via env var
2. **Fallback:** All posts route through local-only flow
3. **Investigation:** Review logs for service auth failures
4. **Fix Forward:** Deploy patch, re-enable gradually

**No data loss:** Posts are written to PDS, indexed via firehose regardless of federation method.

---

## Open Questions

1. **Instance Discovery:** How do users find communities on other instances?
   - Beta: Manual (users share links)
   - Future: Instance directory, community search across instances

2. **Service Auth Token Caching:** Should AppViews cache service auth tokens?
   - Pros: Reduce latency, fewer PDS requests
   - Cons: Stale permissions, ban enforcement delay
   - **Decision needed:** Cache with short TTL (5 minutes)?

3. **PDS Implementation:** Who implements service auth validation?
   - Option A: Contribute to official PDS (long timeline)
   - Option B: Run forked PDS (maintenance burden)
   - Option C: Proxy/middleware (added complexity)
   - **Decision needed:** Start with Option B, migrate to Option A?

4. **Federation Symmetry:** If instance-a trusts instance-b, does instance-b auto-trust instance-a?
   - Beta: No (asymmetric trust)
   - Future: Mutual federation agreements?

5. **Cross-Instance Moderation:** Should bans propagate across instances?
   - Beta: No (each instance decides)
   - Future: Shared moderation lists?

---

## Future Enhancements (Post-Beta)

1. **Service Auth Token Caching:** Reduce latency for frequent posters
2. **Batch Service Auth:** Request tokens for multiple communities at once
3. **Instance Discovery API:** Automatic instance detection/registration
4. **Federation Analytics:** Dashboard showing cross-instance activity
5. **Moderation Sync:** Optional shared ban lists across trusted instances
6. **Content Mirroring:** Cache federated posts locally for performance
7. **User Migration:** Transfer account between instances

---

## Resources

### Documentation
- [atProto Service Auth Spec](https://atproto.com/specs/service-auth) (hypothetical - check actual docs)
- Lemmy Federation Architecture
- Mastodon Federation Implementation

### Code References
- `internal/core/posts/service.go` - Post creation service
- `internal/api/handlers/post/create.go` - Post creation handler
- `internal/atproto/jetstream/` - Firehose consumers

### Dependencies
- atproto SDK (for service auth)
- PDS v0.4+ (service auth support)
- PostgreSQL 14+ (for federation tables)

---

## Appendix A: Service Auth Request Example

**Request to Remote PDS:**
```http
POST https://covesinstance.com/xrpc/com.atproto.server.getServiceAuth
Authorization: Bearer {coves-social-instance-jwt}
Content-Type: application/json

{
  "aud": "did:plc:community123",
  "exp": 1700000000,
  "lxm": "social.coves.community.post.create"
}
```

**Response:**
```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "token": "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

**Using Token to Create Post:**
```http
POST https://covesinstance.com/xrpc/com.atproto.repo.createRecord
Authorization: Bearer {service-auth-token}
Content-Type: application/json

{
  "repo": "did:plc:community123",
  "collection": "social.coves.community.post",
  "record": {
    "$type": "social.coves.community.post",
    "community": "did:plc:community123",
    "author": "did:plc:user456",
    "title": "Hello from coves.social!",
    "content": "This is a federated post",
    "createdAt": "2024-11-16T12:00:00Z"
  }
}
```

---

## Appendix B: Error Handling Matrix

| Error Condition | HTTP Status | Error Code | User Message | Retry Strategy |
|----------------|-------------|------------|--------------|----------------|
| Instance not trusted | 403 | `UntrustedInstance` | "This community doesn't accept posts from your instance" | No retry |
| User banned | 403 | `Banned` | "You are banned from this community" | No retry |
| Rate limit exceeded | 429 | `RateLimited` | "Too many posts. Try again in X minutes" | Exponential backoff |
| PDS unreachable | 503 | `ServiceUnavailable` | "Community temporarily unavailable" | Retry 3x with backoff |
| Invalid token | 401 | `InvalidToken` | "Session expired. Please try again" | Refresh token & retry |
| Community not found | 404 | `CommunityNotFound` | "Community not found" | No retry |
| Service auth failed | 500 | `FederationFailed` | "Unable to connect. Try again later" | Retry 2x |

---

**End of PRD**
