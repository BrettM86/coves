package routes

import (
	"Coves/internal/api/handlers/community"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

	"github.com/go-chi/chi/v5"
)

// CommunityRouteOption supplies a collaborator that only some of the community
// routes need. Every route is registered either way, so the HTTP surface does
// not change shape with the wiring.
type CommunityRouteOption func(*communityRouteConfig)

type communityRouteConfig struct {
	getOptions []community.GetHandlerOption
	roles      communities.RoleReader
}

// WithCommunityGetOptions configures the community.get handler (for example its
// ban reader).
func WithCommunityGetOptions(opts ...community.GetHandlerOption) CommunityRouteOption {
	return func(c *communityRouteConfig) { c.getOptions = append(c.getOptions, opts...) }
}

// WithCommunityRoles supplies the community role index behind getModerators
// and getActorRoles.
func WithCommunityRoles(roles communities.RoleReader) CommunityRouteOption {
	return func(c *communityRouteConfig) { c.roles = roles }
}

// RegisterCommunityRoutes registers community-related XRPC endpoints on the router
// Implements social.coves.community.* lexicon endpoints. creation is the
// instance's community creation policy (COMMUNITY_CREATION) and admins decides
// who counts as an instance admin under it; admins must not be nil.
func RegisterCommunityRoutes(r chi.Router, service communities.Service, repo communities.Repository, authMiddleware *middleware.OAuthAuthMiddleware, creation communities.CreationPolicy, admins communities.InstanceAdminChecker, opts ...CommunityRouteOption) {
	var cfg communityRouteConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// Initialize handlers
	createHandler := community.NewCreateHandler(service, creation, admins)
	getHandler := community.NewGetHandler(service, repo, cfg.getOptions...)
	getModeratorsHandler := community.NewGetModeratorsHandler(service, cfg.roles)
	getActorRolesHandler := community.NewGetActorRolesHandler(cfg.roles)
	updateHandler := community.NewUpdateHandler(service)
	listHandler := community.NewListHandler(service, repo)
	searchHandler := community.NewSearchHandler(service)
	subscribeHandler := community.NewSubscribeHandler(service)
	blockHandler := community.NewBlockHandler(service)

	// Query endpoints (GET) - public access, optional auth for viewer state
	// social.coves.community.get - get a single community by identifier
	// Uses OptionalAuth to populate viewer.subscribed and viewer.blocked when authenticated
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.community.get", getHandler.HandleGet)

	// social.coves.community.list - list communities with filters
	// Uses OptionalAuth to populate viewer.subscribed when authenticated
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.community.list", listHandler.HandleList)

	// social.coves.community.getModerators - a community's owner and moderators
	// Public; OptionalAuth leaves room for viewer state
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.community.getModerators", getModeratorsHandler.HandleGetModerators)

	// social.coves.community.getActorRoles - the communities an account holds a role in
	// Public, as the role records are
	r.With(authMiddleware.OptionalAuth).Get("/xrpc/social.coves.community.getActorRoles", getActorRolesHandler.HandleGetActorRoles)

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

	// social.coves.community.getBlockedCommunities - list the authenticated user's community blocks
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.community.getBlockedCommunities", blockHandler.HandleGetBlocked)

	// TODO: Add delete handler when implemented
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.delete", deleteHandler.HandleDelete)
}
