package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// RemoveCommunityHandler serves social.coves.moderation.removeCommunity.
type RemoveCommunityHandler struct {
	service moderation.Service
}

// NewRemoveCommunityHandler builds the removeCommunity handler.
func NewRemoveCommunityHandler(service moderation.Service) *RemoveCommunityHandler {
	return &RemoveCommunityHandler{service: service}
}

// HandleRemoveCommunity handles POST /xrpc/social.coves.moderation.removeCommunity.
func (h *RemoveCommunityHandler) HandleRemoveCommunity(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.service.RemoveCommunity(r.Context(), actorDID, moderation.RemoveCommunityRequest{
		Subject: input.Subject, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	})
	writeMutationResult(w, "removeCommunity", result, err)
}
