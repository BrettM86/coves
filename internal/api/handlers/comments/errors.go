package comments

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/comments"
)

// errorMapper maps comment service errors to XRPC responses.
//
// comments signals validation and not-found with sentinels rather than the
// shared typed errors, so unlike most packages it spells out both groups here.
// Everything else — dead sessions, other PDS failures, request lifecycle —
// comes from xrpc's shared rules.
var errorMapper = xrpc.NewMapper("comments",
	// Not found.
	xrpc.Sentinel(comments.ErrCommentNotFound, http.StatusNotFound,
		"CommentNotFound", "Comment not found"),
	xrpc.Sentinel(comments.ErrParentNotFound, http.StatusNotFound,
		"ParentNotFound", "Parent post or comment not found"),
	xrpc.Sentinel(comments.ErrRootNotFound, http.StatusNotFound,
		"RootNotFound", "Root post not found"),

	// Validation.
	xrpc.Sentinel(comments.ErrInvalidReply, http.StatusBadRequest,
		"InvalidReply", "The reply reference is invalid or malformed"),
	xrpc.Sentinel(comments.ErrContentTooLong, http.StatusBadRequest,
		"ContentTooLong", "Comment content exceeds 10000 graphemes"),
	xrpc.Sentinel(comments.ErrContentEmpty, http.StatusBadRequest,
		"ContentEmpty", "Comment content is required"),
	// Ahead of InvalidFacets so an #image facet that breaks a placement rule is
	// its own 400. The message names the facet and the rule it breaks, which is
	// client-actionable and carries no internal state.
	xrpc.SentinelDetail(comments.ErrInvalidImageFacet, http.StatusBadRequest,
		"InvalidImageFacet"),
	// The error names which facet and which field, which is client-actionable
	// and carries no internal state.
	xrpc.SentinelDetail(comments.ErrInvalidFacets, http.StatusBadRequest,
		"InvalidFacets"),
	// The message names the failing part of the embed (non-object, $type
	// missing or not allowed on a comment, an image that is not a well-formed
	// image blob ref, or the lexicon failure).
	xrpc.SentinelDetail(comments.ErrInvalidEmbed, http.StatusBadRequest,
		"InvalidEmbed"),
	// Fixed message: the error repeats the embedimages cause's text.
	xrpc.Sentinel(comments.ErrTooManyImages, http.StatusBadRequest,
		"TooManyImages", "The comment's embed carries more than 8 images"),
	// The lexicon failure of the record about to be written, outside its embed.
	xrpc.SentinelDetail(comments.ErrInvalidRecord, http.StatusBadRequest,
		"InvalidRequest"),
	// Fixed message: the repository layer wraps this one with detail the client
	// has no use for.
	xrpc.Sentinel(comments.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidRequest", "Invalid or mismatched pagination cursor"),

	// A concurrent edit lost the optimistic-locking race on PutRecord. This
	// used to be absent, on the theory that the PDS never surfaced a conflict —
	// it does, and the omission turned every lost race into a 500.
	xrpc.Sentinel(comments.ErrConcurrentModification, http.StatusConflict,
		"ConcurrentModification", "The comment was modified by another request. Fetch it again and retry."),

	// Authorization.
	xrpc.Sentinel(comments.ErrNotAuthorized, http.StatusForbidden,
		"NotAuthorized", "User is not authorized to perform this action"),
	xrpc.Sentinel(comments.ErrBanned, http.StatusForbidden,
		"Banned", "User is banned from this community"),

	// Catch-alls for sentinels added to the domain without a rule here: a new
	// not-found still answers 404 rather than 500.
	xrpc.Match(comments.IsNotFound, http.StatusNotFound,
		"NotFound", "The requested resource was not found"),
	xrpc.Match(comments.IsValidationError, http.StatusBadRequest,
		"InvalidRequest", "The request contains invalid data"),
)

// writeError writes a JSON error response with the given status code.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service-layer errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
