package posts

import (
	"context"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
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

	// GetPosts batch-fetches post views by AT-URI for feed hydration and permalink
	// (cold-load) rendering. Implements social.coves.community.post.get.
	// URIs must be canonical DID-based AT-URIs; malformed or handle-based URIs are
	// rejected with a validation error (handles are mutable and would break on rename).
	// Results are returned in the same order as req.URIs; valid URIs whose post is
	// missing or deleted come back as NotFoundPost markers. When req.ViewerDID is set
	// and a BlockChecker is wired, posts authored by users the viewer has blocked are
	// returned as BlockedPost markers instead of full views (matching feed/timeline).
	GetPosts(ctx context.Context, req GetPostsRequest) ([]*PostResult, error)

	// DeletePost deletes a post from the community's PDS repository
	// SECURITY: Only the post author can delete their own posts
	// Flow: Validate URI -> Fetch community -> Verify author -> Delete from PDS
	DeletePost(ctx context.Context, session *oauth.ClientSessionData, req DeletePostRequest) error

	// Future methods (Beta):
	// UpdatePost(ctx context.Context, req UpdatePostRequest) (*Post, error)
	// ListCommunityPosts(ctx context.Context, communityDID string, limit, offset int) ([]*Post, error)
}

// Repository defines the data access interface for posts
// Used by Jetstream consumer to index posts from firehose
type Repository interface {
	// Create inserts a new post into the AppView database
	// Called by Jetstream consumer after post is created on PDS
	Create(ctx context.Context, post *Post) error

	// GetByURI retrieves a post by its AT-URI
	// Used for E2E test verification and single-record lookups (returns the raw
	// record without author/community joins)
	GetByURI(ctx context.Context, uri string) (*Post, error)

	// GetViewsByURIs retrieves full post views (with author + community joins) for a
	// set of canonical DID-based AT-URIs. Returns a map keyed by URI; missing or
	// soft-deleted posts are simply absent from the map. Backs social.coves.community.post.get.
	GetViewsByURIs(ctx context.Context, uris []string) (map[string]*PostView, error)

	// GetByAuthor retrieves posts authored by a specific user
	// Supports filtering by post type and community
	// Returns posts, cursor for pagination, and error
	GetByAuthor(ctx context.Context, req GetAuthorPostsRequest) ([]*PostView, *string, error)

	// SoftDelete marks a post as deleted in the AppView database
	// Called by Jetstream consumer after post is deleted from PDS
	// Idempotent: Returns success if post already deleted
	SoftDelete(ctx context.Context, uri string) error

	// Future methods (Beta):
	// Update(ctx context.Context, post *Post) error
	// List(ctx context.Context, communityDID string, limit, offset int) ([]*Post, int, error)
}

// BlockChecker reports which of the given author DIDs the viewer (blockerDID) has
// blocked. It is the narrow slice of the user-block store that GetPosts needs to enforce
// viewer block visibility, and is satisfied by the userblocks repository's AreBlocked.
// Optional: when nil, block enforcement is skipped (e.g. minimal setups and unit tests).
type BlockChecker interface {
	// AreBlocked returns a map of blockedDID -> true for each DID in blockedDIDs that
	// blockerDID has blocked. DIDs that are not blocked are absent from the map.
	AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error)
}
