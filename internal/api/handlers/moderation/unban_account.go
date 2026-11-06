package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// UnbanAccountHandler serves social.coves.moderation.unbanAccount.
type UnbanAccountHandler struct {
	service moderation.Service
}

// NewUnbanAccountHandler builds the unbanAccount handler.
func NewUnbanAccountHandler(service moderation.Service) *UnbanAccountHandler {
	return &UnbanAccountHandler{service: service}
}

// HandleUnbanAccount handles POST /xrpc/social.coves.moderation.unbanAccount.
func (h *UnbanAccountHandler) HandleUnbanAccount(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         string `json:"subject"`
		ExpectedVersion string `json:"expectedVersion"`
		IdempotencyKey  string `json:"idempotencyKey"`
		Reason          string `json:"reason"`
		PrivateNote     string `json:"privateNote"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	result, err := h.service.UnbanAccount(r.Context(), actorDID, moderation.UnbanAccountRequest{
		Subject: input.Subject, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	})
	writeMutationResult(w, "unbanAccount", result, err)
}
