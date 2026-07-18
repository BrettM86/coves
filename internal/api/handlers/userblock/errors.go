package userblock

import (
	"Coves/internal/atproto/pds"
	"Coves/internal/core/userblocks"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// XRPCError represents an XRPC error response
type XRPCError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes an XRPC error response
func writeError(w http.ResponseWriter, status int, errCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(XRPCError{
		Error:   errCode,
		Message: message,
	}); err != nil {
		slog.Error("Failed to encode error response", "error", err)
	}
}

// handleServiceError converts user block service errors to appropriate HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, userblocks.ErrBlockNotFound):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())
	case errors.Is(err, userblocks.ErrBlockAlreadyExists):
		writeError(w, http.StatusConflict, "AlreadyExists", err.Error())
	case errors.Is(err, userblocks.ErrCannotBlockSelf):
		writeError(w, http.StatusBadRequest, "InvalidRequest", "cannot block yourself")
	// PDS-specific errors. 403 is a permissions problem (e.g. missing OAuth
	// scope), not an expired session — it must not trigger a client sign-out.
	case errors.Is(err, pds.ErrForbidden):
		writeError(w, http.StatusForbidden, "PermissionDenied", "Your session does not have permission for this action. Sign out and back in to grant it.")
	case errors.Is(err, pds.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required or session expired")
	case errors.Is(err, pds.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request to PDS")
	case errors.Is(err, pds.ErrNotFound):
		writeError(w, http.StatusNotFound, "NotFound", "Record not found on PDS")
	case errors.Is(err, pds.ErrConflict):
		writeError(w, http.StatusConflict, "Conflict", "Record was modified by another operation")
	case errors.Is(err, pds.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "RateLimitExceeded", "Too many requests, please try again later")
	case errors.Is(err, pds.ErrPayloadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Request payload exceeds size limit")
	default:
		slog.Error("XRPC user block handler error", "error", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
