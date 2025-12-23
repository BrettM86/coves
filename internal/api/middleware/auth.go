package middleware

import (
	"Coves/internal/atproto/oauth"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Context keys for storing user information
type contextKey string

const (
	UserDIDKey          contextKey = "user_did"
	OAuthSessionKey     contextKey = "oauth_session"
	UserAccessToken     contextKey = "user_access_token" // Backward compatibility: handlers/tests using GetUserAccessToken()
	IsAggregatorAuthKey contextKey = "is_aggregator_auth"
	AuthMethodKey       contextKey = "auth_method"
)

// AuthMiddleware is an interface for authentication middleware
// Both OAuthAuthMiddleware and DualAuthMiddleware implement this
type AuthMiddleware interface {
	RequireAuth(next http.Handler) http.Handler
}

// Auth method constants
const (
	AuthMethodOAuth      = "oauth"
	AuthMethodServiceJWT = "service_jwt"
)

// SessionUnsealer is an interface for unsealing session tokens
// This allows for mocking in tests
type SessionUnsealer interface {
	UnsealSession(token string) (*oauth.SealedSession, error)
}

// AggregatorChecker is an interface for checking if a DID is a registered aggregator
type AggregatorChecker interface {
	IsAggregator(ctx context.Context, did string) (bool, error)
}

// ServiceAuthValidator is an interface for validating service JWTs
type ServiceAuthValidator interface {
	Validate(ctx context.Context, tokenString string, lexMethod *syntax.NSID) (syntax.DID, error)
}

// OAuthAuthMiddleware enforces OAuth authentication using sealed session tokens.
type OAuthAuthMiddleware struct {
	unsealer SessionUnsealer
	store    oauthlib.ClientAuthStore
}

// NewOAuthAuthMiddleware creates a new OAuth auth middleware using sealed session tokens.
func NewOAuthAuthMiddleware(unsealer SessionUnsealer, store oauthlib.ClientAuthStore) *OAuthAuthMiddleware {
	return &OAuthAuthMiddleware{
		unsealer: unsealer,
		store:    store,
	}
}

// RequireAuth middleware ensures the user is authenticated.
// Supports sealed session tokens via:
//   - Authorization: Bearer <sealed_token>
//   - Cookie: coves_session=<sealed_token>
//
// If not authenticated, returns 401.
// If authenticated, injects user DID into context.
func (m *OAuthAuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string

		// Try Authorization header first (for mobile/API clients)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			var ok bool
			token, ok = extractBearerToken(authHeader)
			if !ok {
				writeAuthError(w, "Invalid Authorization header format. Expected: Bearer <token>")
				return
			}
		}

		// If no header, try session cookie (for web clients)
		if token == "" {
			if cookie, err := r.Cookie("coves_session"); err == nil {
				token = cookie.Value
			}
		}

		// Must have authentication from either source
		if token == "" {
			writeAuthError(w, "Missing authentication")
			return
		}

		// Authenticate using sealed token
		sealedSession, err := m.unsealer.UnsealSession(token)
		if err != nil {
			log.Printf("[AUTH_FAILURE] type=unseal_failed ip=%s method=%s path=%s error=%v",
				r.RemoteAddr, r.Method, r.URL.Path, err)
			writeAuthError(w, "Invalid or expired token")
			return
		}

		// Parse DID
		did, err := syntax.ParseDID(sealedSession.DID)
		if err != nil {
			log.Printf("[AUTH_FAILURE] type=invalid_did ip=%s method=%s path=%s did=%s error=%v",
				r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, err)
			writeAuthError(w, "Invalid DID in token")
			return
		}

		// Load full OAuth session from database
		session, err := m.store.GetSession(r.Context(), did, sealedSession.SessionID)
		if err != nil {
			log.Printf("[AUTH_FAILURE] type=session_not_found ip=%s method=%s path=%s did=%s session_id=%s error=%v",
				r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, sealedSession.SessionID, err)
			writeAuthError(w, "Session not found or expired")
			return
		}

		// Verify session DID matches token DID
		if session.AccountDID.String() != sealedSession.DID {
			log.Printf("[AUTH_FAILURE] type=did_mismatch ip=%s method=%s path=%s token_did=%s session_did=%s",
				r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, session.AccountDID.String())
			writeAuthError(w, "Session DID mismatch")
			return
		}

		log.Printf("[AUTH_SUCCESS] ip=%s method=%s path=%s did=%s session_id=%s",
			r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, sealedSession.SessionID)

		// Inject user info and session into context
		ctx := context.WithValue(r.Context(), UserDIDKey, sealedSession.DID)
		ctx = context.WithValue(ctx, OAuthSessionKey, session)
		// Store access token for backward compatibility
		ctx = context.WithValue(ctx, UserAccessToken, session.AccessToken)

		// Call next handler
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth middleware loads user info if authenticated, but doesn't require it.
// Useful for endpoints that work for both authenticated and anonymous users.
//
// Supports sealed session tokens via:
//   - Authorization: Bearer <sealed_token>
//   - Cookie: coves_session=<sealed_token>
//
// If authentication fails, continues without user context (does not return error).
func (m *OAuthAuthMiddleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string

		// Try Authorization header first (for mobile/API clients)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			var ok bool
			token, ok = extractBearerToken(authHeader)
			if !ok {
				// Invalid format - continue without user context
				next.ServeHTTP(w, r)
				return
			}
		}

		// If no header, try session cookie (for web clients)
		if token == "" {
			if cookie, err := r.Cookie("coves_session"); err == nil {
				token = cookie.Value
			}
		}

		// If still no token, continue without authentication
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Try to authenticate (don't write errors, just continue without user context on failure)
		sealedSession, err := m.unsealer.UnsealSession(token)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		// Parse DID
		did, err := syntax.ParseDID(sealedSession.DID)
		if err != nil {
			log.Printf("[AUTH_WARNING] Optional auth: invalid DID: %v", err)
			next.ServeHTTP(w, r)
			return
		}

		// Load full OAuth session from database
		session, err := m.store.GetSession(r.Context(), did, sealedSession.SessionID)
		if err != nil {
			log.Printf("[AUTH_WARNING] Optional auth: session not found: %v", err)
			next.ServeHTTP(w, r)
			return
		}

		// Verify session DID matches token DID
		if session.AccountDID.String() != sealedSession.DID {
			log.Printf("[AUTH_WARNING] Optional auth: DID mismatch")
			next.ServeHTTP(w, r)
			return
		}

		// Build authenticated context
		ctx := context.WithValue(r.Context(), UserDIDKey, sealedSession.DID)
		ctx = context.WithValue(ctx, OAuthSessionKey, session)
		ctx = context.WithValue(ctx, UserAccessToken, session.AccessToken)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserDID extracts the user's DID from the request context
// Returns empty string if not authenticated
func GetUserDID(r *http.Request) string {
	val := r.Context().Value(UserDIDKey)
	did, ok := val.(string)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] GetUserDID: type assertion failed, expected string, got %T (value: %v)",
			val, val)
	}
	return did
}

