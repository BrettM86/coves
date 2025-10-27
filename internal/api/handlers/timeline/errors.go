package timeline

import (
	"Coves/internal/core/timeline"
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

// writeError writes a JSON error response
func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := XRPCError{
		Error:   errorType,
		Message: message,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("ERROR: Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case timeline.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, timeline.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "InvalidCursor", "The provided cursor is invalid")
	case errors.Is(err, timeline.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "User must be authenticated")
	default:
		log.Printf("ERROR: Timeline service error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An error occurred while fetching timeline")
	}
}
