package community

import (
	"encoding/json"
	"net/http"
	"strconv"

	"Coves/internal/core/communities"
)

// SearchHandler handles community search
type SearchHandler struct {
	service communities.Service
}

// NewSearchHandler creates a new search handler
func NewSearchHandler(service communities.Service) *SearchHandler {
	return &SearchHandler{
		service: service,
	}
}

// HandleSearch searches communities by name/description
// GET /xrpc/social.coves.community.search?q={query}&limit={n}&cursor={offset}
func (h *SearchHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	searchQuery := query.Get("q")
	if searchQuery == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "q parameter is required")
		return
	}

	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	offset := 0
	if cursorStr := query.Get("cursor"); cursorStr != "" {
		if o, err := strconv.Atoi(cursorStr); err == nil && o >= 0 {
			offset = o
		}
	}

	req := communities.SearchCommunitiesRequest{
		Query:      searchQuery,
		Limit:      limit,
		Offset:     offset,
		Visibility: query.Get("visibility"),
	}

	// Search communities in AppView DB
	results, total, err := h.service.SearchCommunities(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build response
	response := map[string]interface{}{
		"communities": results,
		"cursor":      offset + len(results),
		"total":       total,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
