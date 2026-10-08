package notification

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/notifications"
)

const defaultNotificationListLimit = 50
const maximumNotificationListLimit = 100

// ListHandler serves social.coves.notification.listNotifications.
type ListHandler struct{ service notifications.ListService }

// NewListHandler builds the listNotifications handler.
func NewListHandler(service notifications.ListService) *ListHandler {
	return &ListHandler{service: service}
}

// HandleListNotifications serves the authenticated account's notifications.
func (h *ListHandler) HandleListNotifications(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	limit := defaultNotificationListLimit
	if values, present := r.URL.Query()["limit"]; present {
		var err error
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > maximumNotificationListLimit {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Limit must be between 1 and 100")
			return
		}
	}
	output, err := h.service.ListNotifications(r.Context(), userDID, r.URL.Query().Get("cursor"), limit)
	if errors.Is(err, notifications.ErrInvalidCursor) {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidCursor", "Invalid notification cursor")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to list notifications", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}
