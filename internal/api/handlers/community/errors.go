package community

import (
	"encoding/json"
	"log"
	"net/http"

	"Coves/internal/core/communities"
)

// XRPCError represents an XRPC error response
type XRPCError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes an XRPC error response
func writeError(w http.ResponseWriter, status int, error, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(XRPCError{
		Error:   error,
		Message: message,
	})
}

// handleServiceError converts service errors to appropriate HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case communities.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())
	case communities.IsConflict(err):
		if err == communities.ErrHandleTaken {
			writeError(w, http.StatusConflict, "NameTaken", "Community handle is already taken")
		} else {
			writeError(w, http.StatusConflict, "AlreadyExists", err.Error())
		}
	case communities.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case err == communities.ErrUnauthorized:
		writeError(w, http.StatusForbidden, "Forbidden", "You do not have permission to perform this action")
	case err == communities.ErrMemberBanned:
		writeError(w, http.StatusForbidden, "Blocked", "You are blocked from this community")
	default:
		// Internal server error - log the actual error for debugging
		// TODO: Use proper logger instead of log package
		log.Printf("XRPC handler error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
