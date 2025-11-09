package unfurl

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type postgresUnfurlRepo struct {
	db *sql.DB
}

// NewRepository creates a new PostgreSQL unfurl cache repository
func NewRepository(db *sql.DB) Repository {
	return &postgresUnfurlRepo{db: db}
}

// Get retrieves a cached unfurl result for the given URL.
// Returns nil, nil if not found or expired (not an error condition).
// Returns error only on database failures.
func (r *postgresUnfurlRepo) Get(ctx context.Context, url string) (*UnfurlResult, error) {
	query := `
		SELECT metadata, thumbnail_url, provider
		FROM unfurl_cache
		WHERE url = $1 AND expires_at > NOW()
	`

	var metadataJSON []byte
	var thumbnailURL sql.NullString
	var provider string

	err := r.db.QueryRowContext(ctx, query, url).Scan(&metadataJSON, &thumbnailURL, &provider)
	if err == sql.ErrNoRows {
		// Not found or expired is not an error
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get unfurl cache entry: %w", err)
	}

	// Unmarshal metadata JSONB to UnfurlResult
	var result UnfurlResult
	if err := json.Unmarshal(metadataJSON, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	// Ensure provider and thumbnailURL are set (may not be in metadata JSON)
	result.Provider = provider
	if thumbnailURL.Valid {
		result.ThumbnailURL = thumbnailURL.String
	}

	return &result, nil
}

// Set stores an unfurl result in the cache with the specified TTL.
// If an entry already exists for the URL, it will be updated.
// The expires_at is calculated as NOW() + ttl.
func (r *postgresUnfurlRepo) Set(ctx context.Context, url string, result *UnfurlResult, ttl time.Duration) error {
	// Marshal UnfurlResult to JSON for metadata column
	metadataJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Store thumbnail_url separately for potential queries
	var thumbnailURL sql.NullString
	if result.ThumbnailURL != "" {
		thumbnailURL.String = result.ThumbnailURL
		thumbnailURL.Valid = true
	}

	// Convert Go duration to PostgreSQL interval string
	// e.g., "1 hour", "24 hours", "7 days"
	intervalStr := formatInterval(ttl)

	query := `
		INSERT INTO unfurl_cache (url, provider, metadata, thumbnail_url, expires_at)
		VALUES ($1, $2, $3, $4, NOW() + $5::interval)
		ON CONFLICT (url) DO UPDATE
		SET provider = EXCLUDED.provider,
		    metadata = EXCLUDED.metadata,
		    thumbnail_url = EXCLUDED.thumbnail_url,
		    expires_at = EXCLUDED.expires_at,
		    fetched_at = NOW()
	`

	_, err = r.db.ExecContext(ctx, query, url, result.Provider, metadataJSON, thumbnailURL, intervalStr)
	if err != nil {
		return fmt.Errorf("failed to insert/update unfurl cache entry: %w", err)
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
