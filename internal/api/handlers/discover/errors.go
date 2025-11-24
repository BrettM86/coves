package discover

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"Coves/internal/core/discover"
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
	case discover.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, discover.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "InvalidCursor", "The provided cursor is invalid")
	default:
		log.Printf("ERROR: Discover service error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An error occurred while fetching discover feed")
	}
}
