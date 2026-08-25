package main

import (
	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/routes"
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/api/handlers/aggregator"
	commentsAPI "Coves/internal/api/handlers/comments"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

const (
	// globalRateLimit and globalRateWindow bound every request by client IP.
	// Per-route limiters layer stricter caps on the expensive and
	// abuse-prone endpoints.
	globalRateLimit  = 100
	globalRateWindow = time.Minute

	// Nested comment queries fan out across the comment tree, so they carry a
	// tighter cap than the global one.
	commentQueryRateLimit  = 20
	commentQueryRateWindow = time.Minute
)

// newRouter builds the chi router with the middleware stack every request
// passes through. Order matters: RequestID and Recoverer must wrap the rate
// limiter so a rejected request is still logged and a panic in the limiter
// cannot take down the process.
func newRouter(otelMiddleware func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()

	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	// Backstop body cap for every route, present and future. Handlers layer
	// tighter per-endpoint limits via reqbody.DecodeJSON (equal, on the image
	// tier); this bound only has to make "one giant request OOMs the AppView"
	// impossible.
	r.Use(chiMiddleware.RequestSize(int64(reqbody.LimitGlobal)))
	r.Use(middleware.NewNamedRateLimiter("global", globalRateLimit, globalRateWindow).Middleware)

	if otelMiddleware != nil {
		r.Use(otelMiddleware)
	}

	return r
}

// registerRoutes mounts every HTTP endpoint the server serves.
//
// Consumers are started before this runs, so /health/consumers can be bound
// directly to the live connector set instead of reading it through shared
// mutable state.
func registerRoutes(r chi.Router, app *application, consumers *consumerSet) {
	registerXRPCRoutes(r, app)
	registerOAuthRoutes(r, app)
	registerWebRoutes(r, app)
	registerHealthRoutes(r, app, consumers)

	if app.imageProxyHandler != nil {
		routes.RegisterImageProxyRoutes(r, app.imageProxyHandler)
		slog.Info("registered image proxy route", "path", "/img/{preset}/plain/{did}/{cid}")
	}
}

// registerXRPCRoutes mounts the atProto XRPC surface.
func registerXRPCRoutes(r chi.Router, app *application) {
	routes.RegisterUserRoutesWithOptions(r, app.userService, app.authMiddleware,
		app.oauthClient.ClientApp, &routes.UserRouteOptions{UserBlockRepo: app.userBlockRepo})

	routes.RegisterCommunityRoutes(r, app.communityService, app.communityRepo,
		app.authMiddleware, app.cfg.Instance.AllowedCommunityCreators)

	// Posts accept dual auth so aggregator bots can publish with a service
	// JWT or API key rather than a user's OAuth session.
	routes.RegisterPostRoutes(r, app.postService, app.voteService, app.blueskyService,
		app.dualAuth, app.authMiddleware,
		routes.WithPostStatusService(app.postStatusService))

	routes.RegisterVoteRoutes(r, app.voteService, app.authMiddleware)
	routes.RegisterUserBlockRoutes(r, app.userBlockService, app.authMiddleware)
	routes.RegisterCommentRoutes(r, app.commentService, app.authMiddleware)
	routes.RegisterAdminReportRoutes(r, app.adminReportService, app.authMiddleware)
	routes.RegisterCommunitySuggestionRoutes(r, app.communitySuggestionService,
		app.authMiddleware, app.cfg.Instance.AllowedCommunityCreators)

	// Feed, timeline, discover, and actor routes take optional auth: they are
	// public, but an authenticated viewer additionally gets vote state and
	// block filtering.
	routes.RegisterCommunityFeedRoutes(r, app.feedService, app.voteService,
		app.blueskyService, app.authMiddleware)
	routes.RegisterTimelineRoutes(r, app.timelineService, app.voteService,
		app.blueskyService, app.authMiddleware)
	routes.RegisterDiscoverRoutes(r, app.discoverService, app.voteService,
		app.blueskyService, app.authMiddleware)
	routes.RegisterActorRoutes(r, app.postService, app.userService, app.voteService,
		app.blueskyService, app.commentService, app.authMiddleware)

	// The registration handler fetches .well-known/atproto-did from a domain an
	// unauthenticated caller supplies, so its client is guarded; the hatch is
	// for a dev stack whose fixture domains resolve to the machine itself.
	routes.RegisterAggregatorRoutes(r, app.aggregatorService, app.communityService,
		app.userService, app.identityResolver,
		aggregator.PrivateHostOptions(app.allowPrivateHosts())...)
	routes.RegisterAggregatorAPIKeyRoutes(r, app.authMiddleware, app.apiKeyService, app.aggregatorService)

	registerCommentQueryRoute(r, app)

	slog.Info("XRPC endpoints registered")
}

// registerCommentQueryRoute mounts the comment tree query separately because
// it needs both optional authentication (for viewer vote state) and a rate
// limit stricter than the global one.
func registerCommentQueryRoute(r chi.Router, app *application) {
	limiter := middleware.NewNamedRateLimiter("commentQuery",
		commentQueryRateLimit, commentQueryRateWindow)
	handler := commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(app.commentService))

	r.Handle(
		"/xrpc/social.coves.community.comment.getComments",
		limiter.Middleware(
			commentsAPI.OptionalAuthMiddleware(app.authMiddleware, handler.HandleGetComments),
		),
	)
}

