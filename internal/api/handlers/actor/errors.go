package actor

import (
	"fmt"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/posts"
)

// ErrorResponse represents an XRPC error response.
type ErrorResponse = xrpc.Error

// errorMapper maps actor service errors to XRPC responses.
//
// Every way an actor can be missing answers ActorNotFound: this package reads
// posts for a specific actor, so a missing post here means the actor is what we
// could not find.
var errorMapper = xrpc.NewMapper("actor",
	xrpc.As[*actorNotFoundError](http.StatusNotFound, "ActorNotFound",
		func(*actorNotFoundError) string { return "Actor not found" }),
	xrpc.Sentinel(posts.ErrNotFound, http.StatusNotFound,
		"ActorNotFound", "Actor not found"),
	xrpc.Sentinel(posts.ErrActorNotFound, http.StatusNotFound,
		"ActorNotFound", "Actor not found"),
	xrpc.Sentinel(posts.ErrCommunityNotFound, http.StatusNotFound,
		"CommunityNotFound", "Community not found"),
	xrpc.Sentinel(posts.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "Invalid pagination cursor"),
	// Message comes off the typed error rather than the chain, so context added
	// by a caller cannot leak into the response.
	xrpc.As[*posts.ValidationError](http.StatusBadRequest, "InvalidRequest",
		func(e *posts.ValidationError) string { return e.Message }),
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
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
