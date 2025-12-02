package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"

	"github.com/go-chi/chi/v5"
)

// RegisterPostRoutes registers post-related XRPC endpoints on the router
// Implements social.coves.community.post.* lexicon endpoints
func RegisterPostRoutes(r chi.Router, service posts.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	// Initialize handlers
	createHandler := post.NewCreateHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.post.create - create a new post in a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.create", createHandler.HandleCreate)

	// Future endpoints (Beta):
	// r.Get("/xrpc/social.coves.community.post.get", getHandler.HandleGet)
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.update", updateHandler.HandleUpdate)
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.delete", deleteHandler.HandleDelete)
	// r.Get("/xrpc/social.coves.community.post.list", listHandler.HandleList)
}
