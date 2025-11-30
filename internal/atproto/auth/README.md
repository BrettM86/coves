# atProto OAuth Authentication

This package implements third-party OAuth authentication for Coves, validating DPoP-bound access tokens from mobile apps and other atProto clients.

## Architecture

This is **third-party authentication** (validating incoming requests), not first-party authentication (logging users into Coves web frontend).

### Components

1. **JWT Parser** (`jwt.go`) - Parses and validates JWT tokens
2. **JWKS Fetcher** (`jwks_fetcher.go`) - Fetches and caches public keys from PDS authorization servers
3. **Auth Middleware** (`internal/api/middleware/auth.go`) - HTTP middleware that protects endpoints

### Flow

```
Client Request
    ↓
Authorization: DPoP <access_token>
DPoP: <proof-jwt>
    ↓
Auth Middleware
    ↓
Extract JWT → Parse Claims → Verify Signature (via JWKS) → Verify DPoP Proof
    ↓
Inject DID into Context → Call Handler
```

## Usage

### Phase 1: Parse-Only Mode (Testing)

Set `AUTH_SKIP_VERIFY=true` to only parse JWTs without signature verification:

```bash
export AUTH_SKIP_VERIFY=true
```

This is useful for:
- Initial integration testing
- Testing with mock tokens
- Debugging JWT structure

### Phase 2: Full Verification (Production)

Set `AUTH_SKIP_VERIFY=false` (or unset) to enable full JWT signature verification:

```bash
export AUTH_SKIP_VERIFY=false
# or just unset it
```

This is **required for production** and validates:
- JWT signature using PDS public key
- Token expiration
- Required claims (sub, iss)
- DID format

## Protected Endpoints

The following endpoints require authentication:

- `POST /xrpc/social.coves.community.create`
- `POST /xrpc/social.coves.community.update`
- `POST /xrpc/social.coves.community.subscribe`
- `POST /xrpc/social.coves.community.unsubscribe`

### Making Authenticated Requests

Include the JWT in the `Authorization` header:

```bash
curl -X POST https://coves.social/xrpc/social.coves.community.create \
  -H "Authorization: DPoP eyJhbGc..." \
  -H "DPoP: eyJhbGc..." \
  -H "Content-Type: application/json" \
  -d '{"name":"Gaming","hostedByDid":"did:plc:..."}'
```

### Getting User DID in Handlers

The middleware injects the authenticated user's DID into the request context:

```go
import "Coves/internal/api/middleware"

func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
    // Extract authenticated user DID
    userDID := middleware.GetUserDID(r)
    if userDID == "" {
        // Not authenticated (should never happen with RequireAuth middleware)
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    // Use userDID for authorization checks
    // ...
}
```

## Key Caching

Public keys are fetched from PDS authorization servers and cached for 1 hour. The cache is automatically cleaned up hourly to remove expired entries.

### JWKS Discovery Flow

1. Extract `iss` claim from JWT (e.g., `https://pds.example.com`)
2. Fetch `https://pds.example.com/.well-known/oauth-authorization-server`
3. Extract `jwks_uri` from metadata
4. Fetch JWKS from `jwks_uri`
5. Find matching key by `kid` from JWT header
6. Cache the JWKS for 1 hour

## DPoP Token Binding

DPoP (Demonstrating Proof-of-Possession) binds access tokens to client-controlled cryptographic keys, preventing token theft and replay attacks.

### What is DPoP?

DPoP is an OAuth extension (RFC 9449) that adds proof-of-possession semantics to bearer tokens. When a PDS issues a DPoP-bound access token:

