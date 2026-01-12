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
		handleServiceError(w, err, userDID)
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

// writeJSONError writes a JSON error response
// Marshals JSON before writing headers to catch encoding errors
func writeJSONError(w http.ResponseWriter, statusCode int, errorType, message string) {
	responseBytes, err := json.Marshal(map[string]interface{}{
		"error":   errorType,
		"message": message,
	})
	if err != nil {
		// Fallback to plain text if JSON encoding fails (should never happen with simple strings)
		slog.Error("failed to marshal error response", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(message))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		slog.Warn("failed to write error response", slog.String("error", writeErr.Error()))
	}
}

// handleServiceError maps service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error, userDID string) {
	// Check for specific error types
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		writeJSONError(w, http.StatusNotFound, "AccountNotFound", "Account not found")

	case errors.Is(err, context.DeadlineExceeded):
		slog.Error("account deletion timed out",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusGatewayTimeout, "Timeout", "Request timed out")

	case errors.Is(err, context.Canceled):
		slog.Info("account deletion canceled",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")

	default:
		// Check for InvalidDIDError
		var invalidDIDErr *users.InvalidDIDError
		if errors.As(err, &invalidDIDErr) {
			writeJSONError(w, http.StatusBadRequest, "InvalidDID", invalidDIDErr.Error())
			return
		}

		// Internal server error - don't leak details
		slog.Error("account deletion failed",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
