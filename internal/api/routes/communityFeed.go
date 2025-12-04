package routes

import (
	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterCommunityFeedRoutes registers feed-related XRPC endpoints
func RegisterCommunityFeedRoutes(
	r chi.Router,
	feedService communityFeeds.Service,
	voteService votes.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
) {
	// Create handlers
	getCommunityHandler := communityFeed.NewGetCommunityHandler(feedService, voteService)

	// GET /xrpc/social.coves.communityFeed.getCommunity
	// Public endpoint with optional auth for viewer-specific state (vote state)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.communityFeed.getCommunity", getCommunityHandler.HandleGetCommunity)
}
