package notification

import (
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/notifications"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// HandleUpdateSeen serves social.coves.notification.updateSeen.
func (h *Handler) HandleUpdateSeen(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	var input struct {
		SeenAt string `json:"seenAt"`
	}
	if !xrpc.DecodeJSON(w, r, reqbody.LimitTiny, &input) {
		return
	}
	seenAt, err := syntax.ParseDatetimeTime(input.SeenAt)
	if err != nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid seenAt datetime")
		return
	}
	if err := h.service.UpdateSeen(r.Context(), userDID, seenAt); err != nil {
		if errors.Is(err, notifications.ErrAccountNotIndexed) {
			slog.WarnContext(r.Context(), "notification seen update rejected: account not indexed")
			xrpc.WriteError(w, http.StatusBadRequest, "AccountNotIndexed", "Account is not indexed")
			return
		}
		slog.ErrorContext(r.Context(), "failed to update notification seen time", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	w.WriteHeader(http.StatusOK)
}
