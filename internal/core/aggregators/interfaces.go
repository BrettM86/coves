package aggregators

import (
	"context"
	"time"
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
