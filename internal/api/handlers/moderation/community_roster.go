package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// CommunityRosterActions runs community roster actions.
type CommunityRosterActions interface {
	ResignRole(ctx context.Context, req moderation.ResignRoleRequest) (*moderation.CommunityActionResult, error)
	RemoveModerator(ctx context.Context, req moderation.RemoveModeratorRequest) (*moderation.CommunityActionResult, error)
}

// CommunityRosterHandler serves social.coves.community.resignModerator and
// social.coves.community.removeModerator.
type CommunityRosterHandler struct {
	service CommunityRosterActions
}

// NewCommunityRosterHandler builds the handler over the service.
func NewCommunityRosterHandler(service CommunityRosterActions) *CommunityRosterHandler {
	return &CommunityRosterHandler{service: service}
}

// communityRosterOutput is the output of both roster procedures.
type communityRosterOutput struct {
	Community string `json:"community"`
	Subject   string `json:"subject"`
	Version   int64  `json:"version"`
}

// HandleResign serves social.coves.community.resignModerator: the
// authenticated caller resigns its own moderator role. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the caller DID.
func (h *CommunityRosterHandler) HandleResign(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Community       string `json:"community"`
		ExpectedVersion *int64 `json:"expectedVersion"`
		IdempotencyKey  string `json:"idempotencyKey"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	if input.ExpectedVersion == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "expectedVersion is required")
		return
	}
	result, err := h.service.ResignRole(r.Context(), moderation.ResignRoleRequest{
		ActorDID: actorDID, CommunityDID: input.Community,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityRosterResult(w, "resignModerator", result, err)
}

// HandleRemove serves social.coves.community.removeModerator: the
// authenticated caller, the community's owner or an instance admin, removes a
// moderator. It must run behind InstanceAdminMiddleware.RequireAuthenticated.
func (h *CommunityRosterHandler) HandleRemove(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Community       string `json:"community"`
		Subject         string `json:"subject"`
		ExpectedVersion *int64 `json:"expectedVersion"`
		IdempotencyKey  string `json:"idempotencyKey"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	if input.ExpectedVersion == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "expectedVersion is required")
		return
	}
	result, err := h.service.RemoveModerator(r.Context(), moderation.RemoveModeratorRequest{
		ActorDID: actorDID, CommunityDID: input.Community, SubjectDID: input.Subject,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityRosterResult(w, "removeModerator", result, err)
}

func writeCommunityRosterResult(w http.ResponseWriter, procedure string, result *moderation.CommunityActionResult, err error) {
	if err == nil && result == nil {
		err = errors.New("community roster action returned no result")
	}
	if err != nil {
		writeCommunityRosterError(w, procedure, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, communityRosterOutput{
		Community: result.CommunityDID, Subject: result.SubjectDID, Version: result.Version,
	})
}

func writeCommunityRosterError(w http.ResponseWriter, procedure string, err error) {
	switch {
	case errors.Is(err, moderation.ErrAuthRequired):
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
	case errors.Is(err, moderation.ErrForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not permitted to change this community's moderators")
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid community, subject, expectedVersion or idempotencyKey")
	case errors.Is(err, moderation.ErrStateConflict):
		xrpc.WriteError(w, http.StatusConflict, "StateConflict", "The community's moderators changed; reload and retry")
	case errors.Is(err, moderation.ErrIdempotencyConflict):
		xrpc.WriteError(w, http.StatusConflict, "IdempotencyConflict", "Idempotency key was used for a different request")
	case errors.Is(err, moderation.ErrOwnerMustTransfer):
		xrpc.WriteError(w, http.StatusBadRequest, "OwnerMustTransfer", "The owner must transfer ownership before resigning")
	case errors.Is(err, moderation.ErrCannotRemoveOwner):
		xrpc.WriteError(w, http.StatusBadRequest, "CannotRemoveOwner", "The community's owner cannot be removed")
	case errors.Is(err, moderation.ErrRoleNotFound):
		xrpc.WriteError(w, http.StatusNotFound, "RoleNotFound", "The account holds no moderator role in this community")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("community roster action unavailable", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	default:
		// DIDs, record URIs and database errors carry no credentials, so the
		// full error is logged; the response body stays generic.
		slog.Error("unexpected community roster action failure", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
