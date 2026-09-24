package routes

import (
	handler "Coves/internal/api/handlers/moderation"
	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
)

// RegisterModerationRoutes registers the social.coves.moderation.* endpoints.
// Moderation NSIDs are AppView-served, never PDS-proxied, so the PDS OAuth
// scopes in cmd/server remain unchanged (see oauth_scopes_test.go).
func RegisterModerationRoutes(r chi.Router, service moderation.Service, adminAuth *middleware.InstanceAdminMiddleware) {
	getSubjectState := handler.NewGetSubjectStateHandler(service)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getSubjectState", getSubjectState.HandleGetSubjectState)
}
