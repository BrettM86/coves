package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

func writeSubjectStateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, moderation.ErrInvalidSubject):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidSubject", "Invalid subject URI")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("moderation subject state unavailable", "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation state temporarily unavailable")
	default:
		// Subject AT-URIs, DIDs and database errors carry no credentials, so the
		// full error is logged; the response body stays generic.
		slog.Error("unexpected moderation subject state failure", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}

func writeActionListError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid action list parameters")
	case errors.Is(err, moderation.ErrInvalidCursor):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidCursor", "Invalid action list cursor")
	case errors.Is(err, context.Canceled):
		// A client that went away is not a failure of ours and is not logged.
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrResolverUnavailable):
		// A filter target resolver, not the moderation store, is unavailable.
		slog.Warn("moderation action list resolver unavailable", "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("moderation action list unavailable", "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	default:
		slog.Error("unexpected moderation action list failure", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
