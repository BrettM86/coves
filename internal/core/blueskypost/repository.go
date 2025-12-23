package blueskypost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCacheMiss is returned when a cache entry is not found or has expired
var ErrCacheMiss = errors.New("cache miss")

type postgresBlueskyPostRepo struct {
	db *sql.DB
}

// NewRepository creates a new PostgreSQL Bluesky post cache repository
func NewRepository(db *sql.DB) Repository {
	if db == nil {
		panic("blueskypost: db cannot be nil")
	}
	return &postgresBlueskyPostRepo{db: db}
}

// Get retrieves a cached Bluesky post result for the given AT-URI.
// Returns ErrCacheMiss if not found or expired.
// Returns error only on database failures.
func (r *postgresBlueskyPostRepo) Get(ctx context.Context, atURI string) (*BlueskyPostResult, error) {
	query := `
		SELECT metadata
		FROM bluesky_post_cache
		WHERE at_uri = $1 AND expires_at > NOW()
	`

	var metadataJSON []byte

	err := r.db.QueryRowContext(ctx, query, atURI).Scan(&metadataJSON)
	if err == sql.ErrNoRows {
		// Not found or expired is a cache miss
		return nil, ErrCacheMiss
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bluesky post cache entry: %w", err)
	}

	// Unmarshal metadata JSONB to BlueskyPostResult
	var result BlueskyPostResult
	if err := json.Unmarshal(metadataJSON, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &result, nil
}

// Set stores a Bluesky post result in the cache with the specified TTL.
// If an entry already exists for the AT-URI, it will be updated.
// The expires_at is calculated as NOW() + ttl.
func (r *postgresBlueskyPostRepo) Set(ctx context.Context, atURI string, result *BlueskyPostResult, ttl time.Duration) error {
	// Validate AT-URI format to prevent cache pollution
	if err := validateATURI(atURI); err != nil {
		return err
	}

	// Marshal BlueskyPostResult to JSON for metadata column
	metadataJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Convert Go duration to PostgreSQL interval string
	// e.g., "1 hour", "24 hours", "7 days"
	intervalStr := formatInterval(ttl)

	query := `
		INSERT INTO bluesky_post_cache (at_uri, metadata, expires_at)
		VALUES ($1, $2, NOW() + $3::interval)
		ON CONFLICT (at_uri) DO UPDATE
		SET metadata = EXCLUDED.metadata,
		    expires_at = EXCLUDED.expires_at,
		    fetched_at = NOW()
	`

	_, err = r.db.ExecContext(ctx, query, atURI, metadataJSON, intervalStr)
	if err != nil {
		return fmt.Errorf("failed to insert/update bluesky post cache entry: %w", err)
	}

	return nil
}

// formatInterval converts a Go duration to a PostgreSQL interval string
// PostgreSQL accepts intervals like "1 hour", "24 hours", "7 days"
func formatInterval(d time.Duration) string {
	seconds := int64(d.Seconds())

	// Convert to appropriate unit for readability
	switch {
	case seconds >= 86400: // >= 1 day
		days := seconds / 86400
		return fmt.Sprintf("%d days", days)
	case seconds >= 3600: // >= 1 hour
		hours := seconds / 3600
		return fmt.Sprintf("%d hours", hours)
	case seconds >= 60: // >= 1 minute
		minutes := seconds / 60
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return fmt.Sprintf("%d seconds", seconds)
	}
}

// validateATURI validates that a string is a properly formatted AT-URI for a Bluesky post.
// AT-URIs for Bluesky posts must:
//   - Start with "at://"
//   - Contain "/app.bsky.feed.post/"
//
// Example valid URI: at://did:plc:abc123/app.bsky.feed.post/xyz789
func validateATURI(atURI string) error {
	if !strings.HasPrefix(atURI, "at://") {
		return fmt.Errorf("invalid AT-URI: must start with 'at://'")
	}

	if !strings.Contains(atURI, "/app.bsky.feed.post/") {
		return fmt.Errorf("invalid AT-URI: must contain '/app.bsky.feed.post/'")
	}

	return nil
}
