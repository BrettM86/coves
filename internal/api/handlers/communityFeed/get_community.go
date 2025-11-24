package communityFeed

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

// GetCommunityHandler handles community feed retrieval
type GetCommunityHandler struct {
	service communityFeeds.Service
}

// NewGetCommunityHandler creates a new community feed handler
func NewGetCommunityHandler(service communityFeeds.Service) *GetCommunityHandler {
	return &GetCommunityHandler{
		service: service,
	}
}

// HandleGetCommunity retrieves posts from a community with sorting
// GET /xrpc/social.coves.communityFeed.getCommunity?community={did_or_handle}&sort=hot&limit=15&cursor=...
func (h *GetCommunityHandler) HandleGetCommunity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	req, err := h.parseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Alpha: No viewer context needed for basic community sorting
	// TODO(feed-generator): Extract viewer DID when implementing viewer-specific state
	//                       (blocks, upvotes, saves) in feed generator skeleton

	// Get community feed
	response, err := h.service.GetCommunityFeed(r.Context(), req)
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
		// Log encoding errors but don't return error response (headers already sent)
		log.Printf("ERROR: Failed to encode feed response: %v", err)
	}
}

// parseRequest parses query parameters into GetCommunityFeedRequest
func (h *GetCommunityHandler) parseRequest(r *http.Request) (communityFeeds.GetCommunityFeedRequest, error) {
	req := communityFeeds.GetCommunityFeedRequest{}

	// Required: community
	req.Community = r.URL.Query().Get("community")

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

	return req, nil
}
