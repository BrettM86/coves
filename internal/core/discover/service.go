package discover

import (
	"context"
	"fmt"
)

type discoverService struct {
	repo Repository
}

// NewDiscoverService creates a new discover service
func NewDiscoverService(repo Repository) Service {
	return &discoverService{
		repo: repo,
	}
}

// GetDiscover retrieves posts from all communities (public feed)
func (s *discoverService) GetDiscover(ctx context.Context, req GetDiscoverRequest) (*DiscoverResponse, error) {
	// Validate request
	if err := s.validateRequest(&req); err != nil {
		return nil, err
	}

	// Fetch discover feed from repository (all posts from all communities)
	feedPosts, cursor, err := s.repo.GetDiscover(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get discover feed: %w", err)
	}

	// Return discover response
	return &DiscoverResponse{
		Feed:   feedPosts,
		Cursor: cursor,
	}, nil
}

// validateRequest validates the discover request parameters
func (s *discoverService) validateRequest(req *GetDiscoverRequest) error {
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
