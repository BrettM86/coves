package aggregator

import (
	"errors"
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
)

// CreateAPIKeyHandler handles API key creation for aggregators
type CreateAPIKeyHandler struct {
	apiKeyService     aggregators.APIKeyServiceInterface
	aggregatorService aggregators.Service
}

// NewCreateAPIKeyHandler creates a new handler for API key creation
func NewCreateAPIKeyHandler(apiKeyService aggregators.APIKeyServiceInterface, aggregatorService aggregators.Service) *CreateAPIKeyHandler {
	return &CreateAPIKeyHandler{
		apiKeyService:     apiKeyService,
		aggregatorService: aggregatorService,
	}
}

// CreateAPIKeyResponse represents the response when creating an API key
type CreateAPIKeyResponse struct {
	Key       string `json:"key"`       // The plain-text key (shown ONCE)
	KeyPrefix string `json:"keyPrefix"` // First 12 chars for identification
	DID       string `json:"did"`       // Aggregator DID
	CreatedAt string `json:"createdAt"` // ISO8601 timestamp
}

// HandleCreateAPIKey handles POST /xrpc/social.coves.aggregator.createApiKey
// This endpoint requires OAuth authentication and is only available to registered aggregators.
// The API key is returned ONCE and cannot be retrieved again.
//
// Key Replacement: If an aggregator already has an API key, calling this endpoint will
// generate a new key and replace the existing one. The old key will be immediately
// invalidated and all future requests using the old key will fail authentication.
func (h *CreateAPIKeyHandler) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get authenticated DID from context (set by RequireAuth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthenticationRequired", "Must be authenticated to create API key")
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
		writeError(w, http.StatusForbidden, "AggregatorRequired", "Only registered aggregators can create API keys")
		return
	}

	// Get the OAuth session from context
	oauthSession := middleware.GetOAuthSession(r)
	if oauthSession == nil {
		writeError(w, http.StatusUnauthorized, "OAuthSessionRequired", "OAuth session required to create API key")
		return
	}

	// Generate the API key
	plainKey, keyPrefix, err := h.apiKeyService.GenerateKey(r.Context(), userDID, oauthSession)
	if err != nil {
		log.Printf("ERROR: Failed to generate API key for %s: %v", userDID, err)

		// Differentiate error types for appropriate HTTP status codes
		switch {
		case aggregators.IsNotFound(err):
			// Aggregator not found in database - should not happen if IsAggregator check passed
			writeError(w, http.StatusForbidden, "AggregatorRequired", "User is not a registered aggregator")
		case errors.Is(err, aggregators.ErrOAuthSessionMismatch):
			// OAuth session DID doesn't match the requested aggregator DID
			writeError(w, http.StatusBadRequest, "SessionMismatch", "OAuth session does not match the requested aggregator")
		default:
			// All other errors are internal server errors
			writeError(w, http.StatusInternalServerError, "KeyGenerationFailed", "Failed to generate API key")
		}
		return
	}

	// Return the key (shown ONCE only)
	response := CreateAPIKeyResponse{
		Key:       plainKey,
		KeyPrefix: keyPrefix,
		DID:       userDID,
		CreatedAt: formatTimestamp(),
	}

	writeJSONResponse(w, http.StatusOK, response)
}
