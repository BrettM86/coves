package user

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/users"
)

// DeleteHandler handles account deletion requests
type DeleteHandler struct {
	userService users.UserService
}

// NewDeleteHandler creates a new delete handler
func NewDeleteHandler(userService users.UserService) *DeleteHandler {
	return &DeleteHandler{
		userService: userService,
	}
}

// DeleteAccountResponse represents the response for account deletion
type DeleteAccountResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// HandleDeleteAccount handles POST /xrpc/social.coves.actor.deleteAccount
// Deletes the authenticated user's account from the Coves AppView.
// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
// The user's identity remains intact for use with other atProto apps.
//
// Security:
//   - Requires OAuth authentication
//   - Users can ONLY delete their own account (DID from auth context)
//   - No request body required - DID is derived from authenticated session
func (h *DeleteHandler) HandleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	// 1. Check HTTP method
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// 2. Extract authenticated user DID from request context (injected by auth middleware)
	// SECURITY: This ensures users can ONLY delete their own account
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeJSONError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// 3. Delete the account
	// The service handles validation, logging, and atomic deletion
	err := h.userService.DeleteAccount(r.Context(), userDID)
	if err != nil {
		handleServiceError(w, err, userDID, "account deletion")
		return
	}

	// 4. Return success response
	// Marshal JSON before writing headers to catch encoding errors early
	response := DeleteAccountResponse{
		Success: true,
		Message: "Account deleted successfully. Your atProto identity remains intact on your PDS.",
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		slog.Error("failed to marshal delete account response",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		slog.Warn("failed to write delete account response",
			slog.String("did", userDID),
			slog.String("error", writeErr.Error()),
		)
	}
}

// handleServiceError maps service errors to HTTP responses.
// operation is a human-readable label for log messages (e.g. "account deletion", "get profile").
func handleServiceError(w http.ResponseWriter, err error, userDID, operation string) {
	if err == nil {
		slog.Error(operation+" reached its error path without an error",
			slog.String("did", userDID))
		accountErrorMapper.Write(w, err)
		return
	}

	// The DID and operation are worth recording on every failure here, not just
	// the unmapped ones the mapper logs, so account problems can be traced to a
	// user. Severity follows the answer, though: a mistyped DID or a missing
	// account is the caller's ordinary mistake, and logging those at ERROR — as
	// this did before the outcome was available to branch on — buries real
	// faults in alert noise.
	mapping, matched := accountErrorMapper.Resolve(err)
	switch {
	case errors.Is(err, context.Canceled):
		slog.Info(operation+" canceled",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
	case matched && mapping.Status < http.StatusInternalServerError:
		slog.Info(operation+" rejected",
			slog.String("did", userDID),
			slog.String("code", mapping.Code),
			slog.String("error", err.Error()),
		)
	default:
		slog.Error(operation+" failed",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
	}

	accountErrorMapper.Write(w, err)
}
