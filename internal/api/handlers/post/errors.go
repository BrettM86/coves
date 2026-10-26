package post

import (
	"errors"
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

	// An embed the record lexicon or the embed write rules refuse is its own
	// 400, so a client can point the author at the attachment. It is also a
	// ValidationError, so it must come before the shared InvalidRequest rule.
	invalidEmbedRule,

	// More images than an embed may carry is its own 400, so a client can tell
	// the author to remove some rather than fix an attachment. It is also a
	// ValidationError, so it too must come before the shared InvalidRequest
	// rule.
	tooManyImagesRule,

	// An #image facet that breaks a placement rule is its own 400, so a client
	// can point the author at the misplaced image. It is also a ValidationError,
	// so it must come before the shared InvalidRequest rule.
	invalidImageFacetRule,

	// The AppView holds nothing to authenticate as the author with. It is NOT
	// the shared re-auth rule's 401: that one means the CALLER's session is
	// dead and signing in again fixes it, which is true for a person and false
	// for an aggregator posting on stored tokens — nobody is at the keyboard to
	// sign it in, and telling its operator to do so would hide a revoked grant
	// behind a message aimed at a browser. 503 says what is true of both: the
	// service cannot write on this author's behalf right now.
	xrpc.Sentinel(posts.ErrNoAuthorCredentials, http.StatusServiceUnavailable,
		"NoAuthorCredentials", "No credentials are available to write to your repository"),

	// A lost swap guard on an edit. 409 rather than a retry here, because the
	// edit was composed against content that no longer stands: re-reading and
	// re-applying it server-side would silently erase the change its author
	// never saw. The client re-reads and decides.
	xrpc.Sentinel(posts.ErrConcurrentModification, http.StatusConflict,
		"ConcurrentModification", "The post was modified by another edit; re-read it and try again"),

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

// invalidEmbedRule answers posts.ErrInvalidEmbed with 400 InvalidEmbed.
var invalidEmbedRule = validationRule(posts.ErrInvalidEmbed, "InvalidEmbed", "The post's embed is invalid")

// tooManyImagesRule answers posts.ErrTooManyImages with 400 TooManyImages.
var tooManyImagesRule = validationRule(posts.ErrTooManyImages, "TooManyImages", "The post's embed carries too many images")

// invalidImageFacetRule answers posts.ErrInvalidImageFacet with 400 InvalidImageFacet.
var invalidImageFacetRule = validationRule(posts.ErrInvalidImageFacet, "InvalidImageFacet", "An #image facet in the post is invalid")

// validationRule answers sentinel with 400 code. The message is the typed
// ValidationError's own text, so wrapper context added upstream cannot reach
// the client; without one it is fallback.
func validationRule(sentinel error, code, fallback string) xrpc.Rule {
	return func(err error) (xrpc.Mapping, bool) {
		if !errors.Is(err, sentinel) {
			return xrpc.Mapping{}, false
		}
		message := fallback
		var validationErr *posts.ValidationError
		if errors.As(err, &validationErr) {
			message = validationErr.Error()
		}
		return xrpc.Mapping{Status: http.StatusBadRequest, Code: code, Message: message}, true
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
