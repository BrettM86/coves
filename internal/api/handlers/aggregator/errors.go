package aggregator

import (
	"Coves/internal/core/aggregators"
	"encoding/json"
	"log"
	"net/http"
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
		log.Printf("ERROR: Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	// Map domain errors to HTTP status codes
	switch {
	case aggregators.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())
	case aggregators.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case aggregators.IsUnauthorized(err):
		writeError(w, http.StatusForbidden, "Forbidden", err.Error())
	case aggregators.IsConflict(err):
		writeError(w, http.StatusConflict, "Conflict", err.Error())
	case aggregators.IsRateLimited(err):
		writeError(w, http.StatusTooManyRequests, "RateLimitExceeded", err.Error())
	case aggregators.IsNotImplemented(err):
		writeError(w, http.StatusNotImplemented, "NotImplemented", "This feature is not yet available (Phase 2)")
	default:
		// Internal errors - don't leak details
		log.Printf("ERROR: Aggregator service error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
