package routes

import (
	"Coves/internal/api/handlers/discover"
	"Coves/internal/api/middleware"
	discoverCore "Coves/internal/core/discover"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterDiscoverRoutes registers discover-related XRPC endpoints
//
// SECURITY & RATE LIMITING:
// - Discover feed is PUBLIC (works without authentication)
// - Optional auth: if authenticated, includes viewer vote state on posts
// - Protected by global rate limiter: 100 requests/minute per IP (main.go:84)
// - Query timeout enforced via context (prevents long-running queries)
// - Result limit capped at 50 posts per request (validated in service layer)
// - No caching currently implemented (future: 30-60s cache for hot feed)
func RegisterDiscoverRoutes(
	r chi.Router,
	discoverService discoverCore.Service,
	voteService votes.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
) {
	// Create handlers
	getDiscoverHandler := discover.NewGetDiscoverHandler(discoverService, voteService)

	// GET /xrpc/social.coves.feed.getDiscover
	// Public endpoint with optional auth for viewer-specific state (vote state)
	// Shows posts from ALL communities (not personalized)
	// Rate limited: 100 req/min per IP via global middleware
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.feed.getDiscover", getDiscoverHandler.HandleGetDiscover)
}