1. Access token contains `cnf.jkt` claim (JWK thumbprint of client's public key)
2. Client creates a DPoP proof JWT signed with their private key
3. Server verifies the proof signature and checks it matches the token's `cnf.jkt`

### CRITICAL: DPoP Security Model

> ⚠️ **DPoP is an ADDITIONAL security layer, NOT a replacement for token signature verification.**

The correct verification order is:
1. **ALWAYS verify the access token signature first** (via JWKS, HS256 shared secret, or DID resolution)
2. **If the verified token has `cnf.jkt`, REQUIRE valid DPoP proof**
3. **NEVER use DPoP as a fallback when signature verification fails**

**Why This Matters**: An attacker could create a fake token with `sub: "did:plc:victim"` and their own `cnf.jkt`, then present a valid DPoP proof signed with their key. If we accept DPoP as a fallback, the attacker can impersonate any user.

### How DPoP Works

```
┌─────────────┐                          ┌─────────────┐
│   Client    │                          │   Server    │
│             │                          │  (Coves)    │
└─────────────┘                          └─────────────┘
       │                                        │
       │ 1. Authorization: DPoP <token>         │
       │    DPoP: <proof-jwt>                  │
       │───────────────────────────────────────>│
       │                                        │
       │                                        │ 2. VERIFY token signature
       │                                        │    (REQUIRED - no fallback!)
       │                                        │
       │                                        │ 3. If token has cnf.jkt:
       │                                        │    - Verify DPoP proof
       │                                        │    - Check thumbprint match
       │                                        │
       │                              200 OK    │
       │<───────────────────────────────────────│
```

### When DPoP is Required

DPoP verification is **REQUIRED** when:
- Access token signature has been verified AND
- Access token contains `cnf.jkt` claim (DPoP-bound)

If the token has `cnf.jkt` but no DPoP header is present, the request is **REJECTED**.

### Replay Protection

DPoP proofs include a unique `jti` (JWT ID) claim. The server tracks seen `jti` values to prevent replay attacks:

```go
// Create a verifier with replay protection (default)
verifier := auth.NewDPoPVerifier()
defer verifier.Stop() // Stop cleanup goroutine on shutdown

// The verifier automatically rejects reused jti values within the proof validity window (5 minutes)
```

### DPoP Implementation

The `dpop.go` module provides:

```go
// Create a verifier with replay protection
verifier := auth.NewDPoPVerifier()
defer verifier.Stop()

// Verify the DPoP proof
proof, err := verifier.VerifyDPoPProof(dpopHeader, "POST", "https://coves.social/xrpc/...")
if err != nil {
    // Invalid proof (includes replay detection)
}

// Verify it binds to the VERIFIED access token
expectedThumbprint, err := auth.ExtractCnfJkt(claims)
if err != nil {
    // Token not DPoP-bound
}

if err := verifier.VerifyTokenBinding(proof, expectedThumbprint); err != nil {
    // Proof doesn't match token
}
```

### DPoP Proof Format

The DPoP header contains a JWT with:

**Header**:
- `typ`: `"dpop+jwt"` (required)
- `alg`: `"ES256"` (or other supported algorithm)
- `jwk`: Client's public key (JWK format)

**Claims**:
- `jti`: Unique proof identifier (tracked for replay protection)
- `htm`: HTTP method (e.g., `"POST"`)
- `htu`: HTTP URI (without query/fragment)
- `iat`: Timestamp (must be recent, within 5 minutes)

**Example**:
```json
{
  "typ": "dpop+jwt",
  "alg": "ES256",
  "jwk": {
    "kty": "EC",
    "crv": "P-256",
    "x": "...",
    "y": "..."
  }
}
{
  "jti": "unique-id-123",
  "htm": "POST",
  "htu": "https://coves.social/xrpc/social.coves.community.create",
  "iat": 1700000000
}
```

## Security Considerations

### ✅ Implemented

- JWT signature verification with PDS public keys
- Token expiration validation
- DID format validation
- Required claims validation (sub, iss)
- Key caching with TTL
- Secure error messages (no internal details leaked)
- **DPoP proof verification** (proof-of-possession for token binding)
- **DPoP thumbprint validation** (prevents token theft attacks)
- **DPoP freshness checks** (5-minute proof validity window)
- **DPoP replay protection** (jti tracking with in-memory cache)
- **Secure DPoP model** (DPoP required AFTER signature verification, never as fallback)

### ⚠️ Not Yet Implemented

- Server-issued DPoP nonces (additional replay protection)
- Scope validation (checking `scope` claim)
- Audience validation (checking `aud` claim)
- Rate limiting per DID
- Token revocation checking

## Testing

Run the test suite:

```bash
go test ./internal/atproto/auth/... -v
```

### Manual Testing

1. **Phase 1 (Parse Only)**:
   ```bash
   # Create a test JWT (use jwt.io or a tool)
   export AUTH_SKIP_VERIFY=true
   curl -X POST http://localhost:8081/xrpc/social.coves.community.create \
     -H "Authorization: DPoP <test-jwt>" \
     -H "DPoP: <test-dpop-proof>" \
     -d '{"name":"Test","hostedByDid":"did:plc:test"}'
   ```

2. **Phase 2 (Full Verification)**:
   ```bash
   # Use a real JWT from a PDS
   export AUTH_SKIP_VERIFY=false
   curl -X POST http://localhost:8081/xrpc/social.coves.community.create \
     -H "Authorization: DPoP <real-jwt>" \
     -H "DPoP: <real-dpop-proof>" \
     -d '{"name":"Test","hostedByDid":"did:plc:test"}'
   ```

## Error Responses

### 401 Unauthorized

Missing or invalid token:

```json
{
  "error": "AuthenticationRequired",
  "message": "Missing Authorization header"
}
```

```json
{
  "error": "AuthenticationRequired",
  "message": "Invalid or expired token"
}
```

### Common Issues

1. **Missing Authorization header** → Add `Authorization: DPoP <token>` and `DPoP: <proof>`
2. **Token expired** → Get a new token from PDS
3. **Invalid signature** → Ensure token is from a valid PDS
4. **JWKS fetch fails** → Check PDS availability and network connectivity

## Future Enhancements

- [ ] DPoP nonce validation (server-managed nonce for additional replay protection)
- [ ] Scope-based authorization
- [ ] Audience claim validation
- [ ] Token revocation support
- [ ] Rate limiting per DID
- [ ] Metrics and monitoring
