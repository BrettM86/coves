package aggregator

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
)

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

// errorMapper maps aggregator service errors to XRPC responses.
//
// Handlers here call into communities as well, to resolve a community
// identifier, so those errors are checked first and answer with the more
// specific CommunityNotFound.
var errorMapper = xrpc.NewMapper("aggregator",
	xrpc.MatchDetail(communities.IsNotFound, http.StatusNotFound, "CommunityNotFound"),
	xrpc.MatchDetail(communities.IsValidationError, http.StatusBadRequest, "InvalidRequest"),

	xrpc.MatchDetail(aggregators.IsNotFound, http.StatusNotFound, "NotFound"),
	xrpc.MatchDetail(aggregators.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
	xrpc.MatchDetail(aggregators.IsUnauthorized, http.StatusForbidden, "Forbidden"),
	xrpc.MatchDetail(aggregators.IsConflict, http.StatusConflict, "Conflict"),
	xrpc.MatchDetail(aggregators.IsRateLimited, http.StatusTooManyRequests, "RateLimitExceeded"),
	xrpc.Match(aggregators.IsNotImplemented, http.StatusNotImplemented,
		"NotImplemented", "This feature is not yet available (Phase 2)"),
)

// writeError writes a JSON error response with proper buffering.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
// Handles errors from both aggregators and communities packages.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
