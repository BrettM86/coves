package routes

import (
	"Coves/internal/api/handlers/vote"
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterVoteRoutes registers vote-related XRPC endpoints on the router
// Implements social.coves.interaction.* lexicon endpoints for voting
func RegisterVoteRoutes(r chi.Router, service votes.Service, authMiddleware *middleware.AtProtoAuthMiddleware) {
	// Initialize handlers
	createVoteHandler := vote.NewCreateVoteHandler(service)
	deleteVoteHandler := vote.NewDeleteVoteHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.interaction.createVote - create or toggle a vote on a post/comment
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.interaction.createVote", createVoteHandler.HandleCreateVote)

	// social.coves.interaction.deleteVote - delete a vote from a post/comment
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.interaction.deleteVote", deleteVoteHandler.HandleDeleteVote)
}
