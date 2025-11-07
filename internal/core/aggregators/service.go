package aggregators

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"Coves/internal/core/communities"

	"github.com/xeipuuv/gojsonschema"
)

// Rate limit constants
const (
	RateLimitWindow   = 1 * time.Hour // Rolling 1-hour window for rate limit enforcement
	RateLimitMaxPosts = 10            // Conservative limit for alpha: 10 posts/hour per community (prevents spam while allowing real-time updates)
	DefaultQueryLimit = 50            // Balance between UX (reasonable page size) and server load
	MaxQueryLimit     = 100           // Prevent abuse while allowing batch operations (e.g., fetching multiple aggregators at once)
)

type aggregatorService struct {
	repo             Repository
	communityService communities.Service
}

// NewAggregatorService creates a new aggregator service
func NewAggregatorService(repo Repository, communityService communities.Service) Service {
	return &aggregatorService{
		repo:             repo,
		communityService: communityService,
	}
}

// ===== Query Operations (Read from AppView) =====

// GetAggregator retrieves a single aggregator by DID
func (s *aggregatorService) GetAggregator(ctx context.Context, did string) (*Aggregator, error) {
	if did == "" {
		return nil, NewValidationError("did", "DID is required")
	}

	return s.repo.GetAggregator(ctx, did)
}

// GetAggregators retrieves multiple aggregators by DIDs
func (s *aggregatorService) GetAggregators(ctx context.Context, dids []string) ([]*Aggregator, error) {
	if len(dids) == 0 {
		return []*Aggregator{}, nil
	}

	if len(dids) > MaxQueryLimit {
		return nil, NewValidationError("dids", fmt.Sprintf("maximum %d DIDs allowed", MaxQueryLimit))
	}

	// Use bulk fetch to avoid N+1 queries
	return s.repo.GetAggregatorsByDIDs(ctx, dids)
}

// ListAggregators retrieves all aggregators with pagination
func (s *aggregatorService) ListAggregators(ctx context.Context, limit, offset int) ([]*Aggregator, error) {
	// Apply defaults and limits
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}
	if offset < 0 {
		offset = 0
	}

	return s.repo.ListAggregators(ctx, limit, offset)
}

