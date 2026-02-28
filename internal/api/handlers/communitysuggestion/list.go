package communitysuggestion

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communitysuggestions"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// ListHandler handles listing community suggestions
type ListHandler struct {
	service communitysuggestions.Service
}

// NewListHandler creates a new list handler
func NewListHandler(service communitysuggestions.Service) *ListHandler {
	return &ListHandler{
		service: service,
	}
}

// HandleList lists community suggestions with filters
// GET /xrpc/social.coves.community.suggestion.list?sort=popular&status=open&limit=50&cursor=0
func (h *ListHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	// Parse sort (default "popular", valid: "popular"|"new")
	sort := query.Get("sort")
	if sort == "" {
		sort = "popular"
	}
	validSorts := map[string]bool{
		"popular": true,
		"new":     true,
	}
	if !validSorts[sort] {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid sort value. Must be: popular or new")
		return
	}

	// Parse status (optional, validate if present)
	status := query.Get("status")
	if status != "" && !communitysuggestions.IsValidStatus(status) {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid status value. Must be one of: open, under_review, approved, declined")
		return
	}

	// Parse limit (1-100, default 50)
	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		l, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid limit parameter: must be an integer")
			return
		}
		if l < 1 || l > 100 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid limit parameter: must be between 1 and 100")
			return
		}
		limit = l
	}

	// Parse cursor (offset-based)
	offset := 0
	if cursorStr := query.Get("cursor"); cursorStr != "" {
		o, err := strconv.Atoi(cursorStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid cursor parameter: must be an integer")
			return
		}
		if o < 0 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid cursor parameter: must be non-negative")
			return
		}
		offset = o
	}

	// Extract optional DID for viewer state
	viewerDID := middleware.GetUserDID(r)

	// Build the list request
	req := communitysuggestions.ListSuggestionsRequest{
		Sort:      sort,
		Status:    status,
		Limit:     limit,
		Offset:    offset,
		ViewerDID: viewerDID,
	}

	// List suggestions via service
	suggestions, err := h.service.ListSuggestions(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build cursor: next offset when there are more results
	var cursor string
	if len(suggestions) == limit {
		cursor = strconv.Itoa(offset + len(suggestions))
	}

	// Build response
	response := map[string]interface{}{
		"suggestions": suggestions,
		"cursor":      cursor,
	}

	data, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal suggestion list response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}
