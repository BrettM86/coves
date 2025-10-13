package routes

import (
	"Coves/internal/api/handlers/community"
	"Coves/internal/core/communities"

	"github.com/go-chi/chi/v5"
)

// RegisterCommunityRoutes registers community-related XRPC endpoints on the router
// Implements social.coves.community.* lexicon endpoints
func RegisterCommunityRoutes(r chi.Router, service communities.Service) {
	// Initialize handlers
	createHandler := community.NewCreateHandler(service)
	getHandler := community.NewGetHandler(service)
	listHandler := community.NewListHandler(service)
	searchHandler := community.NewSearchHandler(service)
	subscribeHandler := community.NewSubscribeHandler(service)

	// Query endpoints (GET)
	// social.coves.community.get - get a single community by identifier
	r.Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

	// social.coves.community.list - list communities with filters
	r.Get("/xrpc/social.coves.community.list", listHandler.HandleList)

	// social.coves.community.search - search communities
	r.Get("/xrpc/social.coves.community.search", searchHandler.HandleSearch)

	// Procedure endpoints (POST) - write-forward operations
	// social.coves.community.create - create a new community
	r.Post("/xrpc/social.coves.community.create", createHandler.HandleCreate)

	// social.coves.community.subscribe - subscribe to a community
	r.Post("/xrpc/social.coves.community.subscribe", subscribeHandler.HandleSubscribe)

	// social.coves.community.unsubscribe - unsubscribe from a community
	r.Post("/xrpc/social.coves.community.unsubscribe", subscribeHandler.HandleUnsubscribe)

	// TODO: Add update and delete handlers when implemented
	// r.Post("/xrpc/social.coves.community.update", updateHandler.HandleUpdate)
	// r.Post("/xrpc/social.coves.community.delete", deleteHandler.HandleDelete)
}
