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
// GET /xrpc/social.coves.community.list?limit={n}&cursor={offset}&visibility={public|unlisted}&sortBy={created_at|member_count}
func (h *ListHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	query := r.URL.Query()

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

	req := communities.ListCommunitiesRequest{
		Limit:      limit,
		Offset:     offset,
		Visibility: query.Get("visibility"),
		HostedBy:   query.Get("hostedBy"),
		SortBy:     query.Get("sortBy"),
		SortOrder:  query.Get("sortOrder"),
	}

	// Get communities from AppView DB
	results, total, err := h.service.ListCommunities(r.Context(), req)
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
