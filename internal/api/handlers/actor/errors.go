package actor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"Coves/internal/core/posts"
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
	// Check for handler-level errors first
	var actorNotFound *actorNotFoundError
	if errors.As(err, &actorNotFound) {
		writeError(w, http.StatusNotFound, "ActorNotFound", "Actor not found")
		return
	}

	// Check for service-level errors
	switch {
	case errors.Is(err, posts.ErrNotFound):
		writeError(w, http.StatusNotFound, "ActorNotFound", "Actor not found")

	case errors.Is(err, posts.ErrActorNotFound):
		writeError(w, http.StatusNotFound, "ActorNotFound", "Actor not found")

	case errors.Is(err, posts.ErrCommunityNotFound):
		writeError(w, http.StatusNotFound, "CommunityNotFound", "Community not found")

	case errors.Is(err, posts.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "InvalidCursor", "Invalid pagination cursor")

	case posts.IsValidationError(err):
		// Extract message from ValidationError for cleaner response
		var valErr *posts.ValidationError
		if errors.As(err, &valErr) {
			writeError(w, http.StatusBadRequest, "InvalidRequest", valErr.Message)
		} else {
			writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		}

	default:
		// Internal server error - don't leak details
		log.Printf("ERROR: Actor posts service error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}

// actorNotFoundError represents an actor not found error
type actorNotFoundError struct {
	actor string
}

func (e *actorNotFoundError) Error() string {
	return fmt.Sprintf("actor not found: %s", e.actor)
}

// resolutionFailedError represents an infrastructure failure during resolution
// (database down, DNS failures, TLS errors, etc.)
// This is distinct from actorNotFoundError to avoid masking real problems as "not found"
type resolutionFailedError struct {
	actor string
	cause error
}

func (e *resolutionFailedError) Error() string {
	return fmt.Sprintf("failed to resolve actor %s: %v", e.actor, e.cause)
}

func (e *resolutionFailedError) Unwrap() error {
	return e.cause
}
