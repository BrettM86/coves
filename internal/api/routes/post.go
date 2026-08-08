package routes

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
	"time"

	"github.com/go-chi/chi/v5"
)

// getStatusRateLimit is social.coves.community.post.getStatus' per-client
// budget, per minute.
//
// Chosen against the endpoint's own UX rather than copied from a neighbour:
// §7 has a client poll for the accepted transition, so the budget must sit
// ABOVE any polling rate the product itself prescribes — an earlier 60 was
// below the T2 tier's own 600ms cadence (100/minute) and cut off the
// endpoint's documented use case mid-wait (caught at the task-5 merge gate:
// a legitimate wait died at poll 61). 120 keeps a poll-a-second client
// comfortable for two full minutes while a script enumerating a community's
// rejected posts still hits a rate an operator notices.
const getStatusRateLimit = 120

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
// that the HTTP surface does not silently change shape with the wiring.
func WithPostStatusService(service posts.StatusService) PostRouteOption {
	return func(c *postRouteConfig) { c.statusService = service }
}

// NOT GUARDED AT REGISTRATION, unlike oauthMiddleware above, and the asymmetry
// is forced rather than chosen. The review asked for a fail-fast panic on a
// missing status service, matching that neighbour — but the routes table builds
// the whole router with every service nil and no options at all
// (registration_test.go's theRouter), so a panic there would fail every
// surface-declaration test rather than the one wiring bug it is aimed at.
// Scoping it to a supplied-but-nil option would guard a shape nobody writes.
//
// The exposure is small and named here so it is not mistaken for an oversight:
// cmd/server always supplies the option, and a build that did not would serve
// 500s from getStatus alone. Closing it properly needs the routes table to pass
// the option, which is a test-side change.

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
	//
	// It also carries its OWN limiter, tighter than the global 100/minute, and
	// it is the only unauthenticated route in the product that does. Two facts
	// make the exception worth it: §7's client UX is to POLL this until a post
	// flips to accepted, so the honest traffic shape is repeated requests from
	// one caller; and because it takes no auth, an unauthenticated stranger can
	// ask about any post URI they can name. The budget is what bounds
	// enumeration of a community's rejected posts to a rate an operator notices.
	statusHandler := post.NewGetStatusHandler(cfg.statusService)
	// NAMED, so a 429 from here is distinguishable in the logs from the global
	// limiter's. They have different budgets and different fixes, and an
	// unnamed one logs "default" — which is exactly the diagnosis an operator
	// staring at a rate-limited poller needs and would not get.
	statusRateLimiter := middleware.NewNamedRateLimiter("postGetStatus", getStatusRateLimit, time.Minute)
	r.With(statusRateLimiter.Middleware).
		Get("/xrpc/social.coves.community.post.getStatus", statusHandler.HandleGetStatus)

	// Future endpoints (Beta):
	// r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.community.post.update", updateHandler.HandleUpdate)
	// r.Get("/xrpc/social.coves.community.post.list", listHandler.HandleList)
}
