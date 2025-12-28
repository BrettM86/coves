package aggregator

import (
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"bytes"
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse represents an XRPC error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeJSONResponse buffers the JSON encoding before sending headers.
// This ensures that encoding failures don't result in partial responses
// with already-sent headers. Returns true if the response was written
// successfully, false otherwise.
func writeJSONResponse(w http.ResponseWriter, statusCode int, data interface{}) bool {
	// Buffer the JSON first to detect encoding errors before sending headers
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		log.Printf("ERROR: Failed to encode JSON response: %v", err)
		// Send a proper error response since we haven't sent headers yet
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"InternalServerError","message":"Failed to encode response"}`))
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("ERROR: Failed to write response body: %v", err)
		return false
	}
	return true
}

// writeError writes a JSON error response with proper buffering
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	writeJSONResponse(w, statusCode, ErrorResponse{
		Error:   errorType,
		Message: message,
	})
}

// handleServiceError maps service errors to HTTP responses
// Handles errors from both aggregators and communities packages
func handleServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	// Map domain errors to HTTP status codes
	// Check community errors first (for ResolveCommunityIdentifier calls)
	switch {
	case communities.IsNotFound(err):
		writeError(w, http.StatusNotFound, "CommunityNotFound", err.Error())
	case communities.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
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
