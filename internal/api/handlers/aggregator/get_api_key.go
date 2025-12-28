package aggregator

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"encoding/json"
	"log"
	"net/http"
)

// GetAPIKeyHandler handles API key info retrieval for aggregators
type GetAPIKeyHandler struct {
	apiKeyService     *aggregators.APIKeyService
	aggregatorService aggregators.Service
}

// NewGetAPIKeyHandler creates a new handler for API key info retrieval
func NewGetAPIKeyHandler(apiKeyService *aggregators.APIKeyService, aggregatorService aggregators.Service) *GetAPIKeyHandler {
	return &GetAPIKeyHandler{
		apiKeyService:     apiKeyService,
		aggregatorService: aggregatorService,
	}
}

// APIKeyView represents the nested key metadata (matches social.coves.aggregator.defs#apiKeyView)
type APIKeyView struct {
	Prefix     string  `json:"prefix"`               // First 12 chars for identification
	CreatedAt  string  `json:"createdAt"`            // ISO8601 timestamp when key was created
	LastUsedAt *string `json:"lastUsedAt,omitempty"` // ISO8601 timestamp when key was last used
	IsRevoked  bool    `json:"isRevoked"`            // Whether the key has been revoked
	RevokedAt  *string `json:"revokedAt,omitempty"`  // ISO8601 timestamp when key was revoked
}

// GetAPIKeyResponse represents the response when getting API key info
type GetAPIKeyResponse struct {
	HasKey  bool        `json:"hasKey"`            // Whether the aggregator has an API key
	KeyInfo *APIKeyView `json:"keyInfo,omitempty"` // Key metadata (only present if hasKey is true)
}

// HandleGetAPIKey handles GET /xrpc/social.coves.aggregator.getApiKey
// This endpoint requires OAuth authentication and returns info about the aggregator's API key.
// NOTE: The actual key value is NEVER returned - only metadata about the key.
func (h *GetAPIKeyHandler) HandleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get authenticated DID from context (set by RequireAuth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "Must be authenticated to get API key info")
		return
	}

	// Verify the caller is a registered aggregator
	isAggregator, err := h.aggregatorService.IsAggregator(r.Context(), userDID)
	if err != nil {
		log.Printf("ERROR: Failed to check aggregator status: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to verify aggregator status")
		return
	}
	if !isAggregator {
		writeError(w, http.StatusForbidden, "AggregatorRequired", "Only registered aggregators can get API key info")
		return
	}

	// Get API key info
	keyInfo, err := h.apiKeyService.GetAPIKeyInfo(r.Context(), userDID)
	if err != nil {
		if aggregators.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AggregatorNotFound", "Aggregator not found")
			return
		}
		log.Printf("ERROR: Failed to get API key info for %s: %v", userDID, err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to get API key info")
		return
	}

	// Build response
	response := GetAPIKeyResponse{
		HasKey: keyInfo.HasKey,
	}

	if keyInfo.HasKey {
		view := &APIKeyView{
			Prefix:    keyInfo.KeyPrefix,
			IsRevoked: keyInfo.IsRevoked,
		}

		if keyInfo.CreatedAt != nil {
			view.CreatedAt = keyInfo.CreatedAt.Format("2006-01-02T15:04:05.000Z")
		}

		if keyInfo.LastUsedAt != nil {
			ts := keyInfo.LastUsedAt.Format("2006-01-02T15:04:05.000Z")
			view.LastUsedAt = &ts
		}

		if keyInfo.RevokedAt != nil {
			ts := keyInfo.RevokedAt.Format("2006-01-02T15:04:05.000Z")
			view.RevokedAt = &ts
		}

		response.KeyInfo = view
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("ERROR: Failed to encode response: %v", err)
	}
}
