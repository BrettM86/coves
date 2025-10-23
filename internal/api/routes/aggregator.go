package routes

import (
	"Coves/internal/api/handlers/aggregator"
	"Coves/internal/core/aggregators"

	"github.com/go-chi/chi/v5"
)

// RegisterAggregatorRoutes registers aggregator-related XRPC endpoints
// Following Bluesky's pattern for feed generators and labelers
func RegisterAggregatorRoutes(
	r chi.Router,
	aggregatorService aggregators.Service,
) {
	// Create query handlers
	getServicesHandler := aggregator.NewGetServicesHandler(aggregatorService)
	getAuthorizationsHandler := aggregator.NewGetAuthorizationsHandler(aggregatorService)
	listForCommunityHandler := aggregator.NewListForCommunityHandler(aggregatorService)

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

	// Write endpoints (Phase 2 - require authentication and moderator permissions)
	// TODO: Implement after Jetstream consumer is ready
	// POST /xrpc/social.coves.aggregator.enable (requires auth + moderator)
	// POST /xrpc/social.coves.aggregator.disable (requires auth + moderator)
	// POST /xrpc/social.coves.aggregator.updateConfig (requires auth + moderator)
}
