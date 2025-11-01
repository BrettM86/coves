package vote

import (
	"Coves/internal/api/handlers"
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"
	"encoding/json"
	"log"
	"net/http"
)

// DeleteVoteHandler handles vote deletion
type DeleteVoteHandler struct {
	service votes.Service
}

// NewDeleteVoteHandler creates a new delete vote handler
func NewDeleteVoteHandler(service votes.Service) *DeleteVoteHandler {
	return &DeleteVoteHandler{
		service: service,
	}
}

// HandleDeleteVote removes a vote from a post/comment
// POST /xrpc/social.coves.interaction.deleteVote
//
// Request body: { "subject": "at://..." }
func (h *DeleteVoteHandler) HandleDeleteVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req votes.DeleteVoteRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Subject == "" {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "subject is required")
		return
	}

	// Extract authenticated user DID and access token from request context (injected by auth middleware)
	voterDID := middleware.GetUserDID(r)
	if voterDID == "" {
		handlers.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	userAccessToken := middleware.GetUserAccessToken(r)
	if userAccessToken == "" {
		handlers.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Missing access token")
		return
	}

	// Delete vote via service (delete record on PDS)
	err := h.service.DeleteVote(r.Context(), voterDID, userAccessToken, req)
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
