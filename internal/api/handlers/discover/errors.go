package discover

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/discover"
)

// errorMapper maps discover service errors to XRPC responses.
var errorMapper = xrpc.NewMapper("discover",
	xrpc.MatchDetail(discover.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
	xrpc.Sentinel(discover.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "The provided cursor is invalid"),
)

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
