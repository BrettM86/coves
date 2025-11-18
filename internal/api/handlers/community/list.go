package community

import (
	"Coves/internal/core/communities"
	"encoding/json"
	"net/http"
	"strconv"
)

// ListHandler handles listing communities
type ListHandler struct {
	service communities.Service
}

// NewListHandler creates a new list handler
func NewListHandler(service communities.Service) *ListHandler {
	return &ListHandler{
		service: service,
	}
}

// HandleList lists communities with filters
// GET /xrpc/social.coves.community.list?limit={n}&cursor={str}&sort={popular|active|new|alphabetical}&visibility={public|unlisted|private}
func (h *ListHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	// Parse limit (1-100, default 50)
	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			if l < 1 {
				limit = 1
			} else if l > 100 {
				limit = 100
			} else {
				limit = l
			}
		}
	}

	// Parse cursor (offset-based for now)
	offset := 0
	if cursorStr := query.Get("cursor"); cursorStr != "" {
		if o, err := strconv.Atoi(cursorStr); err == nil && o >= 0 {
			offset = o
		}
	}

	// Parse sort enum (default: popular)
	sort := query.Get("sort")
	if sort == "" {
		sort = "popular"
	}

	// Validate sort value
	validSorts := map[string]bool{
		"popular":      true,
		"active":       true,
		"new":          true,
		"alphabetical": true,
	}
	if !validSorts[sort] {
		http.Error(w, "Invalid sort value. Must be: popular, active, new, or alphabetical", http.StatusBadRequest)
		return
	}

	// Validate visibility value if provided
	visibility := query.Get("visibility")
	if visibility != "" {
		validVisibilities := map[string]bool{
			"public":   true,
			"unlisted": true,
			"private":  true,
		}
		if !validVisibilities[visibility] {
			http.Error(w, "Invalid visibility value. Must be: public, unlisted, or private", http.StatusBadRequest)
			return
		}
	}

	req := communities.ListCommunitiesRequest{
		Limit:      limit,
		Offset:     offset,
		Sort:       sort,
		Visibility: visibility,
		Category:   query.Get("category"),
		Language:   query.Get("language"),
	}

	// Get communities from AppView DB
	results, err := h.service.ListCommunities(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build response
	var cursor string
	if len(results) == limit {
		// More results available - return next cursor
		cursor = strconv.Itoa(offset + len(results))
	}
	// If len(results) < limit, we've reached the end - cursor remains empty string

	response := map[string]interface{}{
		"communities": results,
		"cursor":      cursor,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
