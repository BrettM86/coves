package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"encoding/json"
	"log"
	"net/http"
)

// SubscribeHandler handles community subscriptions
type SubscribeHandler struct {
	service communities.Service
}

// NewSubscribeHandler creates a new subscribe handler
func NewSubscribeHandler(service communities.Service) *SubscribeHandler {
	return &SubscribeHandler{
		service: service,
	}
}

// HandleSubscribe subscribes a user to a community
// POST /xrpc/social.coves.community.subscribe
//
// Request body: { "community": "<identifier>", "contentVisibility": 3 }
// Where <identifier> can be:
//   - DID: did:plc:xxx
//   - Canonical handle: c-name.coves.social
//   - Scoped identifier: !name@coves.social
//   - At-identifier: @c-name.coves.social
func (h *SubscribeHandler) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community         string `json:"community"`         // DID, handle, or scoped identifier
		ContentVisibility int    `json:"contentVisibility"` // Optional: 1-5 scale, defaults to 3
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	// The session contains the user's DID and credentials needed for DPoP authentication
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Subscribe via service (write-forward to PDS with DPoP authentication)
	// Service handles identifier resolution (DIDs, handles, scoped identifiers)
	subscription, err := h.service.SubscribeToCommunity(r.Context(), session, req.Community, req.ContentVisibility)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response
	response := map[string]interface{}{
		"uri":      subscription.RecordURI,
		"cid":      subscription.RecordCID,
		"existing": false, // Would be true if already subscribed
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// HandleUnsubscribe unsubscribes a user from a community
// POST /xrpc/social.coves.community.unsubscribe
//
// Request body: { "community": "<identifier>" }
// Where <identifier> can be:
//   - DID: did:plc:xxx
//   - Canonical handle: c-name.coves.social
//   - Scoped identifier: !name@coves.social
//   - At-identifier: @c-name.coves.social
func (h *SubscribeHandler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // DID, handle, or scoped identifier
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Get OAuth session from context (injected by auth middleware)
	// The session contains the user's DID and credentials needed for DPoP authentication
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Unsubscribe via service (delete record on PDS with DPoP authentication)
	// Service handles identifier resolution (DIDs, handles, scoped identifiers)
	err := h.service.UnsubscribeFromCommunity(r.Context(), session, req.Community)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	}); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
