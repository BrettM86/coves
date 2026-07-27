package userblock

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/userblocks"
)

// errorMapper maps user block service errors to XRPC responses.
//
// This package was the only one that mapped the full set of PDS errors by
// hand; those rules now live in xrpc and every handler package gets them.
var errorMapper = xrpc.NewMapper("userblock",
	xrpc.SentinelDetail(userblocks.ErrBlockNotFound, http.StatusNotFound, "NotFound"),
	xrpc.SentinelDetail(userblocks.ErrBlockAlreadyExists, http.StatusConflict, "AlreadyExists"),
	xrpc.Sentinel(userblocks.ErrCannotBlockSelf, http.StatusBadRequest,
		"InvalidRequest", "cannot block yourself"),
)

// writeError writes an XRPC error response.
func writeError(w http.ResponseWriter, status int, errCode, message string) {
	xrpc.WriteError(w, status, errCode, message)
}

// handleServiceError converts user block service errors to appropriate HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
