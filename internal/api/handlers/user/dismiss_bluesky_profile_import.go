package user

import (
	"net/http"

	"Coves/internal/api/middleware"
)

// DismissBlueskyProfileImportHandler handles social.coves.actor.dismissBlueskyProfileImport.
// It records that the authenticated user no longer wants the automatic
// Bluesky profile import offer. Idempotent: an existing dismissal is kept.
type DismissBlueskyProfileImportHandler struct {
	dismisser BlueskyImportDismisser
}

// NewDismissBlueskyProfileImportHandler creates the dismiss handler.
// Panics if dismisser is nil.
func NewDismissBlueskyProfileImportHandler(dismisser BlueskyImportDismisser) *DismissBlueskyProfileImportHandler {
	if dismisser == nil {
		panic("NewDismissBlueskyProfileImportHandler: dismisser is required")
	}
	return &DismissBlueskyProfileImportHandler{dismisser: dismisser}
}

// ServeHTTP handles POST /xrpc/social.coves.actor.dismissBlueskyProfileImport.
// It takes no input and answers 200 with an empty body.
func (h *DismissBlueskyProfileImportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// SECURITY: the DID comes from the authenticated session, never the request.
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeJSONError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// A client that goes away after asking must not lose the dismissal.
	ctx, cancel := detachedWriteContext(r.Context())
	defer cancel()
	if err := h.dismisser.DismissBlueskyProfileImport(ctx, userDID); err != nil {
		handleServiceError(w, err, userDID, "Bluesky profile import dismissal")
		return
	}

	w.WriteHeader(http.StatusOK)
}
