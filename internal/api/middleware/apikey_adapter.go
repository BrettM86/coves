package middleware

import (
	"Coves/internal/core/aggregators"
	"context"
)

// APIKeyValidatorAdapter adapts the aggregators.APIKeyService to the middleware.APIKeyValidator interface
type APIKeyValidatorAdapter struct {
	service *aggregators.APIKeyService
}

// NewAPIKeyValidatorAdapter creates a new adapter for API key validation
func NewAPIKeyValidatorAdapter(service *aggregators.APIKeyService) *APIKeyValidatorAdapter {
	return &APIKeyValidatorAdapter{
		service: service,
	}
}

// ValidateKey validates an API key and returns the aggregator DID if valid
func (a *APIKeyValidatorAdapter) ValidateKey(ctx context.Context, plainKey string) (string, error) {
	aggregator, err := a.service.ValidateKey(ctx, plainKey)
	if err != nil {
		return "", err
	}
	return aggregator.DID, nil
}

// RefreshTokensIfNeeded refreshes OAuth tokens for the aggregator if they are expired
func (a *APIKeyValidatorAdapter) RefreshTokensIfNeeded(ctx context.Context, aggregatorDID string) error {
	// Get the full aggregator object needed for token refresh
	// Note: This is a second database lookup after ValidateKey. In practice, we may want to cache
	// the aggregator data from ValidateKey to avoid this. For now, we accept the extra lookup
	// since token refresh is not on the hot path.
	aggregator, err := a.service.GetAggregator(ctx, aggregatorDID)
	if err != nil {
		return err
	}

	// If API key is revoked, return an error - don't silently allow continuation
	if aggregator.APIKeyRevokedAt != nil {
		return aggregators.ErrAPIKeyRevoked
	}

	// If no API key exists, return an error
	if aggregator.APIKeyHash == "" {
		return aggregators.ErrAPIKeyInvalid
	}

	// Call the actual token refresh on the service
	return a.service.RefreshTokensIfNeeded(ctx, aggregator)
}
