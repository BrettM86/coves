package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// RestoreCommunityHandler serves social.coves.moderation.restoreCommunity.
type RestoreCommunityHandler struct {
	service moderation.Service
}

// NewRestoreCommunityHandler builds the restoreCommunity handler.
func NewRestoreCommunityHandler(service moderation.Service) *RestoreCommunityHandler {
	return &RestoreCommunityHandler{service: service}
}

// HandleRestoreCommunity handles POST /xrpc/social.coves.moderation.restoreCommunity.
func (h *RestoreCommunityHandler) HandleRestoreCommunity(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.service.RestoreCommunity(r.Context(), actorDID, moderation.RestoreCommunityRequest{
		Subject: input.Subject, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
	})
	writeMutationResult(w, "restoreCommunity", result, err)
}
