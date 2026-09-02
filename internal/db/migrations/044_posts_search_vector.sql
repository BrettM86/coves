-- +goose Up
-- Store the search vector so the planner always uses the GIN index and ranking
-- never re-tokenises post bodies. English matches the configured search parser;
-- title matches rank above content matches through A/B weights.
-- Bound inputs to stay below tsvector's 1 MiB limit: the firehose can index
-- federated content that never passed the local API's 3,000/100,000-byte caps.
-- left() counts characters, so content within either byte cap remains complete.
-- ADD COLUMN ... GENERATED ... STORED rewrites posts under an ACCESS EXCLUSIVE lock.
ALTER TABLE posts ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', left(coalesce(title, ''), 3000)), 'A') ||
        setweight(to_tsvector('english', left(coalesce(content, ''), 100000)), 'B')
    ) STORED;

CREATE INDEX idx_posts_search_vector ON posts USING gin(search_vector)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_posts_search_vector;
ALTER TABLE posts DROP COLUMN IF EXISTS search_vector;
