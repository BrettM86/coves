package moderation

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// GetViewerAuthorityHandler serves social.coves.actor.getViewerAuthority.
type GetViewerAuthorityHandler struct {
	authority moderation.Authority
}

// NewGetViewerAuthorityHandler builds the handler over the instance admin
// authority, which is required.
func NewGetViewerAuthorityHandler(authority moderation.Authority) *GetViewerAuthorityHandler {
	if authority == nil {
		panic("moderation.NewGetViewerAuthorityHandler: authority is required")
	}
	return &GetViewerAuthorityHandler{authority: authority}
}

// HandleGetViewerAuthority reports whether the authenticated viewer is an
// instance admin. It must run behind InstanceAdminMiddleware.RequireAuthenticated,
// which sets the viewer DID; a non-admin gets false, not a refusal.
func (h *GetViewerAuthorityHandler) HandleGetViewerAuthority(w http.ResponseWriter, r *http.Request) {
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, struct {
		InstanceAdmin bool `json:"instanceAdmin"`
	}{InstanceAdmin: h.authority.IsInstanceAdmin(viewerDID)})
}
