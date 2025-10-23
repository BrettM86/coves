package aggregator

import (
	"Coves/internal/core/aggregators"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// ListForCommunityHandler handles listing aggregators for a community
type ListForCommunityHandler struct {
	service aggregators.Service
}

// NewListForCommunityHandler creates a new list for community handler
func NewListForCommunityHandler(service aggregators.Service) *ListForCommunityHandler {
	return &ListForCommunityHandler{
		service: service,
	}
}

// HandleListForCommunity lists all aggregators authorized by a community
// GET /xrpc/social.coves.aggregator.listForCommunity?community=did:plc:xyz789&enabledOnly=true&limit=50&cursor=xyz
// Used by community settings UI to manage aggregators
func (h *ListForCommunityHandler) HandleListForCommunity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request
	req, communityIdentifier, err := h.parseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Resolve community identifier to DID (handles both DIDs and handles)
	// TODO: Implement identifier resolution service - for now, assume it's a DID
	req.CommunityDID = communityIdentifier

	// Get authorizations from service
	// Note: Community handle/name fields will be empty until we integrate with communities service
	// This is acceptable for alpha - clients can resolve community details separately if needed
	auths, err := h.service.ListAggregatorsForCommunity(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Build response
	response := ListForCommunityResponse{
		Aggregators: make([]AuthorizationView, 0, len(auths)),
	}

	for _, auth := range auths {
		response.Aggregators = append(response.Aggregators, toAuthorizationView(auth, req.CommunityDID))
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode listForCommunity response: %v", err)
	}
}

// parseRequest parses query parameters and returns request + community identifier
func (h *ListForCommunityHandler) parseRequest(r *http.Request) (aggregators.ListForCommunityRequest, string, error) {
	req := aggregators.ListForCommunityRequest{}

	// Required: community (at-identifier: DID or handle)
	communityIdentifier := r.URL.Query().Get("community")
	if communityIdentifier == "" {
		return req, "", writeErrorMsg("community parameter is required")
	}

	// Optional: enabledOnly (default: false per lexicon)
	if enabledOnlyStr := r.URL.Query().Get("enabledOnly"); enabledOnlyStr == "true" {
		req.EnabledOnly = true
	}

	// Optional: limit (default: 50, set by service)
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil {
			req.Limit = limit
		}
	}

	// TODO: Add cursor-based pagination support
	// if cursor := r.URL.Query().Get("cursor"); cursor != "" {
	//     req.Cursor = cursor
	// }

	return req, communityIdentifier, nil
}

// writeErrorMsg creates an error for returning
func writeErrorMsg(msg string) error {
	return &requestError{Message: msg}
}

type requestError struct {
	Message string
}

func (e *requestError) Error() string {
	return e.Message
}

// ListForCommunityResponse matches the lexicon output
type ListForCommunityResponse struct {
	Aggregators []AuthorizationView `json:"aggregators"`
	Cursor      *string             `json:"cursor,omitempty"` // Pagination cursor
}

// AuthorizationView matches social.coves.aggregator.defs#authorizationView
// Shows authorization from community's perspective
type AuthorizationView struct {
	AggregatorDID   string      `json:"aggregatorDid"`
	CommunityDID    string      `json:"communityDid"`
	CommunityHandle *string     `json:"communityHandle,omitempty"` // Optional: populated when communities service integration is complete
	CommunityName   *string     `json:"communityName,omitempty"`   // Optional: populated when communities service integration is complete
	Enabled         bool        `json:"enabled"`
	Config          interface{} `json:"config,omitempty"`
	CreatedAt       string      `json:"createdAt"` // REQUIRED
	CreatedBy       *string     `json:"createdBy,omitempty"`
	DisabledAt      *string     `json:"disabledAt,omitempty"`
	DisabledBy      *string     `json:"disabledBy,omitempty"`
	RecordUri       string      `json:"recordUri,omitempty"`
}

// toAuthorizationView converts domain model to API view
// communityHandle and communityName are left nil until communities service integration is complete
func toAuthorizationView(auth *aggregators.Authorization, communityDID string) AuthorizationView {
	// Safety check for nil authorization
	if auth == nil {
		return AuthorizationView{}
	}

	view := AuthorizationView{
		AggregatorDID: auth.AggregatorDID,
		CommunityDID:  communityDID,
		// CommunityHandle and CommunityName left nil - TODO: fetch from communities service
		Enabled:   auth.Enabled,
		CreatedAt: auth.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
	}

	// Add optional fields
	if len(auth.Config) > 0 {
		// Config is JSONB, unmarshal it
		var config interface{}
		if err := json.Unmarshal(auth.Config, &config); err == nil {
			view.Config = config
		}
	}
	if auth.CreatedBy != "" {
		view.CreatedBy = &auth.CreatedBy
	}
	if auth.DisabledAt != nil && !auth.DisabledAt.IsZero() {
		disabledAt := auth.DisabledAt.Format("2006-01-02T15:04:05.000Z")
		view.DisabledAt = &disabledAt
	}
	if auth.DisabledBy != "" {
		view.DisabledBy = &auth.DisabledBy
	}
	if auth.RecordURI != "" {
		view.RecordUri = auth.RecordURI
	}

	return view
}
