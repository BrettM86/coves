package communityFeed

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/communityFeeds"
)

// errorMapper maps community feed service errors to XRPC responses.
var errorMapper = xrpc.NewMapper("communityFeed",
	xrpc.Sentinel(communityFeeds.ErrCommunityNotFound, http.StatusNotFound,
		"CommunityNotFound", "Community not found"),
	xrpc.Sentinel(communityFeeds.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "Invalid pagination cursor"),
	xrpc.MatchDetail(communityFeeds.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
