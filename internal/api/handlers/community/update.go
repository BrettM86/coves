package community

import (
	"encoding/json"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
)

// UpdateHandler handles community updates
type UpdateHandler struct {
	service communities.Service
}

// NewUpdateHandler creates a new update handler
func NewUpdateHandler(service communities.Service) *UpdateHandler {
	return &UpdateHandler{
		service: service,
	}
}

// HandleUpdate updates an existing community
// POST /xrpc/social.coves.community.update
// Body matches UpdateCommunityRequest
func (h *UpdateHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req communities.UpdateCommunityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Validate required fields
	if req.CommunityDID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "communityDid is required")
		return
	}

	// Extract authenticated user DID from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Set the authenticated user as the updater
	req.UpdatedByDID = userDID

	// Update community via service (write-forward to PDS)
	community, err := h.service.UpdateCommunity(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response matching lexicon output
	response := map[string]interface{}{
		"uri":    community.RecordURI,
		"cid":    community.RecordCID,
		"did":    community.DID,
		"handle": community.Handle,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
