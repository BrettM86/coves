package blueskypost

import (
	"context"
	"time"
)

// Service defines the interface for Bluesky post resolution and caching.
// It orchestrates URL parsing, cache lookups, API fetching, and circuit breaking.
type Service interface {
	// ResolvePost fetches and resolves a Bluesky post by AT-URI.
	// It checks the cache first, then fetches from public.api.bsky.app if needed.
	// Returns BlueskyPostResult with Unavailable=true if the post cannot be resolved.
	ResolvePost(ctx context.Context, atURI string) (*BlueskyPostResult, error)

	// ParseBlueskyURL converts a bsky.app URL to an AT-URI.
	// Example: https://bsky.app/profile/user.bsky.social/post/abc123
	//       -> at://did:plc:xxx/app.bsky.feed.post/abc123
	// Returns error if the URL is invalid or handle resolution fails.
	ParseBlueskyURL(ctx context.Context, url string) (string, error)

	// IsBlueskyURL checks if a URL is a valid bsky.app post URL.
	// Returns true for URLs matching https://bsky.app/profile/{handle}/post/{rkey}
	IsBlueskyURL(url string) bool
}

// Repository defines the interface for Bluesky post cache persistence.
// This follows the same pattern as the unfurl cache repository.
type Repository interface {
	// Get retrieves a cached Bluesky post result for the given AT-URI.
	// Returns ErrCacheMiss if not found or expired (not an error condition).
	// Returns error only on database failures.
	Get(ctx context.Context, atURI string) (*BlueskyPostResult, error)

	// Set stores a Bluesky post result in the cache with the specified TTL.
	// If an entry already exists for the AT-URI, it will be updated.
	// The expires_at is calculated as NOW() + ttl.
	Set(ctx context.Context, atURI string, result *BlueskyPostResult, ttl time.Duration) error
}
