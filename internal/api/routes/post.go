package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterPostRoutes registers post-related XRPC endpoints on the router
// Implements social.coves.community.post.* lexicon endpoints
// authMiddleware can be either OAuthAuthMiddleware or DualAuthMiddleware (used for
// write procedures via RequireAuth). oauthMiddleware is the OAuth middleware whose
// OptionalAuth is used for public reads so authenticated viewers get viewer state.
func RegisterPostRoutes(
	r chi.Router,
	service posts.Service,
	voteService votes.Service,
	blueskyService blueskypost.Service,
	authMiddleware middleware.AuthMiddleware,
	oauthMiddleware *middleware.OAuthAuthMiddleware,
) {
	// Initialize handlers
	createHandler := post.NewCreateHandler(service)
	deleteHandler := post.NewDeleteHandler(service)
	getHandler := post.NewGetHandler(service, voteService, blueskyService)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.post.create - create a new post in a community
	// Supports both OAuth (users) and service JWT (aggregators) authentication
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.create", createHandler.HandleCreate)

	// social.coves.community.post.delete - delete a post from a community
	// Only post authors can delete their own posts
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.delete", deleteHandler.HandleDelete)

	// Query endpoints (GET)
	// social.coves.community.post.get - batch fetch post views by AT-URI.
	// Public endpoint with optional auth so authenticated viewers receive their vote state.
	// Used for feed-skeleton hydration and permalink / cold-load rendering.
	r.With(oauthMiddleware.OptionalAuth).Get("/xrpc/social.coves.community.post.get", getHandler.HandleGet)

	// Future endpoints (Beta):
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.update", updateHandler.HandleUpdate)
	// r.Get("/xrpc/social.coves.community.post.list", listHandler.HandleList)
}
