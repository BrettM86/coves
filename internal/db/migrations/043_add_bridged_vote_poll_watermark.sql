-- +goose Up
-- Add nullable poll watermarks for rotating the bridge aggregate side-channel
-- across eligible posts and comments. NULL means never polled and intentionally
-- sorts first, so newly indexed and pre-migration content receives a first pass
-- before already-visited rows cycle back through the bounded sweep.

ALTER TABLE posts
    ADD COLUMN bridged_polled_at TIMESTAMPTZ;

ALTER TABLE comments
    ADD COLUMN bridged_polled_at TIMESTAMPTZ;

-- The bridged-vote poller repeatedly takes the oldest active rows from each table.
-- The created_at and uri suffix makes selection deterministic while a never-polled
-- cohort is larger than one sweep, rather than leaving its first pass to the plan.
CREATE INDEX idx_posts_bridged_poll
    ON posts (bridged_polled_at ASC NULLS FIRST, created_at ASC, uri ASC)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_comments_bridged_poll
    ON comments (bridged_polled_at ASC NULLS FIRST, created_at ASC, uri ASC)
    WHERE deleted_at IS NULL;

COMMENT ON COLUMN posts.bridged_polled_at IS 'When the bridge aggregate side-channel last attempted this post; NULL means never polled';
COMMENT ON COLUMN comments.bridged_polled_at IS 'When the bridge aggregate side-channel last attempted this comment; NULL means never polled';

-- Migration 031's catalog text said these guards accepted only strictly newer
-- samples. Both Jetstream ingestion and the poller deliberately use >= so an
-- equal-asOf replay is idempotent. The column comments are documentation only,
-- so they are corrected here rather than in a migration of their own; Down
-- restores 031's wording so a rollback lands exactly where 031 left the catalog.
COMMENT ON COLUMN posts.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply when the incoming asOf is newer-or-equal (>=)';
COMMENT ON COLUMN comments.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply when the incoming asOf is newer-or-equal (>=)';

-- +goose Down
COMMENT ON COLUMN posts.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply only when strictly newer';
COMMENT ON COLUMN comments.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply only when strictly newer';

DROP INDEX IF EXISTS idx_comments_bridged_poll;
DROP INDEX IF EXISTS idx_posts_bridged_poll;

ALTER TABLE comments
    DROP COLUMN IF EXISTS bridged_polled_at;

ALTER TABLE posts
    DROP COLUMN IF EXISTS bridged_polled_at;
