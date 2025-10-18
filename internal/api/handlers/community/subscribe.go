package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"encoding/json"
	"log"
	"net/http"
	"strings"
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
// Request body: { "community": "did:plc:xxx", "contentVisibility": 3 }
// Note: Per lexicon spec, only DIDs are accepted for the "subject" field (not handles).
func (h *SubscribeHandler) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community         string `json:"community"` // DID only (per lexicon)
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

	// Validate DID format (per lexicon: subject field requires format "did")
	if !strings.HasPrefix(req.Community, "did:") {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"community must be a DID (did:plc:... or did:web:...)")
		return
	}

	// Extract authenticated user DID and access token from request context (injected by auth middleware)
	// Note: contentVisibility defaults and clamping handled by service layer
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	userAccessToken := middleware.GetUserAccessToken(r)
	if userAccessToken == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Missing access token")
		return
	}

	// Subscribe via service (write-forward to PDS)
	subscription, err := h.service.SubscribeToCommunity(r.Context(), userDID, userAccessToken, req.Community, req.ContentVisibility)
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
// Request body: { "community": "did:plc:xxx" }
// Note: Per lexicon spec, only DIDs are accepted (not handles).
func (h *SubscribeHandler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"` // DID only (per lexicon)
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// Validate DID format (per lexicon: subject field requires format "did")
	if !strings.HasPrefix(req.Community, "did:") {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"community must be a DID (did:plc:... or did:web:...)")
		return
	}

	// Extract authenticated user DID and access token from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	userAccessToken := middleware.GetUserAccessToken(r)
	if userAccessToken == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Missing access token")
		return
	}

	// Unsubscribe via service (delete record on PDS)
	err := h.service.UnsubscribeFromCommunity(r.Context(), userDID, userAccessToken, req.Community)
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
