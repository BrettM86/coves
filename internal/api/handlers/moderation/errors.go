package moderation

import (
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