// GetAuthorizationsForAggregator retrieves all communities that authorized an aggregator
func (s *aggregatorService) GetAuthorizationsForAggregator(ctx context.Context, req GetAuthorizationsRequest) ([]*Authorization, error) {
	if req.AggregatorDID == "" {
		return nil, NewValidationError("aggregatorDid", "aggregator DID is required")
	}

	// Apply defaults and limits
	if req.Limit <= 0 {
		req.Limit = DefaultQueryLimit
	}
	if req.Limit > MaxQueryLimit {
		req.Limit = MaxQueryLimit
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	return s.repo.ListAuthorizationsForAggregator(ctx, req.AggregatorDID, req.EnabledOnly, req.Limit, req.Offset)
}

// ListAggregatorsForCommunity retrieves all aggregators authorized by a community
func (s *aggregatorService) ListAggregatorsForCommunity(ctx context.Context, req ListForCommunityRequest) ([]*Authorization, error) {
	if req.CommunityDID == "" {
		return nil, NewValidationError("communityDid", "community DID is required")
	}

	// Apply defaults and limits
	if req.Limit <= 0 {
		req.Limit = DefaultQueryLimit
	}
	if req.Limit > MaxQueryLimit {
		req.Limit = MaxQueryLimit
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	return s.repo.ListAuthorizationsForCommunity(ctx, req.CommunityDID, req.EnabledOnly, req.Limit, req.Offset)
}

// ===== Authorization Management (Write-forward to PDS) =====

// EnableAggregator creates an authorization record for an aggregator in a community
// Following Bluesky's pattern: similar to enabling a labeler or feed generator
// Note: This is a PLACEHOLDER for the write-forward implementation
// TODO: Implement actual XRPC write to community's PDS repository
func (s *aggregatorService) EnableAggregator(ctx context.Context, req EnableAggregatorRequest) (*Authorization, error) {
	// Validate request
	if err := s.validateEnableRequest(ctx, req); err != nil {
		return nil, err
	}

	// Verify aggregator exists
	aggregator, err := s.repo.GetAggregator(ctx, req.AggregatorDID)
	if err != nil {
		return nil, err
	}

	// Validate config against aggregator's schema if provided
	if len(req.Config) > 0 && len(aggregator.ConfigSchema) > 0 {
		if err := s.validateConfig(req.Config, aggregator.ConfigSchema); err != nil {
			return nil, err
		}
	}

	// Check if already authorized
	existing, err := s.repo.GetAuthorization(ctx, req.AggregatorDID, req.CommunityDID)
	if err == nil && existing.Enabled {
		return nil, ErrAlreadyAuthorized
	}

	// TODO Phase 2: Write-forward to PDS
	// For now, return placeholder response
	// The actual implementation will:
	// 1. Create authorization record in community's repository on PDS
	// 2. Wait for Jetstream to index it
	// 3. Return the indexed authorization
	//
	// Record structure:
	// at://community_did/social.coves.aggregator.authorization/{rkey}
	// {
	//   "$type": "social.coves.aggregator.authorization",
	//   "aggregator": req.AggregatorDID,
	//   "enabled": true,
	//   "config": req.Config,
	//   "createdBy": req.EnabledByDID,
	//   "createdAt": "2025-10-20T12:00:00Z"
	// }

	return nil, ErrNotImplemented
}

// DisableAggregator updates an authorization to disabled
// Note: This is a PLACEHOLDER for the write-forward implementation
func (s *aggregatorService) DisableAggregator(ctx context.Context, req DisableAggregatorRequest) (*Authorization, error) {
	// Validate request
	if err := s.validateDisableRequest(ctx, req); err != nil {
		return nil, err
	}

	// Verify authorization exists
	auth, err := s.repo.GetAuthorization(ctx, req.AggregatorDID, req.CommunityDID)
	if err != nil {
		return nil, err
	}

	if !auth.Enabled {
		// Already disabled
		return auth, nil
	}

	// TODO Phase 2: Write-forward to PDS
	// Update the authorization record with enabled=false
	return nil, ErrNotImplemented
}

// UpdateAggregatorConfig updates an aggregator's configuration
// Note: This is a PLACEHOLDER for the write-forward implementation
func (s *aggregatorService) UpdateAggregatorConfig(ctx context.Context, req UpdateConfigRequest) (*Authorization, error) {
	// Validate request
	if err := s.validateUpdateConfigRequest(ctx, req); err != nil {
		return nil, err
	}

	// Verify authorization exists
	auth, err := s.repo.GetAuthorization(ctx, req.AggregatorDID, req.CommunityDID)
	if err != nil {
		return nil, err
	}

	// Get aggregator for schema validation
	aggregator, err := s.repo.GetAggregator(ctx, req.AggregatorDID)
	if err != nil {
		return nil, err
	}

	// Validate new config against schema
	if len(req.Config) > 0 && len(aggregator.ConfigSchema) > 0 {
		if err := s.validateConfig(req.Config, aggregator.ConfigSchema); err != nil {
			return nil, err
		}
	}

	// TODO Phase 2: Write-forward to PDS
	// Update the authorization record with new config
	return auth, ErrNotImplemented
}

// ===== Validation and Authorization Checks =====

// ValidateAggregatorPost validates that an aggregator can post to a community
// Checks: 1) Authorization exists and is enabled, 2) Rate limit not exceeded
// This is called by the post creation handler BEFORE writing to PDS
func (s *aggregatorService) ValidateAggregatorPost(ctx context.Context, aggregatorDID, communityDID string) error {
	// Check authorization exists and is enabled
	authorized, err := s.repo.IsAuthorized(ctx, aggregatorDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to check authorization: %w", err)
	}
	if !authorized {
		return ErrNotAuthorized
	}

	// Check rate limit (10 posts per hour per community)
	since := time.Now().Add(-RateLimitWindow)
	recentPostCount, err := s.repo.CountRecentPosts(ctx, aggregatorDID, communityDID, since)
	if err != nil {
		return fmt.Errorf("failed to check rate limit: %w", err)
	}

	if recentPostCount >= RateLimitMaxPosts {
		return ErrRateLimitExceeded
	}

	return nil
}

// IsAggregator checks if a DID is a registered aggregator
// Fast check used by post creation handler
func (s *aggregatorService) IsAggregator(ctx context.Context, did string) (bool, error) {
	if did == "" {
		return false, nil
	}
	return s.repo.IsAggregator(ctx, did)
}

// RecordAggregatorPost tracks a post created by an aggregator
// Called AFTER successful post creation to update statistics and rate limiting
func (s *aggregatorService) RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error {
	if aggregatorDID == "" || communityDID == "" || postURI == "" || postCID == "" {
		return NewValidationError("post_tracking", "aggregatorDID, communityDID, postURI, and postCID are required")
	}

	return s.repo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, postURI, postCID)
}

// ===== Validation Helpers =====

func (s *aggregatorService) validateEnableRequest(ctx context.Context, req EnableAggregatorRequest) error {
	if req.AggregatorDID == "" {
		return NewValidationError("aggregatorDid", "aggregator DID is required")
	}
	if req.CommunityDID == "" {
		return NewValidationError("communityDid", "community DID is required")
	}
	if req.EnabledByDID == "" {
		return NewValidationError("enabledByDid", "enabledByDID is required")
	}

	// Verify user is a moderator of the community
	// TODO: Implement moderator check
	// membership, err := s.communityService.GetMembership(ctx, req.EnabledByDID, req.CommunityDID)
	// if err != nil || !membership.IsModerator {
	//     return ErrNotModerator
	// }

	return nil
}

func (s *aggregatorService) validateDisableRequest(ctx context.Context, req DisableAggregatorRequest) error {
	if req.AggregatorDID == "" {
		return NewValidationError("aggregatorDid", "aggregator DID is required")
	}
	if req.CommunityDID == "" {
		return NewValidationError("communityDid", "community DID is required")
	}
	if req.DisabledByDID == "" {
		return NewValidationError("disabledByDid", "disabledByDID is required")
	}

	// Verify user is a moderator of the community
	// TODO: Implement moderator check

	return nil
}

func (s *aggregatorService) validateUpdateConfigRequest(ctx context.Context, req UpdateConfigRequest) error {
	if req.AggregatorDID == "" {
		return NewValidationError("aggregatorDid", "aggregator DID is required")
	}
	if req.CommunityDID == "" {
		return NewValidationError("communityDid", "community DID is required")
	}
	if req.UpdatedByDID == "" {
		return NewValidationError("updatedByDid", "updatedByDID is required")
	}
	if len(req.Config) == 0 {
		return NewValidationError("config", "config is required")
	}

	// Verify user is a moderator of the community
	// TODO: Implement moderator check

	return nil
}

// validateConfig validates a config object against a JSON Schema
// Following Bluesky's pattern for feed generator configuration
func (s *aggregatorService) validateConfig(config map[string]interface{}, schemaBytes []byte) error {
	// Parse schema
	schemaLoader := gojsonschema.NewBytesLoader(schemaBytes)

	// Convert config to JSON bytes
	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	configLoader := gojsonschema.NewBytesLoader(configBytes)

	// Validate
	result, err := gojsonschema.Validate(schemaLoader, configLoader)
	if err != nil {
		return fmt.Errorf("failed to validate config: %w", err)
	}

	if !result.Valid() {
		// Collect validation errors
		var errorMessages []string
		for _, desc := range result.Errors() {
			errorMessages = append(errorMessages, desc.String())
		}
		return fmt.Errorf("%w: %s", ErrConfigSchemaValidation, errorMessages)
	}

	return nil
}
