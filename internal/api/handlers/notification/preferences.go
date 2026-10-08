package notification

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/notifications"
)

// PreferencesResponse is the getPreferences and putPreferences output.
type PreferencesResponse struct {
	PostReply    bool `json:"postReply"`
	CommentReply bool `json:"commentReply"`
	Mention      bool `json:"mention"`
	Upvote       bool `json:"upvote"`
}

// PreferencesHandler serves the notification preferences endpoints.
type PreferencesHandler struct {
	service notifications.PreferencesService
}

// NewPreferencesHandler builds the preferences handler.
func NewPreferencesHandler(service notifications.PreferencesService) *PreferencesHandler {
	return &PreferencesHandler{service: service}
}

// HandleGetPreferences serves social.coves.notification.getPreferences.
func (h *PreferencesHandler) HandleGetPreferences(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	preferences, err := h.service.GetPreferences(r.Context(), userDID)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to get notification preferences", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, preferencesResponse(preferences))
}

// HandlePutPreferences serves social.coves.notification.putPreferences.
func (h *PreferencesHandler) HandlePutPreferences(w http.ResponseWriter, r *http.Request) {
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	var values map[string]json.RawMessage
	if !xrpc.DecodeJSON(w, r, reqbody.LimitTiny, &values) {
		return
	}
	if values == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}
	var update notifications.PreferencesUpdate
	for _, field := range []struct {
		name   string
		target **bool
	}{
		{"postReply", &update.PostReply},
		{"commentReply", &update.CommentReply},
		{"mention", &update.Mention},
		{"upvote", &update.Upvote},
	} {
		raw, present := values[field.name]
		if !present {
			continue
		}
		var enabled *bool
		if err := json.Unmarshal(raw, &enabled); err != nil || enabled == nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Preference values must be booleans")
			return
		}
		*field.target = enabled
	}
	preferences, err := h.service.PutPreferences(r.Context(), userDID, update)
	if errors.Is(err, notifications.ErrAccountNotIndexed) {
		slog.WarnContext(r.Context(), "notification preferences rejected: account not indexed")
		xrpc.WriteError(w, http.StatusBadRequest, "AccountNotIndexed", "Account is not indexed")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to put notification preferences", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, preferencesResponse(preferences))
}

func preferencesResponse(preferences notifications.Preferences) PreferencesResponse {
	return PreferencesResponse{
		PostReply:    preferences.PostReply,
		CommentReply: preferences.CommentReply,
		Mention:      preferences.Mention,
		Upvote:       preferences.Upvote,
	}
}
