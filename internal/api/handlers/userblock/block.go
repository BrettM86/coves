package userblock

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/userblocks"
)

// BlockHandler handles user-to-user blocking operations
type BlockHandler struct {
	service userblocks.Service
}

// NewBlockHandler creates a new user block handler
func NewBlockHandler(service userblocks.Service) *BlockHandler {
	if service == nil {
		panic("userblock: NewBlockHandler requires a non-nil service")
	}
	return &BlockHandler{
		service: service,
	}
}

// blockRequest is the input for block/unblock operations.
type blockRequest struct {
	Subject string `json:"subject"`
}

// blockResponse is the output for a successful block operation.
type blockResponse struct {
	Block blockRecord `json:"block"`
}

type blockRecord struct {
	RecordURI string `json:"recordUri"`
	RecordCID string `json:"recordCid"`
}

// unblockResponse is the output for a successful unblock operation.
type unblockResponse struct {
	Success bool `json:"success"`
}

// blockedUsersResponse is the output for the get-blocked-users endpoint.
type blockedUsersResponse struct {
	Blocks []blockedUserEntry `json:"blocks"`
}

type blockedUserEntry struct {
	BlockedDID string    `json:"blockedDid"`
	RecordURI  string    `json:"recordUri"`
	RecordCID  string    `json:"recordCid"`
	BlockedAt  time.Time `json:"blockedAt"`
}

// HandleBlock blocks a user
// POST /xrpc/social.coves.actor.blockUser
//
// Request body: { "subject": "did-or-handle" }
// The subject can be a DID (did:plc:xxx) or a handle.
func (h *BlockHandler) HandleBlock(w http.ResponseWriter, r *http.Request) {
	var req blockRequest
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &req) {
		return
	}

	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "subject is required")
		return
	}

	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	result, err := h.service.BlockUser(r.Context(), session, req.Subject)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	xrpc.WriteJSON(w, http.StatusOK, blockResponse{
		Block: blockRecord{
			RecordURI: result.RecordURI,
			RecordCID: result.RecordCID,
		},
	})
}

// HandleUnblock unblocks a user
// POST /xrpc/social.coves.actor.unblockUser
//
// Request body: { "subject": "did-or-handle" }
func (h *BlockHandler) HandleUnblock(w http.ResponseWriter, r *http.Request) {
	var req blockRequest
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &req) {
		return
	}

	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "subject is required")
		return
	}

	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	err := h.service.UnblockUser(r.Context(), session, req.Subject)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	xrpc.WriteJSON(w, http.StatusOK, unblockResponse{Success: true})
}

// HandleGetBlocked returns the list of users blocked by the authenticated user
// GET /xrpc/social.coves.actor.getBlockedUsers?limit=50&offset=0
func (h *BlockHandler) HandleGetBlocked(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	limit := 50
	offset := 0

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("invalid limit parameter: %q", limitStr))
			return
		}
		limit = parsed
	}

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("invalid offset parameter: %q", offsetStr))
			return
		}
		offset = parsed
	}

	userDID := session.AccountDID.String()

	blocks, err := h.service.GetBlockedUsers(r.Context(), userDID, limit, offset)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	entries := make([]blockedUserEntry, 0, len(blocks))
	for _, b := range blocks {
		entries = append(entries, blockedUserEntry{
			BlockedDID: b.BlockedDID,
			RecordURI:  b.RecordURI,
			RecordCID:  b.RecordCID,
			BlockedAt:  b.BlockedAt,
		})
	}

	xrpc.WriteJSON(w, http.StatusOK, blockedUsersResponse{Blocks: entries})
}
