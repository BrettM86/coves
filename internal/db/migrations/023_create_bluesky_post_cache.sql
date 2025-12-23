-- +goose Up
CREATE TABLE bluesky_post_cache (
    at_uri TEXT PRIMARY KEY,
    metadata JSONB NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_bluesky_post_cache_expires ON bluesky_post_cache(expires_at);

COMMENT ON TABLE bluesky_post_cache IS 'Cache for Bluesky post data fetched from public.api.bsky.app';
COMMENT ON COLUMN bluesky_post_cache.at_uri IS 'AT-URI of the Bluesky post (e.g., at://did:plc:xxx/app.bsky.feed.post/abc123)';
COMMENT ON COLUMN bluesky_post_cache.metadata IS 'Full BlueskyPostResult as JSON (text, author, stats, etc.)';
COMMENT ON COLUMN bluesky_post_cache.expires_at IS 'When this cache entry should be refetched (shorter TTL than unfurl since posts can be edited/deleted)';

-- +goose Down
DROP INDEX IF EXISTS idx_bluesky_post_cache_expires;
DROP TABLE IF EXISTS bluesky_post_cache;
