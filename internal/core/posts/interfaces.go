package posts

import (
	"context"
)

// Service constructor accepts optional blobs.Service and unfurl.Service for embed enhancement.
// When unfurlService is provided, external embeds will be automatically enriched with metadata.
// When blobService is provided, thumbnails from unfurled URLs will be uploaded as blobs.

// Service defines the business logic interface for posts
// Coordinates between Repository, community service, and PDS
type Service interface {
	// CreatePost creates a new post in a community
	// Flow: Validate -> Fetch community -> Ensure fresh token -> Write to PDS -> Return URI/CID
	// AppView indexing happens asynchronously via Jetstream consumer
	CreatePost(ctx context.Context, req CreatePostRequest) (*CreatePostResponse, error)

	// GetAuthorPosts retrieves posts authored by a specific user for their profile page
	// Supports filtering by post type (with/without replies, media only) and community
	// Returns paginated feed with cursor
	GetAuthorPosts(ctx context.Context, req GetAuthorPostsRequest) (*GetAuthorPostsResponse, error)

	// Future methods (Beta):
	// GetPost(ctx context.Context, uri string, viewerDID *string) (*Post, error)
	// UpdatePost(ctx context.Context, req UpdatePostRequest) (*Post, error)
	// DeletePost(ctx context.Context, uri string, userDID string) error
	// ListCommunityPosts(ctx context.Context, communityDID string, limit, offset int) ([]*Post, error)
}

// Repository defines the data access interface for posts
// Used by Jetstream consumer to index posts from firehose
type Repository interface {
	// Create inserts a new post into the AppView database
	// Called by Jetstream consumer after post is created on PDS
	Create(ctx context.Context, post *Post) error

	// GetByURI retrieves a post by its AT-URI
	// Used for E2E test verification and future GET endpoint
	GetByURI(ctx context.Context, uri string) (*Post, error)

	// GetByAuthor retrieves posts authored by a specific user
	// Supports filtering by post type and community
	// Returns posts, cursor for pagination, and error
	GetByAuthor(ctx context.Context, req GetAuthorPostsRequest) ([]*PostView, *string, error)

	// Future methods (Beta):
	// Update(ctx context.Context, post *Post) error
	// Delete(ctx context.Context, uri string) error
	// List(ctx context.Context, communityDID string, limit, offset int) ([]*Post, int, error)
}
