package vote

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/votes"
)

// XRPCError represents an XRPC error response.
type XRPCError = xrpc.Error

// errorMapper maps vote service errors to XRPC responses.
//
// Only the vote-specific rules live here. Dead sessions, other PDS failures,
// shared typed domain errors, and request-lifecycle errors come from xrpc's
// shared rules, so this package cannot fall behind on them the way a
// hand-written switch did.
//
// Error names are part of the client contract: keep them UpperCamelCase and
// stable.
var errorMapper = xrpc.NewMapper("vote",
	// Matches: social.coves.feed.vote.delete#VoteNotFound
	xrpc.Sentinel(votes.ErrVoteNotFound, http.StatusNotFound,
		"VoteNotFound", "No vote found for this subject"),
	xrpc.Sentinel(votes.ErrInvalidDirection, http.StatusBadRequest,
		"InvalidRequest", "Vote direction must be 'up' or 'down'"),
	// Matches: social.coves.feed.vote.create#InvalidSubject
	xrpc.Sentinel(votes.ErrInvalidSubject, http.StatusBadRequest,
		"InvalidSubject", "The subject reference is invalid or malformed"),
	xrpc.Sentinel(votes.ErrVoteAlreadyExists, http.StatusConflict,
		"AlreadyExists", "Vote already exists"),
	// Matches: social.coves.feed.vote.create#NotAuthorized,
	// social.coves.feed.vote.delete#NotAuthorized
	xrpc.Sentinel(votes.ErrNotAuthorized, http.StatusForbidden,
		"NotAuthorized", "User is not authorized to vote on this content"),
	xrpc.Sentinel(votes.ErrBanned, http.StatusForbidden,
		"NotAuthorized", "User is not authorized to vote on this content"),
)

// writeError writes an XRPC error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	xrpc.WriteError(w, status, code, message)
}

// handleServiceError converts service errors to appropriate HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
