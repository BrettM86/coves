package post

import (
	"encoding/json"
	"log"
	"net/http"

	"Coves/internal/core/aggregators"
	"Coves/internal/core/posts"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON error response
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

// handleServiceError maps service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case err == posts.ErrCommunityNotFound:
		writeError(w, http.StatusNotFound, "CommunityNotFound",
			"Community not found")

	case err == posts.ErrNotAuthorized:
		writeError(w, http.StatusForbidden, "NotAuthorized",
			"You are not authorized to post in this community")

	case err == posts.ErrBanned:
		writeError(w, http.StatusForbidden, "Banned",
			"You are banned from this community")

	case posts.IsContentRuleViolation(err):
		writeError(w, http.StatusBadRequest, "ContentRuleViolation", err.Error())

	case posts.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())

	case posts.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())

	// Check aggregator authorization errors
	case aggregators.IsUnauthorized(err):
		writeError(w, http.StatusForbidden, "NotAuthorized",
			"Aggregator not authorized to post in this community")

	// Check both aggregator and post rate limit errors
	case aggregators.IsRateLimited(err) || err == posts.ErrRateLimitExceeded:
		writeError(w, http.StatusTooManyRequests, "RateLimitExceeded",
			"Rate limit exceeded. Please try again later.")

	default:
		// Don't leak internal error details to clients
		log.Printf("Unexpected error in post handler: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
