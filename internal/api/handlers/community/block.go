package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
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
// Request body: { "community": "did:plc:xxx" }
// Note: Per lexicon spec, only DIDs are accepted (not handles).
// The block record's "subject" field requires format: "did".
func (h *BlockHandler) HandleBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // DID only (per lexicon)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Validate DID format (per lexicon: format must be "did")
	if !strings.HasPrefix(req.Community, "did:") {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"community must be a DID (did:plc:... or did:web:...)")
		return
	}

	// Validate DID format with regex: did:method:identifier
	didRegex := regexp.MustCompile(`^did:(plc|web):[a-zA-Z0-9._:%-]+$`)
	if !didRegex.MatchString(req.Community) {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid DID format")
		return
	}

	// Extract authenticated user DID and access token from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	userAccessToken := middleware.GetUserAccessToken(r)
	if userAccessToken == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Missing access token")
		return
	}

	// Block via service (write-forward to PDS)
	block, err := h.service.BlockCommunity(r.Context(), userDID, userAccessToken, req.Community)
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
// Request body: { "community": "did:plc:xxx" }
// Note: Per lexicon spec, only DIDs are accepted (not handles).
func (h *BlockHandler) HandleUnblock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // DID only (per lexicon)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Validate DID format (per lexicon: format must be "did")
	if !strings.HasPrefix(req.Community, "did:") {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"community must be a DID (did:plc:... or did:web:...)")
		return
	}

	// Validate DID format with regex: did:method:identifier
	didRegex := regexp.MustCompile(`^did:(plc|web):[a-zA-Z0-9._:%-]+$`)
	if !didRegex.MatchString(req.Community) {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid DID format")
		return
	}

	// Extract authenticated user DID and access token from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	userAccessToken := middleware.GetUserAccessToken(r)
	if userAccessToken == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Missing access token")
		return
	}

	// Unblock via service (delete record on PDS)
	err := h.service.UnblockCommunity(r.Context(), userDID, userAccessToken, req.Community)
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
