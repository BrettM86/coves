package vote

import (
	"Coves/internal/api/handlers"
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

// HandleCreateVote creates a vote or toggles an existing vote
// POST /xrpc/social.coves.interaction.createVote
//
// Request body: { "subject": "at://...", "direction": "up" | "down" }
func (h *CreateVoteHandler) HandleCreateVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req votes.CreateVoteRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Subject == "" {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "subject is required")
		return
	}

	if req.Direction == "" {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "direction is required")
		return
	}

	if req.Direction != "up" && req.Direction != "down" {
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "direction must be 'up' or 'down'")
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

	// Create vote via service (write-forward to user's PDS)
	response, err := h.service.CreateVote(r.Context(), voterDID, userAccessToken, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Handle toggle-off case (vote was deleted, not created)
	if response.URI == "" {
		// Vote was toggled off (deleted)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"deleted": true,
		}); err != nil {
			log.Printf("Failed to encode response: %v", err)
		}
		return
	}

	// Return success response
	responseMap := map[string]interface{}{
		"uri": response.URI,
		"cid": response.CID,
	}

	if response.Existing != nil {
		responseMap["existing"] = *response.Existing
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(responseMap); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// handleServiceError converts service errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch err {
	case votes.ErrVoteNotFound:
		handlers.WriteError(w, http.StatusNotFound, "VoteNotFound", "Vote not found")
	case votes.ErrSubjectNotFound:
		handlers.WriteError(w, http.StatusNotFound, "SubjectNotFound", "Post or comment not found")
	case votes.ErrInvalidDirection:
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid vote direction")
	case votes.ErrInvalidSubject:
		handlers.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid subject URI")
	case votes.ErrVoteAlreadyExists:
		handlers.WriteError(w, http.StatusConflict, "VoteAlreadyExists", "Vote already exists")
	case votes.ErrNotAuthorized:
		handlers.WriteError(w, http.StatusForbidden, "NotAuthorized", "Not authorized")
	case votes.ErrBanned:
		handlers.WriteError(w, http.StatusForbidden, "Banned", "User is banned from this community")
	default:
		// Check for validation errors
		log.Printf("Vote creation error: %v", err)
		handlers.WriteError(w, http.StatusInternalServerError, "InternalError", "Failed to create vote")
	}
}
