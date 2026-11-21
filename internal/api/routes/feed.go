package routes

import (
	"Coves/internal/api/handlers/feed"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterFeedRoutes registers social.coves.feed.getFeed and its aliases
// social.coves.feed.getTimeline, social.coves.feed.getDiscover and
// social.coves.communityFeed.getCommunity.
func RegisterFeedRoutes(
	r chi.Router,
	feedService feedgen.Service,
	registry *feedgen.Registry,
	communityService communities.Service,
	voteService votes.Service,
	blueskyService blueskypost.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
	// authority decides whether the viewer is an instance admin; instanceDID
	// identifies this instance's own removals and labels.
	authority moderation.Authority,
	instanceDID string,
	opts ...ViewerModerationRouteOption,
) {
	cfg := newViewerModerationRouteConfig(opts)
	getFeedHandler := feed.NewGetFeedHandler(feedService, communityService, voteService, blueskyService).
		WithViewerModeration(authority, instanceDID)
	if cfg.communityViewer != nil {
		getFeedHandler.WithCommunityViewerModeration(cfg.communityViewer)
	}

	// GET /xrpc/social.coves.feed.getFeed
	// Public with optional auth; scope home requires a signed-in viewer.
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.feed.getFeed", getFeedHandler.HandleGetFeed)

	// The old feed endpoints map sort to a built-in feed and serve it through
	// getFeed. getTimeline is the home feed and requires a signed-in viewer.
	aliasHandler := feed.NewFeedAliasHandler(getFeedHandler, registry)
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.feed.getTimeline", aliasHandler.HandleGetTimeline)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.feed.getDiscover", aliasHandler.HandleGetDiscover)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.communityFeed.getCommunity", aliasHandler.HandleGetCommunity)
}

// RegisterFeedGeneratorRoutes registers social.coves.feed.describeFeedGenerator
// and social.coves.feed.getFeedGenerators.
// Both are public and take no auth.
func RegisterFeedGeneratorRoutes(r chi.Router, registry *feedgen.Registry) {
	handler := feed.NewFeedGeneratorsHandler(registry)
	r.Get("/xrpc/social.coves.feed.describeFeedGenerator", handler.HandleDescribeFeedGenerator)
	r.Get("/xrpc/social.coves.feed.getFeedGenerators", handler.HandleGetFeedGenerators)
}
