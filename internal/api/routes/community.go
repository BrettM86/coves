package routes

import (
	"Coves/internal/api/handlers/community"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

	"github.com/go-chi/chi/v5"
)

// RegisterCommunityRoutes registers community-related XRPC endpoints on the router
// Implements social.coves.community.* lexicon endpoints
func RegisterCommunityRoutes(r chi.Router, service communities.Service, authMiddleware *middleware.AtProtoAuthMiddleware) {
	// Initialize handlers
	createHandler := community.NewCreateHandler(service)
	getHandler := community.NewGetHandler(service)
	updateHandler := community.NewUpdateHandler(service)
	listHandler := community.NewListHandler(service)
	searchHandler := community.NewSearchHandler(service)
	subscribeHandler := community.NewSubscribeHandler(service)
	blockHandler := community.NewBlockHandler(service)

	// Query endpoints (GET) - public access
	// social.coves.community.get - get a single community by identifier
	r.Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

	// social.coves.community.list - list communities with filters
	r.Get("/xrpc/social.coves.community.list", listHandler.HandleList)

	// social.coves.community.search - search communities
	r.Get("/xrpc/social.coves.community.search", searchHandler.HandleSearch)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.create - create a new community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.create", createHandler.HandleCreate)

	// social.coves.community.update - update an existing community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.update", updateHandler.HandleUpdate)

	// social.coves.community.subscribe - subscribe to a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.subscribe", subscribeHandler.HandleSubscribe)

	// social.coves.community.unsubscribe - unsubscribe from a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.unsubscribe", subscribeHandler.HandleUnsubscribe)

	// social.coves.community.blockCommunity - block a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.blockCommunity", blockHandler.HandleBlock)

	// social.coves.community.unblockCommunity - unblock a community
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.unblockCommunity", blockHandler.HandleUnblock)

	// TODO: Add delete handler when implemented
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.delete", deleteHandler.HandleDelete)
}
