package routes

import (
	"Coves/internal/api/handlers/actor"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterActorRoutes registers actor-related XRPC endpoints
func RegisterActorRoutes(
	r chi.Router,
	postService posts.Service,
	userService users.UserService,
	voteService votes.Service,
	blueskyService blueskypost.Service,
	commentService comments.Service,
	authMiddleware *middleware.OAuthAuthMiddleware,
) {
	// Create handlers
	getPostsHandler := actor.NewGetPostsHandler(postService, userService, voteService, blueskyService)
	getCommentsHandler := actor.NewGetCommentsHandler(commentService, userService, voteService)

	// GET /xrpc/social.coves.actor.getPosts
	// Public endpoint with optional auth for viewer-specific state (vote state)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.actor.getPosts", getPostsHandler.HandleGetPosts)

	// GET /xrpc/social.coves.actor.getComments
	// Public endpoint with optional auth for viewer-specific state (vote state)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.actor.getComments", getCommentsHandler.HandleGetComments)
}
