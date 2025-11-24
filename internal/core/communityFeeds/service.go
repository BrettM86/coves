package communityFeeds

import (
	"context"
	"fmt"

	"Coves/internal/core/communities"
)

type feedService struct {
	repo             Repository
	communityService communities.Service
}

// NewCommunityFeedService creates a new feed service
func NewCommunityFeedService(
	repo Repository,
	communityService communities.Service,
) Service {
	return &feedService{
		repo:             repo,
		communityService: communityService,
	}
}

// GetCommunityFeed retrieves posts from a community with sorting
func (s *feedService) GetCommunityFeed(ctx context.Context, req GetCommunityFeedRequest) (*FeedResponse, error) {
	// 1. Validate request
	if err := s.validateRequest(&req); err != nil {
		return nil, err
	}

	// 2. Resolve community identifier (handle or DID) to DID
	communityDID, err := s.communityService.ResolveCommunityIdentifier(ctx, req.Community)
	if err != nil {
		if communities.IsNotFound(err) {
			return nil, ErrCommunityNotFound
		}
		if communities.IsValidationError(err) {
			return nil, NewValidationError("community", err.Error())
		}
		return nil, fmt.Errorf("failed to resolve community identifier: %w", err)
	}

	// 3. Update request with resolved DID
	req.Community = communityDID

	// 4. Fetch feed from repository (hydrated posts)
	feedPosts, cursor, err := s.repo.GetCommunityFeed(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get community feed: %w", err)
	}

	// 5. Return feed response
	return &FeedResponse{
		Feed:   feedPosts,
		Cursor: cursor,
	}, nil
}

// validateRequest validates the feed request parameters
func (s *feedService) validateRequest(req *GetCommunityFeedRequest) error {
	// Validate community identifier
	if req.Community == "" {
		return NewValidationError("community", "community parameter is required")
	}

	// Validate and set defaults for sort
	if req.Sort == "" {
		req.Sort = "hot"
	}
	validSorts := map[string]bool{"hot": true, "top": true, "new": true}
	if !validSorts[req.Sort] {
		return NewValidationError("sort", "sort must be one of: hot, top, new")
	}

	// Validate and set defaults for limit
	if req.Limit <= 0 {
		req.Limit = 15
	}
	if req.Limit > 50 {
		return NewValidationError("limit", "limit must not exceed 50")
	}

	// Validate and set defaults for timeframe (only used with top sort)
	if req.Sort == "top" && req.Timeframe == "" {
		req.Timeframe = "day"
	}
	validTimeframes := map[string]bool{
		"hour": true, "day": true, "week": true,
		"month": true, "year": true, "all": true,
	}
	if req.Timeframe != "" && !validTimeframes[req.Timeframe] {
		return NewValidationError("timeframe", "timeframe must be one of: hour, day, week, month, year, all")
	}

	return nil
}
