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
	creds, err := a.service.ValidateKey(ctx, plainKey)
	if err != nil {
		return "", err
	}
	return creds.DID, nil
}

// RefreshTokensIfNeeded refreshes OAuth tokens for the aggregator if they are expired
func (a *APIKeyValidatorAdapter) RefreshTokensIfNeeded(ctx context.Context, aggregatorDID string) error {
	creds, err := a.service.GetAggregatorCredentials(ctx, aggregatorDID)
	if err != nil {
		return err
	}

	if creds.APIKeyRevokedAt != nil {
		return aggregators.ErrAPIKeyRevoked
	}

	if creds.APIKeyHash == "" {
		return aggregators.ErrAPIKeyInvalid
	}

	return a.service.RefreshTokensIfNeeded(ctx, creds)
}
