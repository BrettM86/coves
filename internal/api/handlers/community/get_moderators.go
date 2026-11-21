package community

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"Coves/internal/core/communities"
)

// GetModeratorsHandler serves social.coves.community.getModerators.
type GetModeratorsHandler struct {
	service communities.Service
	roles   communities.RoleReader
}

// NewGetModeratorsHandler creates a getModerators handler.
func NewGetModeratorsHandler(service communities.Service, roles communities.RoleReader) *GetModeratorsHandler {
	return &GetModeratorsHandler{service: service, roles: roles}
}

// roleView is one roster entry of a getModerators response.
type roleView struct {
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

type getModeratorsResponse struct {
	Community  string     `json:"community"`
	Owner      *roleView  `json:"owner,omitempty"`
	Moderators []roleView `json:"moderators"`
}

func newRoleView(role communities.CommunityRole) roleView {
	return roleView{
		Subject:   role.SubjectDID,
		Role:      role.Role,
		CreatedAt: role.RecordCreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// HandleGetModerators lists a community's owner and moderators. The roster is
// public and uncapped, so there is no limit or cursor.
// GET /xrpc/social.coves.community.getModerators?community={identifier}
func (h *GetModeratorsHandler) HandleGetModerators(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	communityID := r.URL.Query().Get("community")
	if communityID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community parameter is required")
		return
	}

	// The same lookup as community.get, so an unknown community answers the
	// same error.
	community, err := h.service.GetCommunity(r.Context(), communityID)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	roster, err := communities.ReadRoster(r.Context(), h.roles, community.DID)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	response := getModeratorsResponse{
		Community:  community.DID,
		Moderators: make([]roleView, 0, len(roster.Moderators)),
	}
	if roster.Owner != nil {
		owner := newRoleView(*roster.Owner)
		response.Owner = &owner
	}
	for _, moderator := range roster.Moderators {
		response.Moderators = append(response.Moderators, newRoleView(moderator))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode community getModerators response: %v", err)
	}
}
