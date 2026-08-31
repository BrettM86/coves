-- +goose Up
-- The redriver prunes expired dead letters by updated_at every pass. Keep that
-- bounded cleanup from becoming a full-table scan as the queue grows under a
-- fabricated-reference flood.
CREATE INDEX idx_jetstream_dead_letters_retention
    ON jetstream_dead_letters (updated_at);

-- +goose Down
DROP INDEX IF EXISTS idx_jetstream_dead_letters_retention;
