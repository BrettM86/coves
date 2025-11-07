package timeline

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"Coves/internal/api/middleware"
	"Coves/internal/core/timeline"
)

// GetTimelineHandler handles timeline feed retrieval
type GetTimelineHandler struct {
	service timeline.Service
}

// NewGetTimelineHandler creates a new timeline handler
func NewGetTimelineHandler(service timeline.Service) *GetTimelineHandler {
	return &GetTimelineHandler{
		service: service,
	}
}

// HandleGetTimeline retrieves posts from all communities the user subscribes to
// GET /xrpc/social.coves.feed.getTimeline?sort=hot&limit=15&cursor=...
// Requires authentication (user must be logged in)
func (h *GetTimelineHandler) HandleGetTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract authenticated user DID from context (set by RequireAuth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" || !strings.HasPrefix(userDID, "did:") {
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "User must be authenticated to view timeline")
		return
	}

	// Parse query parameters
	req, err := h.parseRequest(r, userDID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Get timeline
	response, err := h.service.GetTimeline(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return feed
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		log.Printf("ERROR: Failed to encode timeline response: %v", err)
	}
}

// parseRequest parses query parameters into GetTimelineRequest
func (h *GetTimelineHandler) parseRequest(r *http.Request, userDID string) (timeline.GetTimelineRequest, error) {
	req := timeline.GetTimelineRequest{
		UserDID: userDID, // Set from authenticated context
	}

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
