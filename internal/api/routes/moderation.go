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
	listActions := handler.NewListActionsHandler(service)
	listAdminActions := handler.NewListAdminActionsHandler(service)
	removeContent := handler.NewRemoveContentHandler(service)
	restoreContent := handler.NewRestoreContentHandler(service)
	r.Get("/xrpc/social.coves.moderation.listActions", listActions.HandleListActions)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.listAdminActions", listAdminActions.HandleListAdminActions)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getSubjectState", getSubjectState.HandleGetSubjectState)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.removeContent", removeContent.HandleRemoveContent)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.restoreContent", restoreContent.HandleRestoreContent)
}
