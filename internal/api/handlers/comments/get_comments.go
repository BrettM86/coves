// Package comments provides HTTP handlers for the comment query API.
// These handlers follow XRPC conventions and integrate with the comments service layer.
package comments

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// GetCommentsHandler handles comment retrieval for posts
type GetCommentsHandler struct {
	service Service
}

// Service defines the interface for comment business logic
// This will be implemented by the comments service layer in Phase 2
type Service interface {
	GetComments(r *http.Request, req *GetCommentsRequest) (*comments.GetCommentsResponse, error)
}

// GetCommentsRequest represents the query parameters for fetching comments
// Matches social.coves.feed.getComments lexicon input
type GetCommentsRequest struct {
	PostURI   string  `json:"post"`              // Required: AT-URI of the post
	Sort      string  `json:"sort,omitempty"`    // Optional: "hot", "top", "new" (default: "hot")
	Timeframe string  `json:"timeframe,omitempty"` // Optional: For "top" sort - "hour", "day", "week", "month", "year", "all"
	Depth     int     `json:"depth,omitempty"`   // Optional: Max nesting depth (default: 10)
	Limit     int     `json:"limit,omitempty"`   // Optional: Max comments per page (default: 50, max: 100)
	Cursor    *string `json:"cursor,omitempty"`  // Optional: Pagination cursor
	ViewerDID *string `json:"-"`                 // Internal: Extracted from auth token
}

// NewGetCommentsHandler creates a new handler for fetching comments
func NewGetCommentsHandler(service Service) *GetCommentsHandler {
	return &GetCommentsHandler{
		service: service,
	}
}

// HandleGetComments handles GET /xrpc/social.coves.feed.getComments
// Retrieves comments on a post with threading support
func (h *GetCommentsHandler) HandleGetComments(w http.ResponseWriter, r *http.Request) {
	// 1. Only allow GET method
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Parse query parameters
	query := r.URL.Query()
	post := query.Get("post")
	sort := query.Get("sort")
	timeframe := query.Get("timeframe")
	depthStr := query.Get("depth")
	limitStr := query.Get("limit")
	cursor := query.Get("cursor")

	// 3. Validate required parameters
	if post == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "post parameter is required")
		return
	}

	// 4. Parse and validate depth with default
	depth := 10 // Default depth
	if depthStr != "" {
		parsed, err := strconv.Atoi(depthStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "depth must be a valid integer")
			return
		}
		if parsed < 0 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "depth must be non-negative")
			return
		}
		depth = parsed
	}

	// 5. Parse and validate limit with default and max
	limit := 50 // Default limit
	if limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "limit must be a valid integer")
			return
		}
		if parsed < 1 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "limit must be positive")
			return
		}
		if parsed > 100 {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "limit cannot exceed 100")
			return
		}
		limit = parsed
	}

	// 6. Validate sort parameter (if provided)
	if sort != "" && sort != "hot" && sort != "top" && sort != "new" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"sort must be one of: hot, top, new")
		return
	}

	// 7. Validate timeframe parameter (only valid with "top" sort)
	if timeframe != "" {
		if sort != "top" {
			writeError(w, http.StatusBadRequest, "InvalidRequest",
				"timeframe can only be used with sort=top")
			return
		}
		validTimeframes := map[string]bool{
			"hour": true, "day": true, "week": true,
			"month": true, "year": true, "all": true,
		}
		if !validTimeframes[timeframe] {
			writeError(w, http.StatusBadRequest, "InvalidRequest",
				"timeframe must be one of: hour, day, week, month, year, all")
			return
		}
	}

	// 8. Extract viewer DID from context (set by OptionalAuth middleware)
	viewerDID := middleware.GetUserDID(r)
	var viewerPtr *string
	if viewerDID != "" {
		viewerPtr = &viewerDID
	}

	// 9. Build service request
	req := &GetCommentsRequest{
		PostURI:   post,
		Sort:      sort,
		Timeframe: timeframe,
		Depth:     depth,
		Limit:     limit,
		Cursor:    ptrOrNil(cursor),
		ViewerDID: viewerPtr,
	}

	// 10. Call service layer
	resp, err := h.service.GetComments(r, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 11. Return JSON response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		log.Printf("Failed to encode comments response: %v", err)
	}
}

// ptrOrNil converts an empty string to nil pointer, otherwise returns pointer to string
func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
