package communityFeed

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"Coves/internal/core/communityFeeds"
)

// ErrorResponse represents an XRPC error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON error response
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(ErrorResponse{
		Error:   errorType,
		Message: message,
	}); err != nil {
		// Log encoding errors but can't send error response (headers already sent)
		log.Printf("ERROR: Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, communityFeeds.ErrCommunityNotFound):
		writeError(w, http.StatusNotFound, "CommunityNotFound", "Community not found")

	case errors.Is(err, communityFeeds.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "InvalidCursor", "Invalid pagination cursor")

	case communityFeeds.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())

	default:
		// Internal server error - don't leak details
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
