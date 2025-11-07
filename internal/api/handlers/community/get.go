package community

import (
	"encoding/json"
	"net/http"

	"Coves/internal/core/communities"
)

// GetHandler handles community retrieval
type GetHandler struct {
	service communities.Service
}

// NewGetHandler creates a new get handler
func NewGetHandler(service communities.Service) *GetHandler {
	return &GetHandler{
		service: service,
	}
}

// HandleGet retrieves a community by DID or handle
// GET /xrpc/social.coves.community.get?community={did_or_handle}
func (h *GetHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get community identifier from query params
	communityID := r.URL.Query().Get("community")
	if communityID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community parameter is required")
		return
	}

	// Get community from AppView DB
	community, err := h.service.GetCommunity(r.Context(), communityID)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return community data
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(community); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
