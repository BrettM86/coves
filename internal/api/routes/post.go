package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"

	"github.com/go-chi/chi/v5"
)

// RegisterPostRoutes registers post-related XRPC endpoints on the router
// Implements social.coves.community.post.* lexicon endpoints
// authMiddleware can be either OAuthAuthMiddleware or DualAuthMiddleware
func RegisterPostRoutes(r chi.Router, service posts.Service, authMiddleware middleware.AuthMiddleware) {
	// Initialize handlers
	createHandler := post.NewCreateHandler(service)
	deleteHandler := post.NewDeleteHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.post.create - create a new post in a community
	// Supports both OAuth (users) and service JWT (aggregators) authentication
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.create", createHandler.HandleCreate)

	// social.coves.community.post.delete - delete a post from a community
	// Only post authors can delete their own posts
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.delete", deleteHandler.HandleDelete)

	// Future endpoints (Beta):
	// r.Get("/xrpc/social.coves.community.post.get", getHandler.HandleGet)
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.update", updateHandler.HandleUpdate)
	// r.Get("/xrpc/social.coves.community.post.list", listHandler.HandleList)
}
