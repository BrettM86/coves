package routes

import (
	"Coves/internal/api/handlers/communitysuggestion"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communitysuggestions"
	"time"

	"github.com/go-chi/chi/v5"
)

// RegisterCommunitySuggestionRoutes registers community suggestion XRPC endpoints on the router
// Implements social.coves.community.suggestion.* endpoints for community suggestions and voting
// adminDIDs restricts who can update suggestion status (reuses COMMUNITY_CREATORS env var)
func RegisterCommunitySuggestionRoutes(
	r chi.Router,
	service communitysuggestions.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
	adminDIDs []string,
) {
	// Initialize handlers
	createHandler := communitysuggestion.NewCreateHandler(service)
	listHandler := communitysuggestion.NewListHandler(service)
	getHandler := communitysuggestion.NewGetHandler(service)
	voteHandler := communitysuggestion.NewVoteHandler(service)
	updateStatusHandler := communitysuggestion.NewUpdateStatusHandler(service, adminDIDs)

	// Create IP rate limiter for suggestion creation
	// Allow 10 requests per minute per IP to prevent abuse
	createRateLimiter := middleware.NewRateLimiter(10, time.Minute)

	// Create IP rate limiter for voting
	// Allow 30 requests per minute per IP to prevent vote-toggle abuse
	voteRateLimiter := middleware.NewRateLimiter(30, time.Minute)

	// Query endpoints (GET) - public access, optional auth for viewer state
	// social.coves.community.suggestion.list - list suggestions with filters
	r.With(authMiddleware.OptionalAuth).Get(
		"/xrpc/social.coves.community.suggestion.list",
		listHandler.HandleList)

	// social.coves.community.suggestion.get - get a single suggestion by ID
	r.With(authMiddleware.OptionalAuth).Get(
		"/xrpc/social.coves.community.suggestion.get",
		getHandler.HandleGet)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.suggestion.create - create a new suggestion
	r.With(
		createRateLimiter.Middleware,
		authMiddleware.RequireAuth,
	).Post(
		"/xrpc/social.coves.community.suggestion.create",
		createHandler.HandleCreate)

	// social.coves.community.suggestion.vote - cast or toggle a vote
	r.With(
		voteRateLimiter.Middleware,
		authMiddleware.RequireAuth,
	).Post(
		"/xrpc/social.coves.community.suggestion.vote",
		voteHandler.HandleVote)

	// social.coves.community.suggestion.removeVote - remove a vote
	r.With(
		voteRateLimiter.Middleware,
		authMiddleware.RequireAuth,
	).Post(
		"/xrpc/social.coves.community.suggestion.removeVote",
		voteHandler.HandleRemoveVote)

	// social.coves.community.suggestion.updateStatus - update suggestion status (admin only)
	r.With(authMiddleware.RequireAuth).Post(
		"/xrpc/social.coves.community.suggestion.updateStatus",
		updateStatusHandler.HandleUpdateStatus)
}
