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
	UserDIDKey      contextKey = "user_did"
	OAuthSessionKey contextKey = "oauth_session"
	UserAccessToken contextKey = "user_access_token" // Kept for backward compatibility
)

// SessionUnsealer is an interface for unsealing session tokens
// This allows for mocking in tests
type SessionUnsealer interface {
	UnsealSession(token string) (*oauth.SealedSession, error)
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
	did, _ := r.Context().Value(UserDIDKey).(string)
	return did
}

// GetAuthenticatedDID extracts the authenticated user's DID from the context
// This is used by service layers for defense-in-depth validation
// Returns empty string if not authenticated
func GetAuthenticatedDID(ctx context.Context) string {
	did, _ := ctx.Value(UserDIDKey).(string)
	return did
}

// GetOAuthSession extracts the OAuth session from the request context
// Returns nil if not authenticated
// Handlers can use this to make authenticated PDS calls
func GetOAuthSession(r *http.Request) *oauthlib.ClientSessionData {
	session, _ := r.Context().Value(OAuthSessionKey).(*oauthlib.ClientSessionData)
	return session
}

// GetUserAccessToken extracts the user's access token from the request context
// Returns empty string if not authenticated
func GetUserAccessToken(r *http.Request) string {
	token, _ := r.Context().Value(UserAccessToken).(string)
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
