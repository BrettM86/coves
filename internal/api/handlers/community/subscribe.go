package community

import (
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
// Body: { "community": "did:plc:xxx" or "!gaming@coves.social" }
func (h *SubscribeHandler) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// TODO(Communities-OAuth): Extract authenticated user DID from request context
	// This MUST be replaced with OAuth middleware before production deployment
	// Expected implementation:
	//   userDID := r.Context().Value("authenticated_user_did").(string)
	// For now, we read from header (INSECURE - allows impersonation)
	userDID := r.Header.Get("X-User-DID")
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Subscribe via service (write-forward to PDS)
	subscription, err := h.service.SubscribeToCommunity(r.Context(), userDID, req.Community)
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
// Body: { "community": "did:plc:xxx" or "!gaming@coves.social" }
func (h *SubscribeHandler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Community string `json:"community"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// TODO(Communities-OAuth): Extract authenticated user DID from request context
	// This MUST be replaced with OAuth middleware before production deployment
	// Expected implementation:
	//   userDID := r.Context().Value("authenticated_user_did").(string)
	// For now, we read from header (INSECURE - allows impersonation)
	userDID := r.Header.Get("X-User-DID")
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Unsubscribe via service (delete record on PDS)
	err := h.service.UnsubscribeFromCommunity(r.Context(), userDID, req.Community)
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
