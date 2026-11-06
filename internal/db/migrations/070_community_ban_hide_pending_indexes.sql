-- +goose Up
-- +goose NO TRANSACTION
-- Partial indexes for classifying pending comments and votes when a thread root
-- arrives. Built CONCURRENTLY outside 069's transaction so the comments and
-- votes tables, both heavily written, are not locked for two full scans during
-- deploy. No IF NOT EXISTS: an interrupted concurrent build can leave an
-- INVALID index that an operator must drop with DROP INDEX CONCURRENTLY before
-- retrying, not silently retained.
CREATE INDEX CONCURRENTLY comments_pending_ban_check ON comments (uri) WHERE hide_reason = 'pending_ban_check';
CREATE INDEX CONCURRENTLY votes_pending_ban_check ON votes (subject_uri) WHERE hide_reason = 'pending_ban_check';

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS votes_pending_ban_check;
DROP INDEX CONCURRENTLY IF EXISTS comments_pending_ban_check;
