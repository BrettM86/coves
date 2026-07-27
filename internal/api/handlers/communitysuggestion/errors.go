package communitysuggestion

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/communitysuggestions"
)

// errorMapper maps community suggestion service errors to XRPC responses.
//
// Every sentinel gets a static, user-facing message rather than echoing the
// error, so nothing internal reaches the client.
var errorMapper = xrpc.NewMapper("communitysuggestion",
	xrpc.Sentinel(communitysuggestions.ErrTitleRequired, http.StatusBadRequest,
		"InvalidRequest", "Suggestion title is required"),
	xrpc.Sentinel(communitysuggestions.ErrTitleTooLong, http.StatusBadRequest,
		"InvalidRequest", "Suggestion title exceeds maximum length"),
	xrpc.Sentinel(communitysuggestions.ErrDescriptionRequired, http.StatusBadRequest,
		"InvalidRequest", "Suggestion description is required"),
	xrpc.Sentinel(communitysuggestions.ErrDescriptionTooLong, http.StatusBadRequest,
		"InvalidRequest", "Suggestion description exceeds maximum length"),
	xrpc.Sentinel(communitysuggestions.ErrInvalidStatus, http.StatusBadRequest,
		"InvalidStatus", "Invalid status value. Must be one of: open, under_review, approved, declined"),
	xrpc.Sentinel(communitysuggestions.ErrInvalidVoteValue, http.StatusBadRequest,
		"InvalidVoteValue", "Invalid vote value. Must be 1 or -1"),
	xrpc.Sentinel(communitysuggestions.ErrInvalidSuggestionID, http.StatusBadRequest,
		"InvalidRequest", "Invalid suggestion ID"),
	xrpc.Sentinel(communitysuggestions.ErrVoterRequired, http.StatusBadRequest,
		"InvalidRequest", "Voter identification is required"),
	xrpc.Sentinel(communitysuggestions.ErrSubmitterRequired, http.StatusBadRequest,
		"InvalidRequest", "Submitter identification is required"),

	xrpc.Match(communitysuggestions.IsNotFound, http.StatusNotFound,
		"NotFound", "The requested resource was not found"),
	xrpc.Match(communitysuggestions.IsRateLimitError, http.StatusTooManyRequests,
		"RateLimitExceeded", "Too many suggestions. Please try again later"),
	xrpc.Match(communitysuggestions.IsAuthorizationError, http.StatusForbidden,
		"Forbidden", "You are not authorized to perform this action"),

	// Catch-all so a validation sentinel added to the domain without a rule
	// above still answers 400 rather than 500.
	xrpc.Match(communitysuggestions.IsValidationError, http.StatusBadRequest,
		"InvalidRequest", "The request contains invalid data"),
)

// writeError writes an XRPC error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	xrpc.WriteError(w, status, code, message)
}

// handleServiceError converts service errors to appropriate HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
