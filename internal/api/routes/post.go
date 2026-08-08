package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// PostRouteOption supplies a collaborator that only some of the post routes
// need.
//
// It is variadic rather than another positional parameter because the status
// query is served by its OWN service (posts.StatusService, which needs the
// admissions store and nothing else) and every existing caller — including the
// minimal wirings in tests — would otherwise have to name a dependency it has
// no opinion about.
type PostRouteOption func(*postRouteConfig)

type postRouteConfig struct {
	statusService posts.StatusService
}

// WithPostStatusService supplies the service behind
// social.coves.community.post.getStatus. The route is registered either way, so
// that the HTTP surface does not silently change shape with the wiring; without
// this option the handler has no service to call.
func WithPostStatusService(service posts.StatusService) PostRouteOption {
	return func(c *postRouteConfig) { c.statusService = service }
}

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
	opts ...PostRouteOption,
) {
	var cfg postRouteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	// oauthMiddleware.OptionalAuth gates the public get endpoint below. A nil value is a
	// wiring bug (minimal/test setups) that would otherwise panic on the first request to
	// post.get; fail fast at registration with a clear message instead.
	if oauthMiddleware == nil {
		panic("RegisterPostRoutes: oauthMiddleware is required (provides OptionalAuth for the public post.get endpoint)")
	}

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

	// social.coves.community.post.getStatus - one community's admission
	// decision about one post.
	//
	// NO auth middleware at all, unlike post.get beside it. The caller this is
	// built for is an author on ANOTHER server asking this host whether it
	// accepted their post (PRD §7): they have no account here, so there is no
	// session to require and no viewer state to personalise. OptionalAuth would
	// be harmless but pointless — the answer does not vary by viewer — while
	// RequireAuth would make the cross-server case unanswerable, which is the
	// asymmetry internal/api/routes/registration_test.go declares.
	statusHandler := post.NewGetStatusHandler(cfg.statusService)
	r.Get("/xrpc/social.coves.community.post.getStatus", statusHandler.HandleGetStatus)

	// Future endpoints (Beta):
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.update", updateHandler.HandleUpdate)
	// r.Get("/xrpc/social.coves.community.post.list", listHandler.HandleList)
}
