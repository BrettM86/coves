package routes

import (
	"Coves/internal/api/handlers/actor"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
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
	// authority decides whether the viewer is an instance admin; instanceDID
	// identifies this instance's own removals and labels.
	authority moderation.Authority,
	instanceDID string,
) {
	// Create handlers
	getPostsHandler := actor.NewGetPostsHandler(postService, userService, voteService, blueskyService).
		WithViewerModeration(authority, instanceDID)
	getCommentsHandler := actor.NewGetCommentsHandler(commentService, userService, voteService).
		WithViewerModeration(authority, instanceDID)

	// GET /xrpc/social.coves.actor.getPosts
	// Public endpoint with optional auth for viewer-specific state (vote state, moderation actions)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.actor.getPosts", getPostsHandler.HandleGetPosts)

	// GET /xrpc/social.coves.actor.getComments
	// Public endpoint with optional auth for viewer-specific state (vote state, moderation actions)
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.actor.getComments", getCommentsHandler.HandleGetComments)
}
