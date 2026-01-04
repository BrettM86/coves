package community

import (
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
	if err := json.NewEncoder(w).Encode(XRPCError{
		Error:   error,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
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
	// PDS-specific errors (from DPoP authentication or PDS API calls)
	case errors.Is(err, pds.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request to PDS")
	case errors.Is(err, pds.ErrNotFound):
		writeError(w, http.StatusNotFound, "NotFound", "Record not found on PDS")
	case errors.Is(err, pds.ErrConflict):
		writeError(w, http.StatusConflict, "Conflict", "Record was modified by another operation")
	case errors.Is(err, pds.ErrUnauthorized), errors.Is(err, pds.ErrForbidden):
		// PDS auth errors should prompt re-authentication
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required or session expired")
	default:
		// Internal server error - log the actual error for debugging
		log.Printf("XRPC handler error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
