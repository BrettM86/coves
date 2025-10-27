package aggregator

import (
	"Coves/internal/core/aggregators"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// GetServicesHandler handles aggregator service details retrieval
type GetServicesHandler struct {
	service aggregators.Service
}

// NewGetServicesHandler creates a new get services handler
func NewGetServicesHandler(service aggregators.Service) *GetServicesHandler {
	return &GetServicesHandler{
		service: service,
	}
}

// HandleGetServices retrieves aggregator details by DID(s)
// GET /xrpc/social.coves.aggregator.getServices?dids=did:plc:abc123,did:plc:def456&detailed=true
// Following Bluesky's pattern: app.bsky.feed.getFeedGenerators
func (h *GetServicesHandler) HandleGetServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse DIDs from query parameter
	didsParam := r.URL.Query().Get("dids")
	if didsParam == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "dids parameter is required")
		return
	}

	// Parse detailed flag (default: false)
	detailed := r.URL.Query().Get("detailed") == "true"

	// Split comma-separated DIDs
	rawDIDs := strings.Split(didsParam, ",")

	// Trim whitespace and filter out empty DIDs (handles double commas, trailing commas, etc.)
	dids := make([]string, 0, len(rawDIDs))
	for _, did := range rawDIDs {
		trimmed := strings.TrimSpace(did)
		if trimmed != "" {
			dids = append(dids, trimmed)
		}
	}

	// Validate we have at least one valid DID
	if len(dids) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "at least one valid DID is required")
		return
	}

	// Get aggregators from service
	aggs, err := h.service.GetAggregators(r.Context(), dids)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build response with appropriate view type based on detailed flag
	response := GetServicesResponse{
		Views: make([]interface{}, 0, len(aggs)),
	}

	for _, agg := range aggs {
		if detailed {
			response.Views = append(response.Views, toAggregatorViewDetailed(agg))
		} else {
			response.Views = append(response.Views, toAggregatorView(agg))
		}
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode getServices response: %v", err)
	}
}

// GetServicesResponse matches the lexicon output
type GetServicesResponse struct {
	Views []interface{} `json:"views"` // Union of aggregatorView | aggregatorViewDetailed
}

// AggregatorView matches social.coves.aggregator.defs#aggregatorView (without stats)
type AggregatorView struct {
	DID           string      `json:"did"`
	DisplayName   string      `json:"displayName"`
	Description   *string     `json:"description,omitempty"`
	Avatar        *string     `json:"avatar,omitempty"`
	ConfigSchema  interface{} `json:"configSchema,omitempty"`
	SourceURL     *string     `json:"sourceUrl,omitempty"`
	MaintainerDID *string     `json:"maintainer,omitempty"`
	CreatedAt     string      `json:"createdAt"`
	RecordUri     string      `json:"recordUri"`
}

// AggregatorViewDetailed matches social.coves.aggregator.defs#aggregatorViewDetailed (with stats)
type AggregatorViewDetailed struct {
	DID           string          `json:"did"`
	DisplayName   string          `json:"displayName"`
	Description   *string         `json:"description,omitempty"`
	Avatar        *string         `json:"avatar,omitempty"`
	ConfigSchema  interface{}     `json:"configSchema,omitempty"`
	SourceURL     *string         `json:"sourceUrl,omitempty"`
	MaintainerDID *string         `json:"maintainer,omitempty"`
	CreatedAt     string          `json:"createdAt"`
	RecordUri     string          `json:"recordUri"`
	Stats         AggregatorStats `json:"stats"`
}

// AggregatorStats matches social.coves.aggregator.defs#aggregatorStats
type AggregatorStats struct {
	CommunitiesUsing int `json:"communitiesUsing"`
	PostsCreated     int `json:"postsCreated"`
}

// toAggregatorView converts domain model to basic aggregatorView (no stats)
func toAggregatorView(agg *aggregators.Aggregator) AggregatorView {
	view := AggregatorView{
		DID:         agg.DID,
		DisplayName: agg.DisplayName,
		CreatedAt:   agg.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
		RecordUri:   agg.RecordURI,
	}

	// Add optional fields
	if agg.Description != "" {
		view.Description = &agg.Description
	}
	if agg.AvatarURL != "" {
		view.Avatar = &agg.AvatarURL
	}
	if agg.MaintainerDID != "" {
		view.MaintainerDID = &agg.MaintainerDID
	}
	if agg.SourceURL != "" {
		view.SourceURL = &agg.SourceURL
	}
	if len(agg.ConfigSchema) > 0 {
		// ConfigSchema is already JSON, unmarshal it for the view
		var schema interface{}
		if err := json.Unmarshal(agg.ConfigSchema, &schema); err == nil {
			view.ConfigSchema = schema
		}
	}

	return view
}

// toAggregatorViewDetailed converts domain model to detailed aggregatorViewDetailed (with stats)
func toAggregatorViewDetailed(agg *aggregators.Aggregator) AggregatorViewDetailed {
	view := AggregatorViewDetailed{
		DID:         agg.DID,
		DisplayName: agg.DisplayName,
		CreatedAt:   agg.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
		RecordUri:   agg.RecordURI,
		Stats: AggregatorStats{
			CommunitiesUsing: agg.CommunitiesUsing,
			PostsCreated:     agg.PostsCreated,
		},
	}

	// Add optional fields
	if agg.Description != "" {
		view.Description = &agg.Description
	}
	if agg.AvatarURL != "" {
		view.Avatar = &agg.AvatarURL
	}
	if agg.MaintainerDID != "" {
		view.MaintainerDID = &agg.MaintainerDID
	}
	if agg.SourceURL != "" {
		view.SourceURL = &agg.SourceURL
	}
	if len(agg.ConfigSchema) > 0 {
		// ConfigSchema is already JSON, unmarshal it for the view
		var schema interface{}
		if err := json.Unmarshal(agg.ConfigSchema, &schema); err == nil {
			view.ConfigSchema = schema
		}
	}

	return view
}
