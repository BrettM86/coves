package actor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
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

// resolveActorDID converts the `actor` parameter (DID, handle, or a bridged
// user's name@host) to a DID through users.ResolveActorIdentifierToDID.
//
// The returned errors are what the handlers classify: ErrAmbiguousUser stays
// in the chain (409 AmbiguousActor), a missing user is *actorNotFoundError
// (404), a malformed identifier is *validationError (400), and anything else
// is *resolutionFailedError so an outage is not reported as "not found".
func resolveActorDID(ctx context.Context, userService users.UserService, actor string) (string, error) {
	// If it's already a DID, return it
	if strings.HasPrefix(actor, "did:") {
		return actor, nil
	}

	did, err := userService.ResolveActorIdentifierToDID(ctx, actor)
	if err == nil {
		return did, nil
	}

	// Check for context errors (timeouts, cancellation) - these are infrastructure
	// errors. The handlers log every *resolutionFailedError, so nothing is logged here.
	if ctx.Err() != nil {
		return "", &resolutionFailedError{actor: actor, cause: ctx.Err()}
	}

	var invalidHandle *users.InvalidHandleError
	switch {
	case errors.Is(err, users.ErrAmbiguousUser):
		return "", err
	case errors.Is(err, users.ErrUserNotFound):
		return "", &actorNotFoundError{actor: actor}
	case errors.As(err, &invalidHandle):
		return "", &validationError{field: "actor", message: "actor is not a valid handle, DID, or name@host"}
	}

	// Anything else (database, network, DNS failure) is an infrastructure error,
	// so the client sees a 500 rather than "actor not found". The error text is
	// never inspected: it embeds the caller-supplied identifier.
	return "", &resolutionFailedError{actor: actor, cause: err}
}
