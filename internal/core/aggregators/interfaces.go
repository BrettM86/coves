package aggregators

import (
	"context"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// Repository defines the interface for aggregator data persistence
// This is the AppView's indexed view of aggregators and authorizations from the firehose
type Repository interface {
	// Aggregator CRUD (indexed from firehose)
	CreateAggregator(ctx context.Context, aggregator *Aggregator) error
	GetAggregator(ctx context.Context, did string) (*Aggregator, error)
	GetAggregatorsByDIDs(ctx context.Context, dids []string) ([]*Aggregator, error) // Bulk fetch to avoid N+1 queries
	UpdateAggregator(ctx context.Context, aggregator *Aggregator) error
	DeleteAggregator(ctx context.Context, did string) error
	ListAggregators(ctx context.Context, limit, offset int) ([]*Aggregator, error)
	IsAggregator(ctx context.Context, did string) (bool, error) // Fast check for post creation handler

	// Authorization CRUD (indexed from firehose)
	CreateAuthorization(ctx context.Context, auth *Authorization) error
	GetAuthorization(ctx context.Context, aggregatorDID, communityDID string) (*Authorization, error)
	GetAuthorizationByURI(ctx context.Context, recordURI string) (*Authorization, error) // For Jetstream delete operations
	UpdateAuthorization(ctx context.Context, auth *Authorization) error
	DeleteAuthorization(ctx context.Context, aggregatorDID, communityDID string) error
	DeleteAuthorizationByURI(ctx context.Context, recordURI string) error // For Jetstream delete operations

	// Authorization queries
	ListAuthorizationsForAggregator(ctx context.Context, aggregatorDID string, enabledOnly bool, limit, offset int) ([]*Authorization, error)
	ListAuthorizationsForCommunity(ctx context.Context, communityDID string, enabledOnly bool, limit, offset int) ([]*Authorization, error)
	IsAuthorized(ctx context.Context, aggregatorDID, communityDID string) (bool, error) // Fast check: enabled=true

	// Post tracking (for rate limiting and stats)
	RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error
	CountRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) (int, error)
	GetRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) ([]*AggregatorPost, error)

	// API Key Authentication
	// GetByAPIKeyHash looks up an aggregator by their API key hash for authentication
	GetByAPIKeyHash(ctx context.Context, keyHash string) (*Aggregator, error)
	// GetAggregatorCredentials retrieves only the credential fields for an aggregator.
	// Used by APIKeyService for authentication operations where full aggregator is not needed.
	GetAggregatorCredentials(ctx context.Context, did string) (*AggregatorCredentials, error)
	// GetCredentialsByAPIKeyHash looks up aggregator credentials by their API key hash.
	// Returns ErrAPIKeyRevoked if the key has been revoked.
	// Returns ErrAPIKeyInvalid if no aggregator found with that hash.
	GetCredentialsByAPIKeyHash(ctx context.Context, keyHash string) (*AggregatorCredentials, error)
	// SetAPIKey stores API key credentials and OAuth session for an aggregator
	SetAPIKey(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error
	// UpdateOAuthTokens updates OAuth tokens after a refresh operation
	UpdateOAuthTokens(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error
	// UpdateOAuthNonces updates DPoP nonces after token operations
	UpdateOAuthNonces(ctx context.Context, did, authServerNonce, pdsNonce string) error
	// UpdateAPIKeyLastUsed updates the last_used_at timestamp for audit purposes
	UpdateAPIKeyLastUsed(ctx context.Context, did string) error
	// RevokeAPIKey marks an API key as revoked (sets api_key_revoked_at)
	RevokeAPIKey(ctx context.Context, did string) error

	// ListAggregatorsNeedingTokenRefresh returns aggregators with active API keys
	// whose OAuth tokens expire within the given buffer period
	ListAggregatorsNeedingTokenRefresh(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error)
}

// Service defines the interface for aggregator business logic
// Coordinates between Repository, communities service, and PDS for write-forward
type Service interface {
	// Aggregator queries (read from AppView)
	GetAggregator(ctx context.Context, did string) (*Aggregator, error)
	GetAggregators(ctx context.Context, dids []string) ([]*Aggregator, error)
	ListAggregators(ctx context.Context, limit, offset int) ([]*Aggregator, error)

	// Authorization queries (read from AppView)
	GetAuthorizationsForAggregator(ctx context.Context, req GetAuthorizationsRequest) ([]*Authorization, error)
	ListAggregatorsForCommunity(ctx context.Context, req ListForCommunityRequest) ([]*Authorization, error)

	// Authorization management (write-forward: Service -> PDS -> Firehose -> Consumer -> Repository)
	EnableAggregator(ctx context.Context, req EnableAggregatorRequest) (*Authorization, error)
	DisableAggregator(ctx context.Context, req DisableAggregatorRequest) (*Authorization, error)
	UpdateAggregatorConfig(ctx context.Context, req UpdateConfigRequest) (*Authorization, error)

	// Validation and authorization checks (used by post creation handler)
	ValidateAggregatorPost(ctx context.Context, aggregatorDID, communityDID string) error // Checks authorization + rate limits
	IsAggregator(ctx context.Context, did string) (bool, error)                           // Check if DID is a registered aggregator

	// Post tracking (called after successful post creation)
	RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error
}

// APIKeyServiceInterface defines the interface for API key operations used by handlers.
// This interface enables easier testing by allowing mock implementations.
type APIKeyServiceInterface interface {
	// GenerateKey creates a new API key for an aggregator.
	// Returns the plain-text key (only shown once) and the key prefix for reference.
	GenerateKey(ctx context.Context, aggregatorDID string, oauthSession *oauth.ClientSessionData) (plainKey string, keyPrefix string, err error)

	// GetAPIKeyInfo returns information about an aggregator's API key (without the actual key).
	GetAPIKeyInfo(ctx context.Context, aggregatorDID string) (*APIKeyInfo, error)

	// RevokeKey revokes an API key for an aggregator.
	RevokeKey(ctx context.Context, aggregatorDID string) error

	// GetFailedLastUsedUpdates returns the count of failed last_used timestamp updates.
	GetFailedLastUsedUpdates() int64

	// GetFailedNonceUpdates returns the count of failed OAuth nonce updates.
	GetFailedNonceUpdates() int64
}
