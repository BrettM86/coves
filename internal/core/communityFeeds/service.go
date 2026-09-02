package communityFeeds

import (
	"Coves/internal/core/communities"
	"context"
	"fmt"
	"strings"
)

var validTimeframes = map[string]bool{
	"hour": true, "day": true, "week": true,
	"month": true, "year": true, "all": true,
}

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
	if err := validateLimit(&req.Limit); err != nil {
		return err
	}

	// Validate and set defaults for timeframe (only used with top sort)
	if req.Sort == "top" && req.Timeframe == "" {
		req.Timeframe = "day"
	}
	return validateTimeframe(req.Timeframe)
}

func validateLimit(limit *int) error {
	if *limit <= 0 {
		*limit = 15
	}
	if *limit > 50 {
		return NewValidationError("limit", "limit must not exceed 50")
	}
	return nil
}

func validateTimeframe(timeframe string) error {
	if timeframe != "" && !validTimeframes[timeframe] {
		return NewValidationError("timeframe", "timeframe must be one of: hour, day, week, month, year, all")
	}
	return nil
}

// SearchPosts implements social.coves.feed.searchPosts. Its timeframe defaults
// to all because search should not silently inherit getCommunity's one-day bound
// for top results; callers expect matching posts across the indexed corpus.
func (s *feedService) SearchPosts(ctx context.Context, req SearchPostsRequest) (*FeedResponse, error) {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return nil, NewValidationError("q", "q parameter is required")
	}
	if len(req.Query) > 500 {
		return nil, NewValidationError("q", "q must not exceed 500 bytes")
	}

	if req.Sort == "" {
		req.Sort = "relevance"
	}
	switch req.Sort {
	case "relevance", "new", "top":
	default:
		return nil, NewValidationError("sort", "sort must be one of: relevance, new, top")
	}

	if req.Timeframe == "" {
		req.Timeframe = "all"
	}
	if err := validateTimeframe(req.Timeframe); err != nil {
		return nil, err
	}
	if err := validateLimit(&req.Limit); err != nil {
		return nil, err
	}

	if req.Community != "" {
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
		req.Community = communityDID
	}

	feedPosts, cursor, err := s.repo.SearchPosts(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to search posts: %w", err)
	}
	if feedPosts == nil {
		feedPosts = []*FeedViewPost{}
	}

	return &FeedResponse{
		Feed:   feedPosts,
		Cursor: cursor,
	}, nil
}