// GetAuthenticatedDID extracts the authenticated user's DID from the context
// This is used by service layers for defense-in-depth validation
// Returns empty string if not authenticated
func GetAuthenticatedDID(ctx context.Context) string {
	val := ctx.Value(UserDIDKey)
	did, ok := val.(string)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] GetAuthenticatedDID: type assertion failed, expected string, got %T (value: %v)",
			val, val)
	}
	return did
}

// GetOAuthSession extracts the OAuth session from the request context
// Returns nil if not authenticated
// Handlers can use this to make authenticated PDS calls
func GetOAuthSession(r *http.Request) *oauthlib.ClientSessionData {
	val := r.Context().Value(OAuthSessionKey)
	session, ok := val.(*oauthlib.ClientSessionData)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] GetOAuthSession: type assertion failed, expected *ClientSessionData, got %T",
			val)
	}
	return session
}

// GetUserAccessToken extracts the user's access token from the request context
// Returns empty string if not authenticated
func GetUserAccessToken(r *http.Request) string {
	val := r.Context().Value(UserAccessToken)
	token, ok := val.(string)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] GetUserAccessToken: type assertion failed, expected string, got %T (value: %v)",
			val, val)
	}
	return token
}

// SetTestUserDID sets the user DID in the context for testing purposes
// This function should ONLY be used in tests to mock authenticated users
func SetTestUserDID(ctx context.Context, userDID string) context.Context {
	return context.WithValue(ctx, UserDIDKey, userDID)
}

// extractBearerToken extracts the token from a Bearer Authorization header.
// HTTP auth schemes are case-insensitive per RFC 7235, so "Bearer", "bearer", "BEARER" are all valid.
// Returns the token and true if valid Bearer scheme, empty string and false otherwise.
func extractBearerToken(authHeader string) (string, bool) {
	if authHeader == "" {
		return "", false
	}

	// Split on first space: "Bearer <token>" -> ["Bearer", "<token>"]
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return "", false
	}

	// Case-insensitive scheme comparison per RFC 7235
	if !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}

	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}

	return token, true
}

