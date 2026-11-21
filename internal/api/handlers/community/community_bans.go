package community

import (
	"fmt"
	"net/http"
	"time"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
)

// GetHandlerOption configures the community get handler.
type GetHandlerOption func(*GetHandler)

// WithCommunityBans installs the community ban reader, so community.get serves
// an authenticated viewer's active ban in the community.
func WithCommunityBans(bans communities.BanReader) GetHandlerOption {
	return func(h *GetHandler) { h.bans = bans }
}

// populateBanState sets the viewer's ban fields when the authenticated viewer
// has an active ban in community. Nothing is set otherwise.
func (h *GetHandler) populateBanState(r *http.Request, community *communities.Community) error {
	if h.bans == nil || community == nil {
		return nil
	}
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		return nil
	}
	ban, err := communities.ActiveCommunityBan(r.Context(), h.bans, community.DID, viewerDID, time.Now())
	if err != nil {
		return fmt.Errorf("get community ban state: %w", err)
	}
	if ban == nil {
		return nil
	}
	if community.Viewer == nil {
		community.Viewer = &communities.CommunityViewerState{}
	}
	banned := true
	community.Viewer.Banned = &banned
	community.Viewer.BanURI = ban.URI()
	community.Viewer.BanReason = ban.Reason
	if ban.ExpiresAt != nil {
		community.Viewer.BanExpiresAt = ban.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return nil
}

// WithViewerRole installs the role authorizer, so community.get serves a
// signed-in viewer's usable role in the community.
func WithViewerRole(authorizer communities.RoleAuthorizer) GetHandlerOption {
	return func(h *GetHandler) { h.roles = authorizer }
}

// populateRoleState sets the viewer's role when the authenticated viewer holds
// a usable role in community. Nothing is set otherwise.
func (h *GetHandler) populateRoleState(r *http.Request, community *communities.Community) error {
	if h.roles == nil || community == nil {
		return nil
	}
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		return nil
	}
	role, err := h.roles.ViewerRole(r.Context(), community.DID, viewerDID)
	if err != nil {
		return fmt.Errorf("get community viewer role: %w", err)
	}
	if role == "" {
		return nil
	}
	if community.Viewer == nil {
		community.Viewer = &communities.CommunityViewerState{}
	}
	community.Viewer.Role = role
	return nil
}

// WithViewerInvitation installs the invitation reader, so community.get
// serves a signed-in viewer's pending invitation to a role in the community.
func WithViewerInvitation(reader communities.InvitationReader) GetHandlerOption {
	return func(h *GetHandler) { h.invitations = reader }
}

// populateInvitationState sets the viewer's pending invitation when the
// authenticated viewer has one in community. Nothing is set otherwise, so no
// other caller learns that an invitation exists.
func (h *GetHandler) populateInvitationState(r *http.Request, community *communities.Community) error {
	if h.invitations == nil || community == nil {
		return nil
	}
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		return nil
	}
	invitation, err := h.invitations.PendingInvitation(r.Context(), community.DID, viewerDID, time.Now())
	if err != nil {
		return fmt.Errorf("get community viewer invitation: %w", err)
	}
	if invitation == nil {
		return nil
	}
	if community.Viewer == nil {
		community.Viewer = &communities.CommunityViewerState{}
	}
	community.Viewer.Invitation = &communities.CommunityViewerInvitation{
		ID: invitation.ID, Role: invitation.Role,
		ExpiresAt: invitation.ExpiresAt.UTC().Format(time.RFC3339),
	}
	return nil
}

// WithCommunityViewerModeration installs the community viewer reader, so
// community.get serves the community moderation actions the signed-in viewer
// may take on the community.
func WithCommunityViewerModeration(viewer moderation.CommunityViewer) GetHandlerOption {
	if viewer == nil {
		panic("community.WithCommunityViewerModeration: viewer is required")
	}
	return func(h *GetHandler) { h.communityViewer = viewer }
}

// populateCommunityModeration sets the community moderation actions the
// signed-in viewer may take on community. Nothing is set otherwise; a failed
// authority read is returned, as the sibling viewer reads' failures are.
func (h *GetHandler) populateCommunityModeration(r *http.Request, community *communities.Community) error {
	if community == nil {
		return nil
	}
	actions, err := common.CommunityViewerActionsOn(r, h.communityViewer, community.DID)
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}
	if community.Viewer == nil {
		community.Viewer = &communities.CommunityViewerState{}
	}
	community.Viewer.CommunityModeration = actions
	return nil
}
