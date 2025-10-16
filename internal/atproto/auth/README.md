# atProto OAuth Authentication

This package implements third-party OAuth authentication for Coves, validating JWT Bearer tokens from mobile apps and other atProto clients.

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
Authorization: Bearer <jwt>
    ↓
Auth Middleware
    ↓
Extract JWT → Parse Claims → Verify Signature (via JWKS)
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
  -H "Authorization: Bearer eyJhbGc..." \
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

## Security Considerations

### ✅ Implemented

- JWT signature verification with PDS public keys
- Token expiration validation
- DID format validation
- Required claims validation (sub, iss)
- Key caching with TTL
- Secure error messages (no internal details leaked)

### ⚠️ Not Yet Implemented

- DPoP validation (for replay attack prevention)
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
     -H "Authorization: Bearer <test-jwt>" \
     -d '{"name":"Test","hostedByDid":"did:plc:test"}'
   ```

2. **Phase 2 (Full Verification)**:
   ```bash
   # Use a real JWT from a PDS
   export AUTH_SKIP_VERIFY=false
   curl -X POST http://localhost:8081/xrpc/social.coves.community.create \
     -H "Authorization: Bearer <real-jwt>" \
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

1. **Missing Authorization header** → Add `Authorization: Bearer <token>`
2. **Token expired** → Get a new token from PDS
3. **Invalid signature** → Ensure token is from a valid PDS
4. **JWKS fetch fails** → Check PDS availability and network connectivity

## Future Enhancements

- [ ] DPoP proof validation
- [ ] Scope-based authorization
- [ ] Audience claim validation
- [ ] Token revocation support
- [ ] Rate limiting per DID
- [ ] Metrics and monitoring
