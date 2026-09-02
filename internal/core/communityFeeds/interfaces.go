package communityFeeds

import "context"

// Service defines the business logic interface for feeds
type Service interface {
	// GetCommunityFeed returns posts from a specific community with sorting
	// Supports hot/top/new algorithms, pagination, and viewer state
	GetCommunityFeed(ctx context.Context, req GetCommunityFeedRequest) (*FeedResponse, error)

	// SearchPosts validates a social.coves.feed.searchPosts request, resolves the
	// optional community identifier, and returns matching visible posts.
	SearchPosts(ctx context.Context, req SearchPostsRequest) (*FeedResponse, error)

	// Future methods (Beta):
	// GetTimeline(ctx context.Context, req GetTimelineRequest) (*FeedResponse, error)
	// GetAuthorFeed(ctx context.Context, authorDID string, limit int, cursor *string) (*FeedResponse, error)
}

// Repository defines the data access interface for feeds
type Repository interface {
	// GetCommunityFeed retrieves posts from a community with sorting and pagination
	// Returns hydrated PostView objects (single query with JOINs)
	GetCommunityFeed(ctx context.Context, req GetCommunityFeedRequest) ([]*FeedViewPost, *string, error)

	// SearchPosts runs a full-text search over visible posts, across every
	// community or restricted to req.Community, and returns hydrated posts
	// plus a pagination cursor.
	SearchPosts(ctx context.Context, req SearchPostsRequest) ([]*FeedViewPost, *string, error)

	// Future methods (Beta):
	// GetTimeline(ctx context.Context, userDID string, limit int, cursor *string) ([]*FeedViewPost, *string, error)
	// GetAuthorFeed(ctx context.Context, authorDID string, limit int, cursor *string) ([]*FeedViewPost, *string, error)
}
