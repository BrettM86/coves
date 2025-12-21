package routes

import (
	"Coves/internal/api/handlers/aggregator"
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// RegisterAggregatorRoutes registers aggregator-related XRPC endpoints
// Following Bluesky's pattern for feed generators and labelers
func RegisterAggregatorRoutes(
	r chi.Router,
	aggregatorService aggregators.Service,
	communityService communities.Service,
	userService users.UserService,
	identityResolver identity.Resolver,
) {
	// Create query handlers
	getServicesHandler := aggregator.NewGetServicesHandler(aggregatorService)
	getAuthorizationsHandler := aggregator.NewGetAuthorizationsHandler(aggregatorService)
	listForCommunityHandler := aggregator.NewListForCommunityHandler(aggregatorService, communityService)

	// Create registration handler
	registerHandler := aggregator.NewRegisterHandler(userService, identityResolver)

	// Query endpoints (public - no auth required)
	// GET /xrpc/social.coves.aggregator.getServices?dids=did:plc:abc,did:plc:def
	// Following app.bsky.feed.getFeedGenerators pattern
	r.Get("/xrpc/social.coves.aggregator.getServices", getServicesHandler.HandleGetServices)

	// GET /xrpc/social.coves.aggregator.getAuthorizations?aggregatorDid=did:plc:abc&enabledOnly=true
	// Lists communities that authorized an aggregator
	r.Get("/xrpc/social.coves.aggregator.getAuthorizations", getAuthorizationsHandler.HandleGetAuthorizations)

	// GET /xrpc/social.coves.aggregator.listForCommunity?communityDid=did:plc:xyz&enabledOnly=true
	// Lists aggregators authorized by a community
	r.Get("/xrpc/social.coves.aggregator.listForCommunity", listForCommunityHandler.HandleListForCommunity)

	// Registration endpoint (public - no auth required)
	// Aggregators register themselves after creating their own PDS accounts
	// POST /xrpc/social.coves.aggregator.register
	// Rate limited to 10 requests per 10 minutes per IP to prevent abuse
	registrationRateLimiter := middleware.NewRateLimiter(10, 10*time.Minute)
	r.Post("/xrpc/social.coves.aggregator.register",
		registrationRateLimiter.Middleware(http.HandlerFunc(registerHandler.HandleRegister)).ServeHTTP)

	// Write endpoints (Phase 2 - require authentication and moderator permissions)
	// TODO: Implement after Jetstream consumer is ready
	// POST /xrpc/social.coves.aggregator.enable (requires auth + moderator)
	// POST /xrpc/social.coves.aggregator.disable (requires auth + moderator)
	// POST /xrpc/social.coves.aggregator.updateConfig (requires auth + moderator)
}
