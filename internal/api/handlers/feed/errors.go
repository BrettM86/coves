package feed

import (
	"errors"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/discover"
	coreerrors "Coves/internal/core/errors"
	"Coves/internal/core/feedgen"
)

// errorMapper maps getFeed service errors to XRPC responses.
var errorMapper = xrpc.NewMapper("feed",
	xrpc.Sentinel(feedgen.ErrUnknownFeed, http.StatusBadRequest,
		"UnknownFeed", "The requested feed is not a known feed generator"),
	xrpc.Sentinel(feedgen.ErrUnsupportedFeedScope, http.StatusBadRequest,
		"UnsupportedFeedScope", "The feed cannot be used in the requested scope"),
	xrpc.Sentinel(feedgen.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "Invalid pagination cursor"),
	xrpc.Sentinel(discover.ErrDiscoverUnavailable, http.StatusServiceUnavailable,
		"DiscoverUnavailable", "Discover is temporarily unavailable"),
	xrpc.MatchDetail(coreerrors.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, errorType, message string) {
	xrpc.WriteError(w, status, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	if errors.Is(err, discover.ErrDiscoverUnavailable) {
		w.Header().Set("Retry-After", "30")
	}
	errorMapper.Write(w, err)
}
