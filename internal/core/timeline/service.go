package timeline

import (
	"context"
	"fmt"
)

type timelineService struct {
	repo Repository
}

// NewTimelineService creates a new timeline service
func NewTimelineService(repo Repository) Service {
	return &timelineService{
		repo: repo,
	}
}

// GetTimeline retrieves posts from all communities the user subscribes to
func (s *timelineService) GetTimeline(ctx context.Context, req GetTimelineRequest) (*TimelineResponse, error) {
	// 1. Validate request
	if err := s.validateRequest(&req); err != nil {
		return nil, err
	}

	// 2. UserDID must be set (from auth middleware)
	if req.UserDID == "" {
		return nil, ErrUnauthorized
	}

	// 3. Fetch timeline from repository (hydrated posts from subscribed communities)
	feedPosts, cursor, err := s.repo.GetTimeline(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get timeline: %w", err)
	}

	// 4. Return timeline response
	return &TimelineResponse{
		Feed:   feedPosts,
		Cursor: cursor,
	}, nil
}

// validateRequest validates the timeline request parameters
func (s *timelineService) validateRequest(req *GetTimelineRequest) error {
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