// registerOAuthRoutes mounts the OAuth authentication flow.
func registerOAuthRoutes(r chi.Router, app *application) {
	routes.RegisterOAuthRoutes(r, app.oauthHandler, oauthAllowedOrigins(app))
	slog.Info("OAuth endpoints registered")
}

// oauthAllowedOrigins lists the origins permitted to make credentialed
// cross-origin requests to the OAuth endpoints.
//
// A wildcard is never used: these requests carry credentials, and "*" with
// credentials would let any site drive a user's OAuth flow.
func oauthAllowedOrigins(app *application) []string {
	origins := []string{app.cfg.OAuth.PublicURL}

	if app.cfg.IsDevEnv {
		// Local frontends: the SvelteKit dev server, the PDS, and the
		// AppView itself, on both localhost spellings.
		origins = append(origins,
			"http://localhost:3000",
			"http://localhost:3001",
			"http://localhost:5173",
			"http://127.0.0.1:8080",
			"http://127.0.0.1:3000",
			"http://127.0.0.1:3001",
			"http://127.0.0.1:5173",
		)
		slog.Warn("dev mode: OAuth CORS additionally allows localhost origins")
	}

	slog.Info("OAuth CORS configured", "allowed_origins", origins)
	return origins
}

// registerWebRoutes mounts the landing page, the account deletion flow, the
// mobile captcha page, static assets, and the mobile deep-linking manifests.
func registerWebRoutes(r chi.Router, app *application) {
	routes.RegisterWellKnownRoutes(r)
	routes.RegisterWebRoutes(r, app.oauthClient, app.userService, app.cfg.Signup.TurnstileSiteKey)
	slog.Info("web and well-known endpoints registered")
}

// registerHealthRoutes mounts liveness and indexing-health endpoints.
//
// /health and /xrpc/_health stay pure liveness checks — they are the container
// healthcheck target, so a Jetstream outage must not restart-loop the AppView.
// Indexing health is reported separately by /health/consumers, which monitoring
// can alert on without the orchestrator acting on it.
func registerHealthRoutes(r chi.Router, app *application, consumers *consumerSet) {
	r.Get("/health", livenessHandler)
	r.Get("/xrpc/_health", livenessHandler)
	// The option is added only when a driver actually runs. Passing a typed nil
	// pointer instead would be non-nil to an interface comparison, and the
	// response would carry an all-zero queue — which reads as a driver that is
	// running and settling nothing, the exact failure an operator watches for.
	var healthOptions []consumerHealthOption
	if app.acceptanceQueue != nil {
		healthOptions = append(healthOptions, withAcceptanceQueue(app.acceptanceQueue))
	}
	r.Get("/health/consumers", consumerHealthHandler(consumers.connectors, app.jetstreamState, healthOptions...))
}
