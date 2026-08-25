package communitysuggestion

import (
	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/communitysuggestions"
	"encoding/json"
	"log"
	"net/http"
)

// CreateHandler handles community suggestion creation
type CreateHandler struct {
	service communitysuggestions.Service
}

// NewCreateHandler creates a new create handler
func NewCreateHandler(service communitysuggestions.Service) *CreateHandler {
	return &CreateHandler{
		service: service,
	}
}

// createSuggestionInput represents the JSON request body for creating a suggestion
type createSuggestionInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// HandleCreate creates a new community suggestion
// POST /xrpc/social.coves.community.suggestion.create
func (h *CreateHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Parse JSON body under the small tier
	var input createSuggestionInput
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &input) {
		return
	}

	// Extract authenticated user DID from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Build the create request
	req := communitysuggestions.CreateSuggestionRequest{
		Title:        input.Title,
		Description:  input.Description,
		SubmitterDID: userDID,
	}

	// Create suggestion via service
	suggestion, err := h.service.CreateSuggestion(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return full suggestion JSON on success
	data, err := json.Marshal(suggestion)
	if err != nil {
		log.Printf("Failed to marshal create suggestion response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}
