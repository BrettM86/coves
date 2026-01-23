package community

import (
	"encoding/json"
	"log"
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

	// Parse limit (1-100, default 50)
	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		l, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid limit parameter: must be an integer")
			return
		}
		if l < 1 {
			limit = 1
		} else if l > 100 {
			limit = 100
		} else {
			limit = l
		}
	}

	// Parse cursor (offset-based for now)
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

	// Convert to view structs for API response
	views := make([]*communities.CommunityView, len(results))
	for i, c := range results {
		views[i] = c.ToCommunityView()
	}

	// Build response
	response := map[string]interface{}{
		"communities": views,
		"cursor":      offset + len(results),
		"total":       total,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		log.Printf("Failed to encode community search response: %v", err)
	}
}
