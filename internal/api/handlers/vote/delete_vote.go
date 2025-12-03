package vote

import (
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

// DeleteVoteInput represents the request body for deleting a vote
type DeleteVoteInput struct {
	Subject struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"subject"`
}

// DeleteVoteOutput represents the response body for deleting a vote
// Per lexicon: output is an empty object
type DeleteVoteOutput struct{}

// HandleDeleteVote removes a vote from a post or comment
// POST /xrpc/social.coves.vote.delete
//
// Request body: { "subject": { "uri": "at://...", "cid": "..." } }
// Response: { "success": true }
func (h *DeleteVoteHandler) HandleDeleteVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var input DeleteVoteInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Validate required fields
	if input.Subject.URI == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "subject.uri is required")
		return
	}
	if input.Subject.CID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "subject.cid is required")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Create delete vote request
	req := votes.DeleteVoteRequest{
		Subject: votes.StrongRef{
			URI: input.Subject.URI,
			CID: input.Subject.CID,
		},
	}

	// Call service to delete vote
	err := h.service.DeleteVote(r.Context(), session, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response (empty object per lexicon)
	output := DeleteVoteOutput{}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
