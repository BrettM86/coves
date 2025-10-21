package routes

import (
	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/core/communityFeeds"

	"github.com/go-chi/chi/v5"
)

// RegisterCommunityFeedRoutes registers feed-related XRPC endpoints
func RegisterCommunityFeedRoutes(
	r chi.Router,
	feedService communityFeeds.Service,
) {
	// Create handlers
	getCommunityHandler := communityFeed.NewGetCommunityHandler(feedService)

	// GET /xrpc/social.coves.communityFeed.getCommunity
	// Public endpoint - basic community sorting only for Alpha
	// TODO(feed-generator): Add OptionalAuth middleware when implementing viewer-specific state
	//                       (blocks, upvotes, saves, etc.) in feed generator skeleton
	r.Get("/xrpc/social.coves.communityFeed.getCommunity", getCommunityHandler.HandleGetCommunity)
}
