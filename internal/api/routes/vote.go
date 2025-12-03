package routes

import (
	"Coves/internal/api/handlers/vote"
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"

	"github.com/go-chi/chi/v5"
)

// RegisterVoteRoutes registers vote-related XRPC endpoints on the router
// Implements social.coves.feed.vote.* lexicon endpoints
func RegisterVoteRoutes(r chi.Router, voteService votes.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	// Initialize handlers
	createHandler := vote.NewCreateVoteHandler(voteService)
	deleteHandler := vote.NewDeleteVoteHandler(voteService)

	// Procedure endpoints (POST) - require authentication
	// social.coves.feed.vote.create - create or update a vote on a post/comment
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.feed.vote.create", createHandler.HandleCreateVote)

	// social.coves.feed.vote.delete - delete a vote from a post/comment
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.feed.vote.delete", deleteHandler.HandleDeleteVote)
}
