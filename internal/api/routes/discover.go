package routes

import (
	"Coves/internal/api/handlers/discover"
	discoverCore "Coves/internal/core/discover"

	"github.com/go-chi/chi/v5"
)

// RegisterDiscoverRoutes registers discover-related XRPC endpoints
//
// SECURITY & RATE LIMITING:
// - Discover feed is PUBLIC (no authentication required)
// - Protected by global rate limiter: 100 requests/minute per IP (main.go:84)
// - Query timeout enforced via context (prevents long-running queries)
// - Result limit capped at 50 posts per request (validated in service layer)
// - No caching currently implemented (future: 30-60s cache for hot feed)
func RegisterDiscoverRoutes(
	r chi.Router,
	discoverService discoverCore.Service,
) {
	// Create handlers
	getDiscoverHandler := discover.NewGetDiscoverHandler(discoverService)

	// GET /xrpc/social.coves.feed.getDiscover
	// Public endpoint - no authentication required
	// Shows posts from ALL communities (not personalized)
	// Rate limited: 100 req/min per IP via global middleware
	r.Get("/xrpc/social.coves.feed.getDiscover", getDiscoverHandler.HandleGetDiscover)
}
