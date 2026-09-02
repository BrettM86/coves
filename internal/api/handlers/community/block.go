package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/communities"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// BlockHandler handles community blocking operations
type BlockHandler struct {
	service communities.Service
}

type blockedCommunitiesResponse struct {
	Blocks []blockedCommunityEntry `json:"blocks"`
	Cursor *string                 `json:"cursor,omitempty"`
}

type blockedCommunityEntry struct {
	CommunityDID string    `json:"communityDid"`
	RecordURI    string    `json:"recordUri"`
	RecordCID    string    `json:"recordCid"`
	BlockedAt    time.Time `json:"blockedAt"`
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

	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &req) {
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

// HandleGetBlocked returns the communities blocked by the authenticated user.
// GET /xrpc/social.coves.community.getBlockedCommunities?limit=50&cursor=0
func (h *BlockHandler) HandleGetBlocked(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	limit := 50
	offset := 0

	if limitString := r.URL.Query().Get("limit"); limitString != "" {
		parsed, err := strconv.Atoi(limitString)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("invalid limit parameter: %q", limitString))
			return
		}
		// The lexicon promises 1..100; silently rewriting the limit would make
		// the client's offset cursor skip or repeat records.
		if parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid limit parameter: must be between 1 and 100")
			return
		}
		limit = parsed
	}

	// Cursor pagination is offset-based for now.
	if cursorString := r.URL.Query().Get("cursor"); cursorString != "" {
		parsed, err := strconv.Atoi(cursorString)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("invalid cursor parameter: %q", cursorString))
			return
		}
		if parsed < 0 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid cursor parameter: must be non-negative")
			return
		}
		offset = parsed
	}

	blocks, err := h.service.GetBlockedCommunities(r.Context(), session.AccountDID.String(), limit, offset)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	entries := make([]blockedCommunityEntry, 0, len(blocks))
	for _, block := range blocks {
		entries = append(entries, blockedCommunityEntry{
			CommunityDID: block.CommunityDID,
			RecordURI:    block.RecordURI,
			RecordCID:    block.RecordCID,
			BlockedAt:    block.BlockedAt,
		})
	}

	var nextCursor *string
	if len(blocks) == limit {
		cursor := strconv.Itoa(offset + len(blocks))
		nextCursor = &cursor
	}

	xrpc.WriteJSON(w, http.StatusOK, blockedCommunitiesResponse{
		Blocks: entries,
		Cursor: nextCursor,
	})
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

	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &req) {
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
