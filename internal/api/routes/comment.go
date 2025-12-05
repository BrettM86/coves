package routes

import (
	"Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	commentsCore "Coves/internal/core/comments"

	"github.com/go-chi/chi/v5"
)

// RegisterCommentRoutes registers comment-related XRPC endpoints on the router
// Implements social.coves.community.comment.* lexicon endpoints
// All write operations (create, update, delete) require authentication
func RegisterCommentRoutes(r chi.Router, service commentsCore.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	// Initialize handlers
	createHandler := comments.NewCreateCommentHandler(service)
	updateHandler := comments.NewUpdateCommentHandler(service)
	deleteHandler := comments.NewDeleteCommentHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.community.comment.create - create a new comment on a post or another comment
	r.With(authMiddleware.RequireAuth).Post(
		"/xrpc/social.coves.community.comment.create",
		createHandler.HandleCreate)

	// social.coves.community.comment.update - update an existing comment's content
	r.With(authMiddleware.RequireAuth).Post(
		"/xrpc/social.coves.community.comment.update",
		updateHandler.HandleUpdate)

	// social.coves.community.comment.delete - soft delete a comment
	r.With(authMiddleware.RequireAuth).Post(
		"/xrpc/social.coves.community.comment.delete",
		deleteHandler.HandleDelete)
}
