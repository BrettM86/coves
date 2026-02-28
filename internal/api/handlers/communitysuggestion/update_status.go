package communitysuggestion

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communitysuggestions"
	"encoding/json"
	"log"
	"net/http"
)

// UpdateStatusHandler handles updating a community suggestion's status (admin only)
type UpdateStatusHandler struct {
	service   communitysuggestions.Service
	adminDIDs map[string]bool
}

// NewUpdateStatusHandler creates a new update status handler
// adminDIDs is a list of DIDs that can update suggestion status
func NewUpdateStatusHandler(service communitysuggestions.Service, adminDIDs []string) *UpdateStatusHandler {
	var adminMap map[string]bool
	if len(adminDIDs) > 0 {
		adminMap = make(map[string]bool)
		for _, did := range adminDIDs {
			if did != "" { // Skip empty strings
				adminMap[did] = true
			}
		}
		// If all entries were empty, no admins are configured — block all access
		if len(adminMap) == 0 {
			adminMap = nil
			log.Printf("[WARN] All admin DID entries were empty — suggestion status updates will be blocked for all users")
		}
	}
	return &UpdateStatusHandler{
		service:   service,
		adminDIDs: adminMap,
	}
}

// updateStatusInput represents the JSON request body for updating a suggestion's status
type updateStatusInput struct {
	SuggestionID int64  `json:"suggestionId"`
	Status       string `json:"status"`
}

// HandleUpdateStatus updates a community suggestion's status
// POST /xrpc/social.coves.community.suggestion.updateStatus
func (h *UpdateStatusHandler) HandleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Extract authenticated user DID
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Check if user is an admin
	if h.adminDIDs == nil || !h.adminDIDs[userDID] {
		writeError(w, http.StatusForbidden, "Forbidden", "Admin access required")
		return
	}

	// Limit request body size to 10KB
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	// Parse JSON body
	var input updateStatusInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		log.Printf("[COMMUNITY_SUGGESTION] Failed to decode update status JSON request: %v", err)
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Build the update status request
	req := communitysuggestions.UpdateStatusRequest{
		SuggestionID: input.SuggestionID,
		Status:       communitysuggestions.Status(input.Status),
		AdminDID:     userDID,
	}

	// Update status via service
	if err := h.service.UpdateStatus(r.Context(), req); err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"success":true}`)); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}
