package aggregator

import (
	"Coves/internal/core/aggregators"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// GetAuthorizationsHandler handles listing authorizations for an aggregator
type GetAuthorizationsHandler struct {
	service aggregators.Service
}

// NewGetAuthorizationsHandler creates a new get authorizations handler
func NewGetAuthorizationsHandler(service aggregators.Service) *GetAuthorizationsHandler {
	return &GetAuthorizationsHandler{
		service: service,
	}
}

// HandleGetAuthorizations lists all communities that authorized an aggregator
// GET /xrpc/social.coves.aggregator.getAuthorizations?aggregatorDid=did:plc:abc123&enabledOnly=true&limit=50&cursor=xyz
// Following Bluesky's pattern for listing feed subscribers
func (h *GetAuthorizationsHandler) HandleGetAuthorizations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request
	req, err := h.parseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Get aggregator details first (needed for nested aggregator object in response)
	agg, err := h.service.GetAggregator(r.Context(), req.AggregatorDID)
	if err != nil {
		if aggregators.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AggregatorNotFound", "Aggregator DID does not exist or has no service declaration")
			return
		}
		handleServiceError(w, err)
		return
	}

	// Get authorizations from service
	auths, err := h.service.GetAuthorizationsForAggregator(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build response
	response := GetAuthorizationsResponse{
		Authorizations: make([]CommunityAuthView, 0, len(auths)),
	}

	// Convert aggregator to view for nesting in each authorization
	aggregatorView := toAggregatorView(agg)

	for _, auth := range auths {
		response.Authorizations = append(response.Authorizations, toCommunityAuthView(auth, aggregatorView))
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode getAuthorizations response: %v", err)
	}
}

// parseRequest parses query parameters
func (h *GetAuthorizationsHandler) parseRequest(r *http.Request) (aggregators.GetAuthorizationsRequest, error) {
	req := aggregators.GetAuthorizationsRequest{}

	// Required: aggregatorDid
	req.AggregatorDID = r.URL.Query().Get("aggregatorDid")

	// Optional: enabledOnly (default: false)
	if enabledOnlyStr := r.URL.Query().Get("enabledOnly"); enabledOnlyStr == "true" {
		req.EnabledOnly = true
	}

	// Optional: limit (default: 50, set by service)
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil {
			req.Limit = limit
		}
	}

	// Optional: offset (default: 0)
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if offset, err := strconv.Atoi(offsetStr); err == nil {
			req.Offset = offset
		}
	}

	return req, nil
}

// GetAuthorizationsResponse matches the lexicon output
type GetAuthorizationsResponse struct {
	Cursor         *string             `json:"cursor,omitempty"`
	Authorizations []CommunityAuthView `json:"authorizations"`
}

// CommunityAuthView matches social.coves.aggregator.defs#communityAuthView
// Shows authorization from aggregator's perspective with nested aggregator details
type CommunityAuthView struct {
	Config     interface{}    `json:"config,omitempty"`
	Aggregator AggregatorView `json:"aggregator"`
	CreatedAt  string         `json:"createdAt"`
	RecordUri  string         `json:"recordUri,omitempty"`
	Enabled    bool           `json:"enabled"`
}

// toCommunityAuthView converts domain model to API view
func toCommunityAuthView(auth *aggregators.Authorization, aggregatorView AggregatorView) CommunityAuthView {
	view := CommunityAuthView{
		Aggregator: aggregatorView, // Nested aggregator object
		Enabled:    auth.Enabled,
		CreatedAt:  auth.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
	}

	// Add optional fields
	if len(auth.Config) > 0 {
		// Config is JSONB, unmarshal it
		var config interface{}
		if err := json.Unmarshal(auth.Config, &config); err == nil {
			view.Config = config
		}
	}
	if auth.RecordURI != "" {
		view.RecordUri = auth.RecordURI
	}

	return view
}
