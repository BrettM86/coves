package vote

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"
	"encoding/json"
	"log"
	"net/http"
)

// CreateVoteHandler handles vote creation
type CreateVoteHandler struct {
	service votes.Service
}

// NewCreateVoteHandler creates a new create vote handler
func NewCreateVoteHandler(service votes.Service) *CreateVoteHandler {
	return &CreateVoteHandler{
		service: service,
	}
}

// CreateVoteInput represents the request body for creating a vote
type CreateVoteInput struct {
	Subject struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"subject"`
	Direction string `json:"direction"`
}

// CreateVoteOutput represents the response body for creating a vote
type CreateVoteOutput struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// HandleCreateVote creates a vote on a post or comment
// POST /xrpc/social.coves.vote.create
//
// Request body: { "subject": { "uri": "at://...", "cid": "..." }, "direction": "up" }
// Response: { "uri": "at://...", "cid": "..." }
//
// Behavior:
// - If no vote exists: creates new vote with given direction
// - If vote exists with same direction: deletes vote (toggle off)
// - If vote exists with different direction: updates to new direction
func (h *CreateVoteHandler) HandleCreateVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var input CreateVoteInput
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
	if input.Direction == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "direction is required")
		return
	}

	// Validate direction
	if input.Direction != "up" && input.Direction != "down" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "direction must be 'up' or 'down'")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Create vote request
	req := votes.CreateVoteRequest{
		Subject: votes.StrongRef{
			URI: input.Subject.URI,
			CID: input.Subject.CID,
		},
		Direction: input.Direction,
	}

	// Call service to create vote
	response, err := h.service.CreateVote(r.Context(), session, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response
	output := CreateVoteOutput{
		URI: response.URI,
		CID: response.CID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
