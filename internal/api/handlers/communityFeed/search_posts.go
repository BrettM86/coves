package communityFeed

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/votes"
)

// SearchPostsHandler serves social.coves.feed.searchPosts.
type SearchPostsHandler struct {
	service        communityFeeds.Service
	voteService    votes.Service
	blueskyService blueskypost.Service
}

// NewSearchPostsHandler creates the post search handler.
func NewSearchPostsHandler(service communityFeeds.Service, voteService votes.Service, blueskyService blueskypost.Service) *SearchPostsHandler {
	return &SearchPostsHandler{
		service:        service,
		voteService:    voteService,
		blueskyService: blueskyService,
	}
}

// HandleSearchPosts handles GET /xrpc/social.coves.feed.searchPosts.
func (h *SearchPostsHandler) HandleSearchPosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	searchQuery := query.Get("q")
	if searchQuery == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "q parameter is required")
		return
	}

	req := communityFeeds.SearchPostsRequest{
		Query:     searchQuery,
		Community: query.Get("community"),
		ViewerDID: middleware.GetUserDID(r),
		Sort:      query.Get("sort"),
		Timeframe: query.Get("timeframe"),
	}
	if _, present := query["limit"]; present {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "limit must be an integer")
			return
		}
		if limit < 1 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "limit must be at least 1")
			return
		}
		req.Limit = limit
	}
	if cursor := query.Get("cursor"); cursor != "" {
		req.Cursor = &cursor
	}

	response, err := h.service.SearchPosts(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	if response.Feed == nil {
		response.Feed = []*communityFeeds.FeedViewPost{}
	}

	processFeedPosts(r, response.Feed, h.voteService, h.blueskyService)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode post search response: %v", err)
	}
}
