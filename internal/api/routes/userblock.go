package routes

import (
	"Coves/internal/api/handlers/userblock"
	"Coves/internal/api/middleware"
	"Coves/internal/core/userblocks"

	"github.com/go-chi/chi/v5"
)

// RegisterUserBlockRoutes registers user-to-user blocking XRPC endpoints on the router
// Implements social.coves.actor.blockUser, unblockUser, and getBlockedUsers
func RegisterUserBlockRoutes(r chi.Router, service userblocks.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	handler := userblock.NewBlockHandler(service)

	// Procedure endpoints (POST) - require authentication
	// social.coves.actor.blockUser - block a user
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.actor.blockUser", handler.HandleBlock)

	// social.coves.actor.unblockUser - unblock a user
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.actor.unblockUser", handler.HandleUnblock)

	// Query endpoints (GET) - require authentication (viewing own blocks)
	// social.coves.actor.getBlockedUsers - list blocked users
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.actor.getBlockedUsers", handler.HandleGetBlocked)
}
