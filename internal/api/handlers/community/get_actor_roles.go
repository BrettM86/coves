package community

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/core/communities"
)

// GetActorRolesHandler serves social.coves.community.getActorRoles.
type GetActorRolesHandler struct {
	roles communities.RoleReader
}

// NewGetActorRolesHandler creates a getActorRoles handler.
func NewGetActorRolesHandler(roles communities.RoleReader) *GetActorRolesHandler {
	return &GetActorRolesHandler{roles: roles}
}

// actorRoleView is one community role of a getActorRoles response.
type actorRoleView struct {
	Community string `json:"community"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

type getActorRolesResponse struct {
	Actor string          `json:"actor"`
	Roles []actorRoleView `json:"roles"`
}

// HandleGetActorRoles lists the communities in which an actor holds a role.
// The listing is public, as the role records are.
// GET /xrpc/social.coves.community.getActorRoles?actor={did}
func (h *GetActorRolesHandler) HandleGetActorRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := r.URL.Query().Get("actor")
	if actor == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "actor parameter is required")
		return
	}
	if _, err := syntax.ParseDID(actor); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "actor must be a DID")
		return
	}

	roles, err := h.roles.ListRolesBySubject(r.Context(), actor)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	response := getActorRolesResponse{
		Actor: actor,
		Roles: make([]actorRoleView, 0, len(roles)),
	}
	for _, role := range roles {
		response.Roles = append(response.Roles, actorRoleView{
			Community: role.CommunityDID,
			Role:      role.Role,
			CreatedAt: role.RecordCreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode community getActorRoles response: %v", err)
	}
}
