package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
)

// CommunityInvitationActions runs the community invitation procedures and
// queries. The invite, offer, decline and list methods are
// moderation.CommunityInvitationService's; AcceptInvitation is
// moderation.CommunityActionService's.
type CommunityInvitationActions interface {
	InviteModerator(ctx context.Context, req moderation.InviteRequest) (*moderation.CommunityActionResult, error)
	OfferOwnership(ctx context.Context, req moderation.InviteRequest) (*moderation.CommunityActionResult, error)
	AcceptInvitation(ctx context.Context, req moderation.AcceptInvitationRequest) (*moderation.CommunityActionResult, error)
	DeclineInvitation(ctx context.Context, req moderation.DeclineInvitationRequest) (*moderation.CommunityActionResult, error)
	ListCommunityInvitations(ctx context.Context, actorDID, communityDID string) ([]communities.Invitation, error)
	ListViewerInvitations(ctx context.Context, viewerDID string) ([]communities.Invitation, error)
	// Invitation reads one invitation by ID in any state, to build the
	// invitation view an invite or offer responds with.
	Invitation(ctx context.Context, invitationID string) (*communities.Invitation, error)
	// RosterVersion is the community's current roster version, which each
	// listViewerInvitations item carries.
	RosterVersion(ctx context.Context, communityDID string) (int64, error)
}

// CommunityInvitationHandler serves social.coves.community.inviteModerator,
// offerOwnership, acceptInvitation, declineInvitation, listInvitations and
// listViewerInvitations.
type CommunityInvitationHandler struct {
	service CommunityInvitationActions
}

// NewCommunityInvitationHandler builds the handler over the service.
func NewCommunityInvitationHandler(service CommunityInvitationActions) *CommunityInvitationHandler {
	return &CommunityInvitationHandler{service: service}
}

