package community

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/communities"
)

// errorMapper maps community service errors to XRPC responses.
//
// The PDS rules this package used to spell out by hand now come from xrpc's
// shared rules, along with dead sessions and request-lifecycle errors. That
// also closed two gaps: a rate-limited or oversized write to the PDS answered
// 500 here, because those two sentinels were the ones the hand-written switch
// happened to omit.
var errorMapper = xrpc.NewMapper("community",
	// Ahead of the generic conflict rule, which matches this sentinel too.
	xrpc.Sentinel(communities.ErrHandleTaken, http.StatusConflict,
		"NameTaken", "Community handle is already taken"),

	xrpc.Sentinel(communities.ErrUnauthorized, http.StatusForbidden,
		"Forbidden", "You do not have permission to perform this action"),
	xrpc.Sentinel(communities.ErrMemberBanned, http.StatusForbidden,
		"Blocked", "You are blocked from this community"),
	// Ahead of the not-found and validation rules: a name@origin that matches
	// more than one community is neither missing nor malformed, and the client
	// needs to be told to address it by DID or handle instead.
	xrpc.Sentinel(communities.ErrAmbiguousCommunity, http.StatusConflict,
		"AmbiguousCommunity", "More than one community matches this name and origin; address it by DID or handle instead"),

	// The domain's own predicates. Their sentinel text is written for the
	// client, so it doubles as the message.
	xrpc.MatchDetail(communities.IsNotFound, http.StatusNotFound, "NotFound"),
	xrpc.MatchDetail(communities.IsConflict, http.StatusConflict, "AlreadyExists"),
	xrpc.MatchDetail(communities.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
)

// writeError writes an XRPC error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	xrpc.WriteError(w, status, code, message)
}

// handleServiceError converts service errors to appropriate HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
