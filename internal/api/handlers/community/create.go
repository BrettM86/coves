package community

import (
	"encoding/json"
	"net/http"

	"Coves/internal/core/communities"
)

// CreateHandler handles community creation
type CreateHandler struct {
	service communities.Service
}

// NewCreateHandler creates a new create handler
func NewCreateHandler(service communities.Service) *CreateHandler {
	return &CreateHandler{
		service: service,
	}
}

// HandleCreate creates a new community
// POST /xrpc/social.coves.community.create
// Body matches CreateCommunityRequest
func (h *CreateHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req communities.CreateCommunityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// TODO(Communities-OAuth): Extract authenticated user DID from request context
	// This MUST be replaced with OAuth middleware before production deployment
	// Expected implementation:
	//   userDID := r.Context().Value("authenticated_user_did").(string)
	//   req.CreatedByDID = userDID
	// For now, we require client to send it (INSECURE - allows impersonation)
	if req.CreatedByDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	if req.HostedByDID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "hostedByDid is required")
		return
	}

	// Create community via service (write-forward to PDS)
	community, err := h.service.CreateCommunity(r.Context(), req)
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
	json.NewEncoder(w).Encode(response)
}