// invitationView is social.coves.community.defs#invitationView.
type invitationView struct {
	ID        string `json:"id"`
	Community string `json:"community"`
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	Inviter   string `json:"inviter"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
}

func newInvitationView(invitation communities.Invitation) invitationView {
	return invitationView{
		ID: invitation.ID, Community: invitation.CommunityDID, Subject: invitation.InviteeDID,
		Role: invitation.Role, Inviter: invitation.InviterDID,
		CreatedAt: invitation.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: invitation.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

type inviteOutput struct {
	Invitation invitationView `json:"invitation"`
	Version    int64          `json:"version"`
}

type acceptInvitationOutput struct {
	Community string `json:"community"`
	Subject   string `json:"subject"`
	Version   int64  `json:"version"`
}

type declineInvitationOutput struct {
	Invitation string `json:"invitation"`
}

type listInvitationsOutput struct {
	Invitations []invitationView `json:"invitations"`
}

type viewerInvitationView struct {
	Invitation    invitationView `json:"invitation"`
	RosterVersion int64          `json:"rosterVersion"`
}

type listViewerInvitationsOutput struct {
	Invitations []viewerInvitationView `json:"invitations"`
}

// inviteInput is the input of inviteModerator and offerOwnership.
type inviteInput struct {
	Community       string `json:"community"`
	Subject         string `json:"subject"`
	ExpectedVersion *int64 `json:"expectedVersion"`
	IdempotencyKey  string `json:"idempotencyKey"`
}

// invitationInput is the input of acceptInvitation and declineInvitation.
type invitationInput struct {
	Invitation      string `json:"invitation"`
	ExpectedVersion *int64 `json:"expectedVersion"`
	IdempotencyKey  string `json:"idempotencyKey"`
}

// HandleInviteModerator serves social.coves.community.inviteModerator. It
// and every other handler here must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the caller DID.
func (h *CommunityInvitationHandler) HandleInviteModerator(w http.ResponseWriter, r *http.Request) {
	h.handleInvite(w, r, "inviteModerator", h.service.InviteModerator)
}

// HandleOfferOwnership serves social.coves.community.offerOwnership.
func (h *CommunityInvitationHandler) HandleOfferOwnership(w http.ResponseWriter, r *http.Request) {
	h.handleInvite(w, r, "offerOwnership", h.service.OfferOwnership)
}

func (h *CommunityInvitationHandler) handleInvite(w http.ResponseWriter, r *http.Request, procedure string,
	invite func(context.Context, moderation.InviteRequest) (*moderation.CommunityActionResult, error),
) {
	var input inviteInput
	actorDID, ok := decodeInvitationMutation(w, r, &input, &input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := invite(r.Context(), moderation.InviteRequest{
		ActorDID: actorDID, CommunityDID: input.Community, Invitee: input.Subject,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	if err == nil && result == nil {
		err = errors.New("community invitation action returned no result")
	}
	if err != nil {
		writeCommunityInvitationError(w, procedure, err)
		return
	}
	invitation, err := h.service.Invitation(r.Context(), result.InvitationID)
	if err == nil && invitation == nil {
		err = fmt.Errorf("invitation %s not found after %s", result.InvitationID, procedure)
	}
	if err != nil {
		writeCommunityInvitationError(w, procedure, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, inviteOutput{Invitation: newInvitationView(*invitation), Version: result.Version})
}

// HandleAcceptInvitation serves social.coves.community.acceptInvitation: the
// authenticated invitee accepts.
func (h *CommunityInvitationHandler) HandleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var input invitationInput
	actorDID, ok := decodeInvitationMutation(w, r, &input, &input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := h.service.AcceptInvitation(r.Context(), moderation.AcceptInvitationRequest{
		ActorDID: actorDID, InvitationID: input.Invitation,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	if err == nil && result == nil {
		err = errors.New("community invitation accept returned no result")
	}
	if err != nil {
		writeCommunityInvitationError(w, "acceptInvitation", err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, acceptInvitationOutput{
		Community: result.CommunityDID, Subject: result.SubjectDID, Version: result.Version,
	})
}

// HandleDeclineInvitation serves social.coves.community.declineInvitation:
// the authenticated invitee declines.
func (h *CommunityInvitationHandler) HandleDeclineInvitation(w http.ResponseWriter, r *http.Request) {
	var input invitationInput
	actorDID, ok := decodeInvitationMutation(w, r, &input, &input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := h.service.DeclineInvitation(r.Context(), moderation.DeclineInvitationRequest{
		ActorDID: actorDID, InvitationID: input.Invitation,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	if err == nil && result == nil {
		err = errors.New("community invitation decline returned no result")
	}
	if err != nil {
		writeCommunityInvitationError(w, "declineInvitation", err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, declineInvitationOutput{Invitation: result.InvitationID})
}

// HandleListInvitations serves social.coves.community.listInvitations: the
// community's pending invitations, for its owner or an instance admin.
func (h *CommunityInvitationHandler) HandleListInvitations(w http.ResponseWriter, r *http.Request) {
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	invitations, err := h.service.ListCommunityInvitations(r.Context(), actorDID, r.URL.Query().Get("community"))
	if err != nil {
		writeCommunityInvitationError(w, "listInvitations", err)
		return
	}
	output := listInvitationsOutput{Invitations: make([]invitationView, 0, len(invitations))}
	for _, invitation := range invitations {
		output.Invitations = append(output.Invitations, newInvitationView(invitation))
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// HandleListViewerInvitations serves
// social.coves.community.listViewerInvitations: the authenticated caller's
// own pending invitations, each with its community's current roster version.
// No parameter names another account.
func (h *CommunityInvitationHandler) HandleListViewerInvitations(w http.ResponseWriter, r *http.Request) {
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	invitations, err := h.service.ListViewerInvitations(r.Context(), viewerDID)
	if err != nil {
		writeCommunityInvitationError(w, "listViewerInvitations", err)
		return
	}
	output := listViewerInvitationsOutput{Invitations: make([]viewerInvitationView, 0, len(invitations))}
	versions := make(map[string]int64)
	for _, invitation := range invitations {
		version, known := versions[invitation.CommunityDID]
		if !known {
			version, err = h.service.RosterVersion(r.Context(), invitation.CommunityDID)
			if err != nil {
				writeCommunityInvitationError(w, "listViewerInvitations", err)
				return
			}
			versions[invitation.CommunityDID] = version
		}
		output.Invitations = append(output.Invitations, viewerInvitationView{
			Invitation: newInvitationView(invitation), RosterVersion: version,
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// decodeInvitationMutation checks the method and content type, decodes the
// body into input, requires the caller and expectedVersion, and returns the
// caller DID. It writes the error response and returns false on refusal.
func decodeInvitationMutation(w http.ResponseWriter, r *http.Request, input any, expectedVersion **int64) (string, bool) {
	if !requireMutationPOSTJSON(w, r) {
		return "", false
	}
	if !decodeMutationRequest(w, r, input) {
		return "", false
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return "", false
	}
	if *expectedVersion == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "expectedVersion is required")
		return "", false
	}
	return actorDID, true
}

func writeCommunityInvitationError(w http.ResponseWriter, procedure string, err error) {
	switch {
	case errors.Is(err, moderation.ErrAuthRequired):
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
	case errors.Is(err, moderation.ErrForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not permitted to manage this community's invitations")
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid community, subject, invitation, expectedVersion or idempotencyKey")
	case errors.Is(err, moderation.ErrStateConflict):
		xrpc.WriteError(w, http.StatusConflict, "StateConflict", "The community's moderators changed; reload and retry")
	case errors.Is(err, moderation.ErrIdempotencyConflict):
		xrpc.WriteError(w, http.StatusConflict, "IdempotencyConflict", "Idempotency key was used for a different request")
	case errors.Is(err, moderation.ErrInviteeHasRole):
		xrpc.WriteError(w, http.StatusBadRequest, "InviteeHasRole", "The account already holds a role in this community")
	case errors.Is(err, moderation.ErrInviteeNotModerator):
		xrpc.WriteError(w, http.StatusBadRequest, "InviteeNotModerator", "Ownership can only be offered to a current moderator")
	case errors.Is(err, moderation.ErrInviteeBanned):
		xrpc.WriteError(w, http.StatusBadRequest, "InviteeBanned", "The account is banned from this community")
	case errors.Is(err, moderation.ErrInvitationNotFound):
		xrpc.WriteError(w, http.StatusNotFound, "InvitationNotFound", "Invitation not found")
	case errors.Is(err, moderation.ErrInvitationLapsed):
		xrpc.WriteError(w, http.StatusBadRequest, "InvitationLapsed", "The invitation has lapsed")
	case errors.Is(err, moderation.ErrInvitationNotOpen):
		xrpc.WriteError(w, http.StatusBadRequest, "InvitationNotOpen", "The invitation is no longer open")
	case errors.Is(err, moderation.ErrInvitationStale):
		xrpc.WriteError(w, http.StatusBadRequest, "InvitationStale", "The account that offered ownership is no longer the owner")
	case errors.Is(err, moderation.ErrAccountNotFound):
		xrpc.WriteError(w, http.StatusNotFound, "AccountNotFound", "No account has this handle")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("community invitation action unavailable", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	default:
		// DIDs, invitation IDs and database errors carry no credentials, so
		// the full error is logged; the response body stays generic.
		slog.Error("unexpected community invitation action failure", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
