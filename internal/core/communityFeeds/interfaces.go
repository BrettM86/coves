package communityFeeds

import "context"

// Service defines the business logic interface for feeds
type Service interface {
	// SearchPosts validates a social.coves.feed.searchPosts request, resolves the
	// optional community identifier, and returns matching visible posts.
	SearchPosts(ctx context.Context, req SearchPostsRequest) (*FeedResponse, error)
}

// Repository defines the data access interface for feeds
type Repository interface {
	// SearchPosts runs a full-text search over visible posts, across every
	// community or restricted to req.Community, and returns hydrated posts
	// plus a pagination cursor.
	SearchPosts(ctx context.Context, req SearchPostsRequest) ([]*FeedViewPost, *string, error)
}
