package routes

import (
	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"

	"github.com/go-chi/chi/v5"
)

// RegisterNotificationRoutes registers social.coves.notification.* endpoints.
func RegisterNotificationRoutes(r chi.Router, service notifications.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	handler := notification.NewHandler(service)
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.notification.getUnreadCount", handler.HandleGetUnreadCount)
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.notification.updateSeen", handler.HandleUpdateSeen)
}

// RegisterNotificationPreferenceRoutes registers the authenticated preferences endpoints.
func RegisterNotificationPreferenceRoutes(r chi.Router, service notifications.PreferencesService, authMiddleware *middleware.OAuthAuthMiddleware) {
	handler := notification.NewPreferencesHandler(service)
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.notification.getPreferences", handler.HandleGetPreferences)
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.notification.putPreferences", handler.HandlePutPreferences)
}

// RegisterNotificationListRoutes registers the authenticated list endpoint.
func RegisterNotificationListRoutes(r chi.Router, service notifications.ListService, authMiddleware *middleware.OAuthAuthMiddleware) {
	handler := notification.NewListHandler(service)
	r.With(authMiddleware.RequireAuth).Get("/xrpc/social.coves.notification.listNotifications", handler.HandleListNotifications)
}
