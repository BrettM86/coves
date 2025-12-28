package aggregator

import (
	"log"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
)

// RevokeAPIKeyHandler handles API key revocation for aggregators
type RevokeAPIKeyHandler struct {
	apiKeyService     aggregators.APIKeyServiceInterface
	aggregatorService aggregators.Service
}

// NewRevokeAPIKeyHandler creates a new handler for API key revocation
func NewRevokeAPIKeyHandler(apiKeyService aggregators.APIKeyServiceInterface, aggregatorService aggregators.Service) *RevokeAPIKeyHandler {
	return &RevokeAPIKeyHandler{
		apiKeyService:     apiKeyService,
		aggregatorService: aggregatorService,
	}
}

// RevokeAPIKeyResponse represents the response when revoking an API key
type RevokeAPIKeyResponse struct {
	RevokedAt string `json:"revokedAt"` // ISO8601 timestamp when key was revoked
}

// HandleRevokeAPIKey handles POST /xrpc/social.coves.aggregator.revokeApiKey
// This endpoint requires OAuth authentication and revokes the aggregator's current API key.
// After revocation, the aggregator must complete OAuth flow again to get a new key.
func (h *RevokeAPIKeyHandler) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get authenticated DID from context (set by RequireAuth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "Must be authenticated to revoke API key")
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
		writeError(w, http.StatusForbidden, "AggregatorRequired", "Only registered aggregators can revoke API keys")
		return
	}

	// Check if the aggregator has an API key to revoke
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

	if !keyInfo.HasKey {
		writeError(w, http.StatusBadRequest, "ApiKeyNotFound", "No API key exists to revoke")
		return
	}

	if keyInfo.IsRevoked {
		writeError(w, http.StatusBadRequest, "ApiKeyAlreadyRevoked", "API key has already been revoked")
		return
	}

	// Revoke the API key
	if err := h.apiKeyService.RevokeKey(r.Context(), userDID); err != nil {
		log.Printf("ERROR: Failed to revoke API key for %s: %v", userDID, err)
		writeError(w, http.StatusInternalServerError, "RevocationFailed", "Failed to revoke API key")
		return
	}

	// Return success
	response := RevokeAPIKeyResponse{
		RevokedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	writeJSONResponse(w, http.StatusOK, response)
}

// formatTimestamp returns current time in ISO8601 format
func formatTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
