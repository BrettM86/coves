package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"

	"github.com/go-chi/chi/v5"
)

// RegisterPostRoutes registers post-related XRPC endpoints on the router
// Implements social.coves.post.* lexicon endpoints
func RegisterPostRoutes(r chi.Router, service posts.Service, authMiddleware *middleware.AtProtoAuthMiddleware) {
	// Initialize handlers
	createHandler := post.NewCreateHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.post.create - create a new post in a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.post.create", createHandler.HandleCreate)

	// Future endpoints (Beta):
	// r.Get("/xrpc/social.coves.post.get", getHandler.HandleGet)
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.post.update", updateHandler.HandleUpdate)
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.post.delete", deleteHandler.HandleDelete)
	// r.Get("/xrpc/social.coves.post.list", listHandler.HandleList)
}
