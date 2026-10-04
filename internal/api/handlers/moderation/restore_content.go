package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// RestoreContentHandler serves social.coves.moderation.restoreContent.
type RestoreContentHandler struct {
	service moderation.Service
}

// NewRestoreContentHandler builds the restoreContent handler.
func NewRestoreContentHandler(service moderation.Service) *RestoreContentHandler {
	return &RestoreContentHandler{service: service}
}

// HandleRestoreContent handles POST /xrpc/social.coves.moderation.restoreContent.
func (h *RestoreContentHandler) HandleRestoreContent(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		ActionID        string         `json:"actionId"`
		ReviewedSubject *strongRefView `json:"reviewedSubject"`
		ExpectedVersion string         `json:"expectedVersion"`
		IdempotencyKey  string         `json:"idempotencyKey"`
		Reason          string         `json:"reason"`
		PrivateNote     string         `json:"privateNote"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	request := moderation.RestoreContentRequest{
		ActionID: input.ActionID, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	}
	if input.ReviewedSubject != nil {
		request.ReviewedSubject = &moderation.StrongRef{URI: input.ReviewedSubject.URI, CID: input.ReviewedSubject.CID}
	}
	result, err := h.service.RestoreContent(r.Context(), actorDID, request)
	writeMutationResult(w, "restoreContent", result, err)
}
