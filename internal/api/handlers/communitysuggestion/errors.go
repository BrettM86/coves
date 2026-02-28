package communitysuggestion

import (
	"Coves/internal/api/xrpc"
	"Coves/internal/core/communitysuggestions"
	"errors"
	"log"
	"net/http"
)

// writeError writes an XRPC error response
func writeError(w http.ResponseWriter, status int, error, message string) {
	xrpc.WriteError(w, status, error, message)
}

// handleServiceError converts service errors to appropriate HTTP responses
// Each sentinel error is mapped to a static, user-facing message to prevent
// leaking internal error details to clients.
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case communitysuggestions.IsValidationError(err):
		switch {
		case errors.Is(err, communitysuggestions.ErrTitleRequired):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Suggestion title is required")
		case errors.Is(err, communitysuggestions.ErrTitleTooLong):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Suggestion title exceeds maximum length")
		case errors.Is(err, communitysuggestions.ErrDescriptionRequired):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Suggestion description is required")
		case errors.Is(err, communitysuggestions.ErrDescriptionTooLong):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Suggestion description exceeds maximum length")
		case errors.Is(err, communitysuggestions.ErrInvalidStatus):
			writeError(w, http.StatusBadRequest, "InvalidStatus", "Invalid status value. Must be one of: open, under_review, approved, declined")
		case errors.Is(err, communitysuggestions.ErrInvalidVoteValue):
			writeError(w, http.StatusBadRequest, "InvalidVoteValue", "Invalid vote value. Must be 1 or -1")
		case errors.Is(err, communitysuggestions.ErrInvalidSuggestionID):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid suggestion ID")
		case errors.Is(err, communitysuggestions.ErrVoterRequired):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Voter identification is required")
		case errors.Is(err, communitysuggestions.ErrSubmitterRequired):
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Submitter identification is required")
		default:
			log.Printf("Unhandled validation error in community suggestion handler: %v", err)
			writeError(w, http.StatusBadRequest, "InvalidRequest", "The request contains invalid data")
		}
	case communitysuggestions.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", "The requested resource was not found")
	case communitysuggestions.IsRateLimitError(err):
		writeError(w, http.StatusTooManyRequests, "RateLimitExceeded", "Too many suggestions. Please try again later")
	case communitysuggestions.IsAuthorizationError(err):
		writeError(w, http.StatusForbidden, "Forbidden", "You are not authorized to perform this action")
	default:
		log.Printf("XRPC community suggestion handler error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
