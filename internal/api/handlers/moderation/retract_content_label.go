package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// RetractContentLabelHandler serves social.coves.moderation.retractContentLabel.
type RetractContentLabelHandler struct {
	service moderation.Service
}

// NewRetractContentLabelHandler builds the retractContentLabel handler.
func NewRetractContentLabelHandler(service moderation.Service) *RetractContentLabelHandler {
	return &RetractContentLabelHandler{service: service}
}

// HandleRetractContentLabel handles POST /xrpc/social.coves.moderation.retractContentLabel.
func (h *RetractContentLabelHandler) HandleRetractContentLabel(w http.ResponseWriter, r *http.Request) {
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
	request := moderation.RetractContentLabelRequest{
		ActionID: input.ActionID, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	}
	if input.ReviewedSubject != nil {
		request.ReviewedSubject = &moderation.StrongRef{URI: input.ReviewedSubject.URI, CID: input.ReviewedSubject.CID}
	}
	result, err := h.service.RetractContentLabel(r.Context(), actorDID, request)
	writeMutationResult(w, "retractContentLabel", result, err)
}
