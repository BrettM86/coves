package communityFeeds

import (
	"context"
	"fmt"
	"strings"

	"Coves/internal/core/communities"
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
