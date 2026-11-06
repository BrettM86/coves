package community

import (
	"fmt"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
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
