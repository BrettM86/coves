package routes

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

// RegisterOAuthRoutes registers OAuth-related endpoints on the router with dedicated rate limiting
// OAuth endpoints have stricter rate limits to prevent:
// - Credential stuffing attacks on login endpoints
// - OAuth state exhaustion
// - Refresh token abuse
func RegisterOAuthRoutes(r chi.Router, handler *oauth.OAuthHandler, allowedOrigins []string) {
	// Create stricter rate limiters for OAuth endpoints
	// Login endpoints: 10 req/min per IP (credential stuffing protection)
	loginLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

	// Refresh endpoint: 20 req/min per IP (slightly higher for legitimate token refresh)
	refreshLimiter := middleware.NewRateLimiter(20, 1*time.Minute)

	// Logout endpoint: 10 req/min per IP
	logoutLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

	// OAuth metadata endpoints - public, no extra rate limiting (use global limit)
	r.Get("/oauth/client-metadata.json", handler.HandleClientMetadata)
	r.Get("/.well-known/oauth-protected-resource", handler.HandleProtectedResourceMetadata)

	// OAuth flow endpoints - stricter rate limiting for authentication attempts
	r.With(loginLimiter.Middleware).Get("/oauth/login", handler.HandleLogin)
	r.With(loginLimiter.Middleware).Get("/oauth/mobile/login", handler.HandleMobileLogin)

	// OAuth callback - needs CORS for potential cross-origin redirects from PDS
	// Use login limiter since callback completes the authentication flow
	r.With(corsMiddleware(allowedOrigins), loginLimiter.Middleware).Get("/oauth/callback", handler.HandleCallback)

	// Mobile Universal Link callback route
	// This route is used for iOS Universal Links and Android App Links
	// Path must match the path in .well-known/apple-app-site-association
	// Uses the same handler as web callback - the system routes it to the mobile app
	r.With(loginLimiter.Middleware).Get("/app/oauth/callback", handler.HandleCallback)

	// Session management - dedicated rate limits
	r.With(logoutLimiter.Middleware).Post("/oauth/logout", handler.HandleLogout)
	r.With(refreshLimiter.Middleware).Post("/oauth/refresh", handler.HandleRefresh)
}

// corsMiddleware creates a CORS middleware for OAuth callback with specific allowed origins
func corsMiddleware(allowedOrigins []string) func(next http.Handler) http.Handler {
	return cors.Handler(cors.Options{
		AllowedOrigins: allowedOrigins, // Only allow specific origins for OAuth callback
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"X-CSRF-Token",
		},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300, // 5 minutes
	})
}
