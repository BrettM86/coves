package routes

import (
	"Coves/internal/api/handlers/timeline"
	"Coves/internal/api/middleware"
	timelineCore "Coves/internal/core/timeline"

	"github.com/go-chi/chi/v5"
)

// RegisterTimelineRoutes registers timeline-related XRPC endpoints
func RegisterTimelineRoutes(
	r chi.Router,
	timelineService timelineCore.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
) {
	// Create handlers
	getTimelineHandler := timeline.NewGetTimelineHandler(timelineService)

	// GET /xrpc/social.coves.feed.getTimeline
	// Requires authentication - user must be logged in to see their timeline
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.feed.getTimeline", getTimelineHandler.HandleGetTimeline)
}
