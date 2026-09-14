package discover

import (
	"errors"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/discover"
)

// errorMapper maps discover service errors to XRPC responses.
var errorMapper = xrpc.NewMapper("discover",
	xrpc.MatchDetail(discover.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
	xrpc.Sentinel(discover.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "The provided cursor is invalid"),
	xrpc.Sentinel(discover.ErrDiscoverUnavailable, http.StatusServiceUnavailable,
		"DiscoverUnavailable", "Discover is temporarily unavailable"),
)

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	if errors.Is(err, discover.ErrDiscoverUnavailable) {
		w.Header().Set("Retry-After", "30")
	}
	errorMapper.Write(w, err)
}
