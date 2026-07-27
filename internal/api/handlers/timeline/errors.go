package timeline

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/timeline"
)

// errorMapper maps timeline service errors to XRPC responses.
//
// Rule order matches the switch this replaced: a cursor problem reported as a
// typed validation error answers InvalidRequest, and only a bare
// ErrInvalidCursor answers InvalidCursor.
var errorMapper = xrpc.NewMapper("timeline",
	xrpc.MatchDetail(timeline.IsValidationError, http.StatusBadRequest, "InvalidRequest"),
	xrpc.Sentinel(timeline.ErrInvalidCursor, http.StatusBadRequest,
		"InvalidCursor", "The provided cursor is invalid"),
	xrpc.Sentinel(timeline.ErrUnauthorized, http.StatusUnauthorized,
		"AuthenticationRequired", "User must be authenticated"),
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, errorType, message string) {
	xrpc.WriteError(w, status, errorType, message)
}

// handleServiceError maps service errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
