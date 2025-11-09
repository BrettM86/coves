-- +goose Up
CREATE TABLE unfurl_cache (
    url TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    metadata JSONB NOT NULL,
    thumbnail_url TEXT,
    fetched_at TIMESTAMP NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_unfurl_cache_expires ON unfurl_cache(expires_at);

COMMENT ON TABLE unfurl_cache IS 'Cache for oEmbed/URL unfurl results to reduce external API calls';
COMMENT ON COLUMN unfurl_cache.url IS 'The URL that was unfurled (primary key)';
COMMENT ON COLUMN unfurl_cache.provider IS 'Provider name (streamable, youtube, reddit, etc.)';
COMMENT ON COLUMN unfurl_cache.metadata IS 'Full unfurl result as JSON (title, description, type, etc.)';
COMMENT ON COLUMN unfurl_cache.thumbnail_url IS 'URL of the thumbnail image';
COMMENT ON COLUMN unfurl_cache.expires_at IS 'When this cache entry should be refetched (TTL-based)';

-- +goose Down
DROP INDEX IF EXISTS idx_unfurl_cache_expires;
DROP TABLE IF EXISTS unfurl_cache;
