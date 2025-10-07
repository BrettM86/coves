package middleware

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"Coves/internal/api/handlers/oauth"
	atprotoOAuth "Coves/internal/atproto/oauth"
	oauthCore "Coves/internal/core/oauth"
)

// Context keys for storing user information
type contextKey string

const (
	UserDIDKey      contextKey = "user_did"
	OAuthSessionKey contextKey = "oauth_session"
)

const (
	sessionName = "coves_session"
	sessionDID  = "did"
)

// AuthMiddleware enforces OAuth authentication for protected routes
type AuthMiddleware struct {
	authService *oauthCore.AuthService
}

// NewAuthMiddleware creates a new auth middleware
func NewAuthMiddleware(sessionStore oauthCore.SessionStore) (*AuthMiddleware, error) {
	privateJWK := os.Getenv("OAUTH_PRIVATE_JWK")
	if privateJWK == "" {
		return nil, fmt.Errorf("OAUTH_PRIVATE_JWK not configured")
	}

	// Parse OAuth client key
	privateKey, err := atprotoOAuth.ParseJWKFromJSON([]byte(privateJWK))
	if err != nil {
		return nil, fmt.Errorf("failed to parse OAuth private key: %w", err)
	}

	// Get AppView URL
	appviewURL := os.Getenv("APPVIEW_PUBLIC_URL")
	if appviewURL == "" {
		appviewURL = "http://localhost:8081"
	}

	// Determine client ID
	var clientID string
	if strings.HasPrefix(appviewURL, "http://localhost") || strings.HasPrefix(appviewURL, "http://127.0.0.1") {
		clientID = "http://localhost?redirect_uri=" + appviewURL + "/oauth/callback&scope=atproto%20transition:generic"
	} else {
		clientID = appviewURL + "/oauth/client-metadata.json"
	}

	redirectURI := appviewURL + "/oauth/callback"

	oauthClient := atprotoOAuth.NewClient(clientID, privateKey, redirectURI)
	authService := oauthCore.NewAuthService(sessionStore, oauthClient)

	return &AuthMiddleware{
		authService: authService,
	}, nil
}

// RequireAuth middleware ensures the user is authenticated
// If not authenticated, returns 401
// If authenticated, injects user DID and OAuth session into context
func (m *AuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get HTTP session
		cookieStore := oauth.GetCookieStore()
		httpSession, err := cookieStore.Get(r, sessionName)
		if err != nil || httpSession.IsNew {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Get DID from session
		did, ok := httpSession.Values[sessionDID].(string)
		if !ok || did == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Load OAuth session from database
		session, err := m.authService.ValidateSession(r.Context(), did)
		if err != nil {
			log.Printf("Failed to load OAuth session for DID %s: %v", did, err)
			http.Error(w, "Session expired", http.StatusUnauthorized)
			return
		}

		// Check if token needs refresh and refresh if necessary
		session, err = m.authService.RefreshTokenIfNeeded(r.Context(), session, oauth.TokenRefreshThreshold)
		if err != nil {
			log.Printf("Failed to refresh token for DID %s: %v", did, err)
			http.Error(w, "Session expired", http.StatusUnauthorized)
			return
		}

		// Inject user info into context
		ctx := context.WithValue(r.Context(), UserDIDKey, did)
		ctx = context.WithValue(ctx, OAuthSessionKey, session)

		// Call next handler
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth middleware loads user info if authenticated, but doesn't require it
// Useful for endpoints that work for both authenticated and anonymous users
func (m *AuthMiddleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get HTTP session
		cookieStore := oauth.GetCookieStore()
		httpSession, err := cookieStore.Get(r, sessionName)
		if err != nil || httpSession.IsNew {
			// Not authenticated - continue without user context
			next.ServeHTTP(w, r)
			return
		}

		// Get DID from session
		did, ok := httpSession.Values[sessionDID].(string)
		if !ok || did == "" {
			// No DID - continue without user context
			next.ServeHTTP(w, r)
			return
		}

		// Load OAuth session from database
		session, err := m.authService.ValidateSession(r.Context(), did)
		if err != nil {
			// Session expired - continue without user context
			next.ServeHTTP(w, r)
			return
		}

		// Try to refresh token if needed (best effort)
		refreshedSession, err := m.authService.RefreshTokenIfNeeded(r.Context(), session, oauth.TokenRefreshThreshold)
		if err != nil {
			// If refresh fails, continue with old session (best effort)
			// Session will still be valid for a few more minutes
		} else {
			session = refreshedSession
		}

		// Inject user info into context
		ctx := context.WithValue(r.Context(), UserDIDKey, did)
		ctx = context.WithValue(ctx, OAuthSessionKey, session)

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

// GetOAuthSession extracts the OAuth session from the request context
// Returns nil if not authenticated
func GetOAuthSession(r *http.Request) *oauthCore.OAuthSession {
	session, _ := r.Context().Value(OAuthSessionKey).(*oauthCore.OAuthSession)
	return session
}
