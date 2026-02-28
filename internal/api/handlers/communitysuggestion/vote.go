package communitysuggestion

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communitysuggestions"
	"encoding/json"
	"log"
	"net/http"
)

// VoteHandler handles voting on community suggestions
type VoteHandler struct {
	service communitysuggestions.Service
}

// NewVoteHandler creates a new vote handler
func NewVoteHandler(service communitysuggestions.Service) *VoteHandler {
	return &VoteHandler{
		service: service,
	}
}

// voteInput represents the JSON request body for casting a vote
type voteInput struct {
	SuggestionID int64 `json:"suggestionId"`
	Value        int   `json:"value"`
}

// removeVoteInput represents the JSON request body for removing a vote
type removeVoteInput struct {
	SuggestionID int64 `json:"suggestionId"`
}

// HandleVote casts or toggles a vote on a community suggestion
// POST /xrpc/social.coves.community.suggestion.vote
func (h *VoteHandler) HandleVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Limit request body size to 10KB
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	// Parse JSON body
	var input voteInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		log.Printf("[COMMUNITY_SUGGESTION] Failed to decode vote JSON request: %v", err)
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Extract authenticated user DID
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Build the vote request
	req := communitysuggestions.VoteRequest{
		SuggestionID: input.SuggestionID,
		VoterDID:     userDID,
		Value:        input.Value,
	}

	// Cast vote via service
	if err := h.service.Vote(r.Context(), req); err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"success":true}`)); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

// HandleRemoveVote removes a vote from a community suggestion
// POST /xrpc/social.coves.community.suggestion.removeVote
func (h *VoteHandler) HandleRemoveVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Limit request body size to 10KB
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	// Parse JSON body
	var input removeVoteInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		log.Printf("[COMMUNITY_SUGGESTION] Failed to decode remove vote JSON request: %v", err)
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Extract authenticated user DID
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Remove vote via service
	if err := h.service.RemoveVote(r.Context(), input.SuggestionID, userDID); err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"success":true}`)); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}
