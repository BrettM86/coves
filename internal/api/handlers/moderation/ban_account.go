package moderation

import (
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// BanAccountHandler serves social.coves.moderation.banAccount.
type BanAccountHandler struct {
	service moderation.Service
}

// NewBanAccountHandler builds the banAccount handler.
func NewBanAccountHandler(service moderation.Service) *BanAccountHandler {
	return &BanAccountHandler{service: service}
}

// HandleBanAccount handles POST /xrpc/social.coves.moderation.banAccount.
func (h *BanAccountHandler) HandleBanAccount(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         string  `json:"subject"`
		ExpectedVersion string  `json:"expectedVersion"`
		IdempotencyKey  string  `json:"idempotencyKey"`
		Reason          string  `json:"reason"`
		PrivateNote     string  `json:"privateNote"`
		ExpiresAt       *string `json:"expiresAt"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		parsed, err := syntax.ParseDatetime(*input.ExpiresAt)
		if err != nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "expiresAt must be a datetime")
			return
		}
		instant := parsed.Time()
		expiresAt = &instant
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	result, err := h.service.BanAccount(r.Context(), actorDID, moderation.BanAccountRequest{
		Subject: input.Subject, ExpectedVersion: input.ExpectedVersion,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, PrivateNote: input.PrivateNote,
		ExpiresAt: expiresAt,
	})
	writeMutationResult(w, "banAccount", result, err)
}
