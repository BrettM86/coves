package routes

import (
	"time"

	"Coves/internal/api/handlers/communityFeed"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

const (
	// Full-text search is the most expensive read, so the global 100/minute cap
	// is not sufficient on its own; keep a tighter route-specific client budget.
	postSearchRateLimit  = 30
	postSearchRateWindow = time.Minute
)

// RegisterCommunityFeedRoutes registers social.coves.feed.searchPosts.
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
	searchPostsHandler := communityFeed.NewSearchPostsHandler(feedService, voteService, blueskyService).
		WithViewerModeration(authority, instanceDID)
	postSearchLimiter := middleware.NewNamedRateLimiter("postSearch", postSearchRateLimit, postSearchRateWindow)

	// GET /xrpc/social.coves.feed.searchPosts
	r.With(postSearchLimiter.Middleware, authMiddleware.OptionalAuth).
		Get("/xrpc/social.coves.feed.searchPosts", searchPostsHandler.HandleSearchPosts)
}
