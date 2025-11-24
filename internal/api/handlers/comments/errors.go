package comments

import (
	"Coves/internal/core/comments"
	"encoding/json"
	"log"
	"net/http"
)

// errorResponse represents a standardized JSON error response
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON error response with the given status code
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(errorResponse{
		Error:   errorType,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service-layer errors to HTTP responses
// This follows the error handling pattern from other handlers (post, community)
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case comments.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())

	case comments.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())

	default:
		// Don't leak internal error details to clients
		log.Printf("Unexpected error in comments handler: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
