package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// LabelContentHandler serves social.coves.moderation.labelContent.
type LabelContentHandler struct {
	service moderation.Service
}

// NewLabelContentHandler builds the labelContent handler.
func NewLabelContentHandler(service moderation.Service) *LabelContentHandler {
	return &LabelContentHandler{service: service}
}

// HandleLabelContent handles POST /xrpc/social.coves.moderation.labelContent.
func (h *LabelContentHandler) HandleLabelContent(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         strongRefView `json:"subject"`
		LabelValue      string        `json:"labelValue"`
		ExpectedVersion string        `json:"expectedVersion"`
		IdempotencyKey  string        `json:"idempotencyKey"`
		Reason          string        `json:"reason"`
		PrivateNote     string        `json:"privateNote"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	result, err := h.service.LabelContent(r.Context(), actorDID, moderation.LabelContentRequest{
		Subject:    moderation.StrongRef{URI: input.Subject.URI, CID: input.Subject.CID},
		LabelValue: input.LabelValue, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	})
	writeMutationResult(w, "labelContent", result, err)
}
