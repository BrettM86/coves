package notification

import (
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/notifications"
)

// UnreadCountResponse is the getUnreadCount output.
type UnreadCountResponse struct {
	Count int `json:"count"`
}

// Handler serves social.coves.notification.* endpoints.
type Handler struct{ service notifications.Service }

// NewHandler builds the notification handler.
func NewHandler(service notifications.Service) *Handler { return &Handler{service: service} }

// HandleGetUnreadCount serves social.coves.notification.getUnreadCount.
func (h *Handler) HandleGetUnreadCount(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	count, err := h.service.CountUnread(r.Context(), userDID)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to count unread notifications", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, UnreadCountResponse{Count: count})
}
