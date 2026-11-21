package moderation

import (
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/accountbans"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
)

// GetViewerAuthorityHandler serves social.coves.actor.getViewerAuthority.
type GetViewerAuthorityHandler struct {
	authority   moderation.Authority
	accountBans accountbans.Reader
	roles       communities.RoleAuthorizer
}

// GetViewerAuthorityOption configures NewGetViewerAuthorityHandler.
type GetViewerAuthorityOption func(*GetViewerAuthorityHandler)

// WithAccountBanNotice installs the instance account-ban read. With it, a
// viewer whose account has a ban in force gets the ban notice.
func WithAccountBanNotice(reader accountbans.Reader) GetViewerAuthorityOption {
	return func(h *GetViewerAuthorityHandler) { h.accountBans = reader }
}

// WithCommunityRoles installs the community role authorizer, so the output
// reports whether the viewer holds a usable community role.
func WithCommunityRoles(authorizer communities.RoleAuthorizer) GetViewerAuthorityOption {
	return func(h *GetViewerAuthorityHandler) { h.roles = authorizer }
}

// NewGetViewerAuthorityHandler builds the handler over the instance admin
// authority, which is required.
func NewGetViewerAuthorityHandler(authority moderation.Authority, opts ...GetViewerAuthorityOption) *GetViewerAuthorityHandler {
	if authority == nil {
		panic("moderation.NewGetViewerAuthorityHandler: authority is required")
	}
	h := &GetViewerAuthorityHandler{authority: authority}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HandleGetViewerAuthority reports whether the authenticated viewer is an
// instance admin, whether the viewer holds a usable community role and, while
// the viewer's account is banned, the ban notice clients show wherever the
// account would post. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the viewer DID; a
// non-admin gets false, not a refusal.
func (h *GetViewerAuthorityHandler) HandleGetViewerAuthority(w http.ResponseWriter, r *http.Request) {
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	var notice *users.AccountBanNotice
	if h.accountBans != nil {
		ban, err := h.accountBans.ActiveBan(r.Context(), viewerDID)
		if err != nil {
			slog.Error("getViewerAuthority: reading the viewer's account ban failed", slog.String("error", err.Error()))
			xrpc.WriteError(w, http.StatusInternalServerError, "InternalError", "Failed to read viewer authority")
			return
		}
		if ban.InForce(time.Now()) {
			notice = &users.AccountBanNotice{Reason: ban.Reason, ExpiresAt: ban.ExpiresAt}
		}
	}
	var hasCommunityRole *bool
	if h.roles != nil {
		held, err := h.roles.HasUsableRole(r.Context(), viewerDID)
		if err != nil {
			slog.Error("getViewerAuthority: reading the viewer's community roles failed", slog.String("error", err.Error()))
			xrpc.WriteError(w, http.StatusInternalServerError, "InternalError", "Failed to read viewer authority")
			return
		}
		hasCommunityRole = &held
	}
	xrpc.WriteJSON(w, http.StatusOK, struct {
		BanNotice        *users.AccountBanNotice `json:"banNotice,omitempty"`
		HasCommunityRole *bool                   `json:"hasCommunityRole,omitempty"`
		InstanceAdmin    bool                    `json:"instanceAdmin"`
	}{InstanceAdmin: h.authority.IsInstanceAdmin(viewerDID), BanNotice: notice, HasCommunityRole: hasCommunityRole})
}
