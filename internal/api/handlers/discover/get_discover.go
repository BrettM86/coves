package discover

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
)

// GetDiscoverHandler handles discover feed retrieval
type GetDiscoverHandler struct {
	service discover.Service
}

// NewGetDiscoverHandler creates a new discover handler
func NewGetDiscoverHandler(service discover.Service) *GetDiscoverHandler {
	return &GetDiscoverHandler{
		service: service,
	}
}

// HandleGetDiscover retrieves posts from all communities (public feed)
// GET /xrpc/social.coves.feed.getDiscover?sort=hot&limit=15&cursor=...
// Public endpoint - no authentication required
func (h *GetDiscoverHandler) HandleGetDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	req := h.parseRequest(r)

	// Get discover feed
	response, err := h.service.GetDiscover(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Transform blob refs to URLs for all posts
	for _, feedPost := range response.Feed {
		if feedPost.Post != nil {
			posts.TransformBlobRefsToURLs(feedPost.Post)
		}
	}

	// Return feed
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode discover response: %v", err)
	}
}

// parseRequest parses query parameters into GetDiscoverRequest
func (h *GetDiscoverHandler) parseRequest(r *http.Request) discover.GetDiscoverRequest {
	req := discover.GetDiscoverRequest{}

	// Optional: sort (default: hot)
	req.Sort = r.URL.Query().Get("sort")
	if req.Sort == "" {
		req.Sort = "hot"
	}

	// Optional: timeframe (default: day for top sort)
	req.Timeframe = r.URL.Query().Get("timeframe")
	if req.Timeframe == "" && req.Sort == "top" {
		req.Timeframe = "day"
	}

	// Optional: limit (default: 15, max: 50)
	req.Limit = 15
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil {
			req.Limit = limit
		}
	}

	// Optional: cursor
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		req.Cursor = &cursor
	}

	return req
}
