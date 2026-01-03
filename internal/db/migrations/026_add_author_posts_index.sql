-- +goose Up
-- +goose NO TRANSACTION
-- Add optimized index for author posts queries with soft delete filter
-- This supports the social.coves.actor.getPosts endpoint which retrieves posts by author
-- The existing idx_posts_author doesn't filter deleted posts, causing full index scans
CREATE INDEX CONCURRENTLY idx_posts_author_created
ON posts(author_did, created_at DESC)
WHERE deleted_at IS NULL;

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS idx_posts_author_created;
