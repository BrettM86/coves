package unfurl

import (
	"context"
	"time"
)

// Repository defines the interface for unfurl cache persistence
type Repository interface {
	// Get retrieves a cached unfurl result for the given URL.
	// Returns nil, nil if not found or expired (not an error condition).
	// Returns error only on database failures.
	Get(ctx context.Context, url string) (*UnfurlResult, error)

	// Set stores an unfurl result in the cache with the specified TTL.
	// If an entry already exists for the URL, it will be updated.
	// The expires_at is calculated as NOW() + ttl.
	Set(ctx context.Context, url string, result *UnfurlResult, ttl time.Duration) error
}