// writeAuthError writes a JSON error response for authentication failures
func writeAuthError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Use json.NewEncoder to properly escape the message and prevent injection
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error":   "AuthenticationRequired",
		"message": message,
	}); err != nil {
		log.Printf("Failed to write auth error response: %v", err)
	}
}

// DualAuthMiddleware enforces authentication using either OAuth sealed tokens (for users)
// or PDS service JWTs (for aggregators only).
type DualAuthMiddleware struct {
	unsealer          SessionUnsealer
	store             oauthlib.ClientAuthStore
	serviceValidator  ServiceAuthValidator
	aggregatorChecker AggregatorChecker
}

// NewDualAuthMiddleware creates a new dual auth middleware that supports both OAuth and service JWT authentication.
func NewDualAuthMiddleware(
	unsealer SessionUnsealer,
	store oauthlib.ClientAuthStore,
	serviceValidator ServiceAuthValidator,
	aggregatorChecker AggregatorChecker,
) *DualAuthMiddleware {
	return &DualAuthMiddleware{
		unsealer:          unsealer,
		store:             store,
		serviceValidator:  serviceValidator,
		aggregatorChecker: aggregatorChecker,
	}
}

// RequireAuth middleware ensures the user is authenticated via either OAuth or service JWT.
// Supports:
//   - OAuth sealed session tokens via Authorization: Bearer <sealed_token> or Cookie: coves_session=<sealed_token>
//   - Service JWTs via Authorization: Bearer <jwt>
//
// SECURITY: Service JWT authentication is RESTRICTED to registered aggregators only.
// Non-aggregator DIDs will be rejected even with valid JWT signatures.
// This enforcement happens in handleServiceAuth() via aggregatorChecker.IsAggregator().
//
// If not authenticated, returns 401.
// If authenticated, injects user DID and auth method into context.
func (m *DualAuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		var tokenSource string

		// Try Authorization header first (for mobile/API clients and service auth)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			var ok bool
			token, ok = extractBearerToken(authHeader)
			if !ok {
				writeAuthError(w, "Invalid Authorization header format. Expected: Bearer <token>")
				return
			}
			tokenSource = "header"
		}

		// If no header, try session cookie (for web clients - OAuth only)
		if token == "" {
			if cookie, err := r.Cookie("coves_session"); err == nil {
				token = cookie.Value
				tokenSource = "cookie"
			}
		}

		// Must have authentication from either source
		if token == "" {
			writeAuthError(w, "Missing authentication")
			return
		}

		log.Printf("[AUTH_TRACE] ip=%s method=%s path=%s token_source=%s",
			r.RemoteAddr, r.Method, r.URL.Path, tokenSource)

		// Detect token type and route to appropriate handler
		if isJWTFormat(token) {
			m.handleServiceAuth(w, r, next, token)
		} else {
			m.handleOAuthAuth(w, r, next, token)
		}
	})
}

// handleServiceAuth handles authentication using PDS service JWTs (aggregators only)
func (m *DualAuthMiddleware) handleServiceAuth(w http.ResponseWriter, r *http.Request, next http.Handler, token string) {
	// Validate the service JWT
	// Note: lexMethod is nil, which allows any lexicon method (endpoint-agnostic validation).
	// The ServiceAuthValidator skips the lexicon method check when nil (see indigo/atproto/auth/jwt.go:86-88).
	// This is intentional - we want aggregators to authenticate globally, not per-endpoint.
	did, err := m.serviceValidator.Validate(r.Context(), token, nil)
	if err != nil {
		log.Printf("[AUTH_FAILURE] type=service_jwt_invalid ip=%s method=%s path=%s error=%v",
			r.RemoteAddr, r.Method, r.URL.Path, err)
		writeAuthError(w, "Invalid or expired service JWT")
		return
	}

	// Convert DID to string
	didStr := did.String()

	// Verify this DID is a registered aggregator
	isAggregator, err := m.aggregatorChecker.IsAggregator(r.Context(), didStr)
	if err != nil {
		log.Printf("[AUTH_FAILURE] type=aggregator_check_failed ip=%s method=%s path=%s did=%s error=%v",
			r.RemoteAddr, r.Method, r.URL.Path, didStr, err)
		writeAuthError(w, "Failed to verify aggregator status")
		return
	}

	if !isAggregator {
		log.Printf("[AUTH_FAILURE] type=not_aggregator ip=%s method=%s path=%s did=%s",
			r.RemoteAddr, r.Method, r.URL.Path, didStr)
		writeAuthError(w, "Not a registered aggregator")
		return
	}

	log.Printf("[AUTH_SUCCESS] type=service_jwt ip=%s method=%s path=%s did=%s",
		r.RemoteAddr, r.Method, r.URL.Path, didStr)

	// Inject DID and auth method into context
	ctx := context.WithValue(r.Context(), UserDIDKey, didStr)
	ctx = context.WithValue(ctx, IsAggregatorAuthKey, true)
	ctx = context.WithValue(ctx, AuthMethodKey, AuthMethodServiceJWT)

	// Call next handler
	next.ServeHTTP(w, r.WithContext(ctx))
}

