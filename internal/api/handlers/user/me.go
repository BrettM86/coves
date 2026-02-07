package user

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/users"
)

// MeHandler handles requests for the authenticated user's own profile.
type MeHandler struct {
	userService users.UserService
}

// NewMeHandler creates a new MeHandler.
func NewMeHandler(userService users.UserService) *MeHandler {
	return &MeHandler{
		userService: userService,
	}
}

// HandleMe handles GET /api/me
// Returns the authenticated user's full profile with stats.
func (h *MeHandler) HandleMe(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeJSONError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	profile, err := h.userService.GetProfile(r.Context(), userDID)
	if err != nil {
		handleServiceError(w, err, userDID, "get profile")
		return
	}

	responseBytes, err := json.Marshal(profile)
	if err != nil {
		slog.Error("failed to marshal profile response",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		slog.Warn("failed to write me response",
			slog.String("did", userDID),
			slog.String("error", writeErr.Error()),
		)
	}
}
