package post

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/posts"
)

// errorMapper maps post service errors to XRPC responses.
//
// Only the post-specific rules live here; dead sessions, other PDS failures,
// shared typed domain errors, and request-lifecycle errors come from xrpc's
// shared rules. Rules are tried in order, so the specific sentinels come before
// the broad predicates that also match them.
var errorMapper = xrpc.NewMapper("post",
	// Ahead of posts.IsNotFound, which matches this sentinel too but answers
	// with the less useful generic code.
	xrpc.Sentinel(posts.ErrCommunityNotFound, http.StatusNotFound,
		"CommunityNotFound", "Community not found"),
	xrpc.Sentinel(posts.ErrNotAuthorized, http.StatusForbidden,
		"NotAuthorized", "You are not authorized to post in this community"),
	xrpc.Sentinel(posts.ErrBanned, http.StatusForbidden,
		"Banned", "You are banned from this community"),

	// A content rule violation names the rule the post broke, which is the
	// whole point of returning it — the client shows it to the author.
	xrpc.As[*posts.ContentRuleViolation](http.StatusBadRequest, "ContentRuleViolation",
		func(e *posts.ContentRuleViolation) string { return e.Error() }),

	xrpc.Sentinel(posts.ErrNotFound, http.StatusNotFound,
		"NotFound", "Post not found"),

	// A submission refused at the admission gate, which is NOT the generic
	// AlreadyExists that a storage conflict produces: 409 DuplicateSubmission
	// tells a client whose response was lost that its post already exists and
	// it should stop resending, where a 429 would have it retry on a timer
	// forever. Ahead of the shared ConflictError rule, which answers with the
	// less specific code.
	xrpc.Sentinel(posts.ErrDuplicateSubmission, http.StatusConflict,
		"DuplicateSubmission", "You have already submitted this post to this community"),

	xrpc.Match(aggregators.IsUnauthorized, http.StatusForbidden,
		"NotAuthorized", "Aggregator not authorized to post in this community"),
	xrpc.Match(aggregators.IsRateLimited, http.StatusTooManyRequests,
		"RateLimitExceeded", "Rate limit exceeded. Please try again later."),
	xrpc.Sentinel(posts.ErrRateLimitExceeded, http.StatusTooManyRequests,
		"RateLimitExceeded", "Rate limit exceeded. Please try again later."),
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