// handleOAuthAuth handles authentication using OAuth sealed session tokens (existing logic)
func (m *DualAuthMiddleware) handleOAuthAuth(w http.ResponseWriter, r *http.Request, next http.Handler, token string) {
	// Authenticate using sealed token
	sealedSession, err := m.unsealer.UnsealSession(token)
	if err != nil {
		log.Printf("[AUTH_FAILURE] type=unseal_failed ip=%s method=%s path=%s error=%v",
			r.RemoteAddr, r.Method, r.URL.Path, err)
		writeAuthError(w, "Invalid or expired token")
		return
	}

	// Parse DID
	did, err := syntax.ParseDID(sealedSession.DID)
	if err != nil {
		log.Printf("[AUTH_FAILURE] type=invalid_did ip=%s method=%s path=%s did=%s error=%v",
			r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, err)
		writeAuthError(w, "Invalid DID in token")
		return
	}

	// Load full OAuth session from database
	session, err := m.store.GetSession(r.Context(), did, sealedSession.SessionID)
	if err != nil {
		log.Printf("[AUTH_FAILURE] type=session_not_found ip=%s method=%s path=%s did=%s session_id=%s error=%v",
			r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, sealedSession.SessionID, err)
		writeAuthError(w, "Session not found or expired")
		return
	}

	// Verify session DID matches token DID
	if session.AccountDID.String() != sealedSession.DID {
		log.Printf("[AUTH_FAILURE] type=did_mismatch ip=%s method=%s path=%s token_did=%s session_did=%s",
			r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, session.AccountDID.String())
		writeAuthError(w, "Session DID mismatch")
		return
	}

	log.Printf("[AUTH_SUCCESS] type=oauth ip=%s method=%s path=%s did=%s session_id=%s",
		r.RemoteAddr, r.Method, r.URL.Path, sealedSession.DID, sealedSession.SessionID)

	// Inject user info and session into context
	ctx := context.WithValue(r.Context(), UserDIDKey, sealedSession.DID)
	ctx = context.WithValue(ctx, OAuthSessionKey, session)
	ctx = context.WithValue(ctx, UserAccessToken, session.AccessToken)
	ctx = context.WithValue(ctx, IsAggregatorAuthKey, false)
	ctx = context.WithValue(ctx, AuthMethodKey, AuthMethodOAuth)

	// Call next handler
	next.ServeHTTP(w, r.WithContext(ctx))
}

// isJWTFormat checks if a token has JWT format (three parts separated by dots).
// NOTE: This is a format heuristic for routing, not security validation.
// Actual JWT signature verification happens in ServiceAuthValidator.Validate().
func isJWTFormat(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	// Ensure all parts are non-empty to prevent misrouting crafted tokens like "..""
	return parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// IsAggregatorAuth checks if the current request was authenticated using aggregator service JWT
func IsAggregatorAuth(r *http.Request) bool {
	val := r.Context().Value(IsAggregatorAuthKey)
	isAggregator, ok := val.(bool)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] IsAggregatorAuth: type assertion failed, expected bool, got %T (value: %v)",
			val, val)
	}
	return isAggregator
}

// GetAuthMethod returns the authentication method used for the current request
// Returns empty string if not authenticated
func GetAuthMethod(r *http.Request) string {
	val := r.Context().Value(AuthMethodKey)
	method, ok := val.(string)
	if !ok && val != nil {
		// SECURITY: Type assertion failed but value exists - this should never happen
		// Log as error since this could indicate context value corruption
		log.Printf("[AUTH_ERROR] GetAuthMethod: type assertion failed, expected string, got %T (value: %v)",
			val, val)
	}
	return method
}
