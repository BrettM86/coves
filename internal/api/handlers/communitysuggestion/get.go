package communitysuggestion

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communitysuggestions"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// GetHandler handles retrieving a single community suggestion
type GetHandler struct {
	service communitysuggestions.Service
}

// NewGetHandler creates a new get handler
func NewGetHandler(service communitysuggestions.Service) *GetHandler {
	return &GetHandler{
		service: service,
	}
}

// HandleGet retrieves a single community suggestion by ID
// GET /xrpc/social.coves.community.suggestion.get?id=123
func (h *GetHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Parse id query param
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Missing required parameter: id")
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid id parameter: must be a positive integer")
		return
	}

	// Extract optional DID for viewer state
	viewerDID := middleware.GetUserDID(r)

	// Get suggestion via service
	suggestion, err := h.service.GetSuggestion(r.Context(), id, viewerDID)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return full suggestion JSON
	data, err := json.Marshal(suggestion)
	if err != nil {
		log.Printf("Failed to marshal get suggestion response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}
