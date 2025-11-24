package middleware

import (
	"context"
	"log"
	"net/http"
	"strings"

	"Coves/internal/atproto/auth"
)

// Context keys for storing user information
type contextKey string

const (
	UserDIDKey      contextKey = "user_did"
	JWTClaimsKey    contextKey = "jwt_claims"
	UserAccessToken contextKey = "user_access_token"
)

// AtProtoAuthMiddleware enforces atProto OAuth authentication for protected routes
// Validates JWT Bearer tokens from the Authorization header
type AtProtoAuthMiddleware struct {
	jwksFetcher auth.JWKSFetcher
	skipVerify  bool // For Phase 1 testing only
}

// NewAtProtoAuthMiddleware creates a new atProto auth middleware
// skipVerify: if true, only parses JWT without signature verification (Phase 1)
//
//	if false, performs full signature verification (Phase 2)
func NewAtProtoAuthMiddleware(jwksFetcher auth.JWKSFetcher, skipVerify bool) *AtProtoAuthMiddleware {
	return &AtProtoAuthMiddleware{
		jwksFetcher: jwksFetcher,
		skipVerify:  skipVerify,
	}
}

// RequireAuth middleware ensures the user is authenticated with a valid JWT
// If not authenticated, returns 401
// If authenticated, injects user DID and JWT claims into context
func (m *AtProtoAuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeAuthError(w, "Missing Authorization header")
			return
		}

		// Must be Bearer token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeAuthError(w, "Invalid Authorization header format. Expected: Bearer <token>")
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		token = strings.TrimSpace(token)

		var claims *auth.Claims
		var err error

		if m.skipVerify {
			// Phase 1: Parse only (no signature verification)
			claims, err = auth.ParseJWT(token)
			if err != nil {
				log.Printf("[AUTH_FAILURE] type=parse_error ip=%s method=%s path=%s error=%v",
					r.RemoteAddr, r.Method, r.URL.Path, err)
				writeAuthError(w, "Invalid token")
				return
			}
		} else {
			// Phase 2: Full verification with signature check
			claims, err = auth.VerifyJWT(r.Context(), token, m.jwksFetcher)
			if err != nil {
				// Try to extract issuer for better logging
				issuer := "unknown"
				if parsedClaims, parseErr := auth.ParseJWT(token); parseErr == nil {
					issuer = parsedClaims.Issuer
				}
				log.Printf("[AUTH_FAILURE] type=verification_failed ip=%s method=%s path=%s issuer=%s error=%v",
					r.RemoteAddr, r.Method, r.URL.Path, issuer, err)
				writeAuthError(w, "Invalid or expired token")
				return
			}
		}

		// Extract user DID from 'sub' claim
		userDID := claims.Subject
		if userDID == "" {
			writeAuthError(w, "Missing user DID in token")
			return
		}

		// Inject user info and access token into context
		ctx := context.WithValue(r.Context(), UserDIDKey, userDID)
		ctx = context.WithValue(ctx, JWTClaimsKey, claims)
		ctx = context.WithValue(ctx, UserAccessToken, token)

		// Call next handler
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth middleware loads user info if authenticated, but doesn't require it
// Useful for endpoints that work for both authenticated and anonymous users
func (m *AtProtoAuthMiddleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			// Not authenticated - continue without user context
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		token = strings.TrimSpace(token)

		var claims *auth.Claims
		var err error

		if m.skipVerify {
			// Phase 1: Parse only
			claims, err = auth.ParseJWT(token)
		} else {
			// Phase 2: Full verification
			claims, err = auth.VerifyJWT(r.Context(), token, m.jwksFetcher)
		}

		if err != nil {
			// Invalid token - continue without user context
			log.Printf("Optional auth failed: %v", err)
			next.ServeHTTP(w, r)
			return
		}

		// Inject user info and access token into context
		ctx := context.WithValue(r.Context(), UserDIDKey, claims.Subject)
		ctx = context.WithValue(ctx, JWTClaimsKey, claims)
		ctx = context.WithValue(ctx, UserAccessToken, token)

		// Call next handler
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

// GetJWTClaims extracts the JWT claims from the request context
// Returns nil if not authenticated
func GetJWTClaims(r *http.Request) *auth.Claims {
	claims, _ := r.Context().Value(JWTClaimsKey).(*auth.Claims)
	return claims
}

// SetTestUserDID sets the user DID in the context for testing purposes
// This function should ONLY be used in tests to mock authenticated users
func SetTestUserDID(ctx context.Context, userDID string) context.Context {
	return context.WithValue(ctx, UserDIDKey, userDID)
}

// GetUserAccessToken extracts the user's access token from the request context
// Returns empty string if not authenticated
func GetUserAccessToken(r *http.Request) string {
	token, _ := r.Context().Value(UserAccessToken).(string)
	return token
}

// writeAuthError writes a JSON error response for authentication failures
func writeAuthError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Simple error response matching XRPC error format
	response := `{"error":"AuthenticationRequired","message":"` + message + `"}`
	if _, err := w.Write([]byte(response)); err != nil {
		log.Printf("Failed to write auth error response: %v", err)
	}
}
