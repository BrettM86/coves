package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"encoding/json"
	"net/http"
)

// CreateHandler handles community creation
type CreateHandler struct {
	service                  communities.Service
	allowedCommunityCreators map[string]bool // nil = allow all
}

// NewCreateHandler creates a new create handler
// allowedCreators is a list of DIDs that can create communities. If empty, anyone can create.
func NewCreateHandler(service communities.Service, allowedCreators []string) *CreateHandler {
	var allowedMap map[string]bool
	if len(allowedCreators) > 0 {
		allowedMap = make(map[string]bool)
		for _, did := range allowedCreators {
			if did != "" { // Skip empty strings
				allowedMap[did] = true
			}
		}
		// If all entries were empty, treat as no restriction
		if len(allowedMap) == 0 {
			allowedMap = nil
		}
	}
	return &CreateHandler{
		service:                  service,
		allowedCommunityCreators: allowedMap,
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

	// Extract authenticated user DID from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Check if user is allowed to create communities (if restriction is enabled)
	if h.allowedCommunityCreators != nil && !h.allowedCommunityCreators[userDID] {
		writeError(w, http.StatusForbidden, "CommunityCreationRestricted",
			"Community creation is restricted to authorized users")
		return
	}

	// Client should not send createdByDid - we derive it from authenticated user
	if req.CreatedByDID != "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"createdByDid must not be provided - derived from authenticated user")
		return
	}

	// Client should not send hostedByDid - we derive it from the instance
	if req.HostedByDID != "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"hostedByDid must not be provided - derived from instance")
		return
	}

	// Set the authenticated user as the creator
	req.CreatedByDID = userDID
	// Note: hostedByDID will be set by the service layer based on instance configuration

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
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
