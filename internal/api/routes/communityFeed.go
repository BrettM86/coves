package routes

import (
	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/votes"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	// Full-text search is the most expensive read, so the global 100/minute cap
	// is not sufficient on its own; keep a tighter route-specific client budget.
	postSearchRateLimit  = 30
	postSearchRateWindow = time.Minute
)

// RegisterCommunityFeedRoutes registers feed-related XRPC endpoints
func RegisterCommunityFeedRoutes(
	r chi.Router,
	feedService communityFeeds.Service,
	voteService votes.Service,
	blueskyService blueskypost.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
	// authority decides whether the viewer is an instance admin; instanceDID
	// identifies this instance's own removals and labels.
	authority moderation.Authority,
	instanceDID string,
) {
	// Create handlers
	getCommunityHandler := communityFeed.NewGetCommunityHandler(feedService, voteService, blueskyService).
		WithViewerModeration(authority, instanceDID)
	searchPostsHandler := communityFeed.NewSearchPostsHandler(feedService, voteService, blueskyService).
		WithViewerModeration(authority, instanceDID)
	postSearchLimiter := middleware.NewNamedRateLimiter("postSearch", postSearchRateLimit, postSearchRateWindow)

	// GET /xrpc/social.coves.communityFeed.getCommunity
	// Public endpoint with optional auth for viewer-specific state (vote state, moderation actions)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.communityFeed.getCommunity", getCommunityHandler.HandleGetCommunity)

	// GET /xrpc/social.coves.feed.searchPosts
	r.With(postSearchLimiter.Middleware, authMiddleware.OptionalAuth).
		Get("/xrpc/social.coves.feed.searchPosts", searchPostsHandler.HandleSearchPosts)
}
