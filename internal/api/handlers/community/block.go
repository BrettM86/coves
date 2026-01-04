package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"encoding/json"
	"log"
	"net/http"
)

// BlockHandler handles community blocking operations
type BlockHandler struct {
	service communities.Service
}

// NewBlockHandler creates a new block handler
func NewBlockHandler(service communities.Service) *BlockHandler {
	return &BlockHandler{
		service: service,
	}
}

// HandleBlock blocks a community
// POST /xrpc/social.coves.community.blockCommunity
//
// Request body: { "community": "at-identifier" }
// Accepts DIDs (did:plc:xxx), handles (@gaming.community.coves.social), or scoped (!gaming@coves.social)
// The block record's "subject" field requires format: "did", so we resolve the identifier internally.
func (h *BlockHandler) HandleBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // at-identifier (DID or handle)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	// The session contains the user's DID and credentials needed for DPoP authentication
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Block via service (write-forward to PDS with DPoP authentication)
	// Service handles identifier resolution (DIDs, handles, scoped identifiers)
	block, err := h.service.BlockCommunity(r.Context(), session, req.Community)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response (following atProto conventions for block responses)
	response := map[string]interface{}{
		"block": map[string]interface{}{
			"recordUri": block.RecordURI,
			"recordCid": block.RecordCID,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// HandleUnblock unblocks a community
// POST /xrpc/social.coves.community.unblockCommunity
//
// Request body: { "community": "at-identifier" }
// Accepts DIDs (did:plc:xxx), handles (@gaming.community.coves.social), or scoped (!gaming@coves.social)
func (h *BlockHandler) HandleUnblock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // at-identifier (DID or handle)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	// The session contains the user's DID and credentials needed for DPoP authentication
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Unblock via service (delete record on PDS with DPoP authentication)
	// Service handles identifier resolution (DIDs, handles, scoped identifiers)
	err := h.service.UnblockCommunity(r.Context(), session, req.Community)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	}); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
