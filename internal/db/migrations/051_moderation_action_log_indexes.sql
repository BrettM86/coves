-- +goose Up
-- +goose NO TRANSACTION
-- Support newest-first public and admin action-log pagination and scoped reads.
CREATE INDEX CONCURRENTLY moderation_actions_created_at
    ON moderation_actions (created_at DESC, id DESC);
CREATE INDEX CONCURRENTLY moderation_actions_actor_created_at
    ON moderation_actions (actor_did, created_at DESC, id DESC);
CREATE INDEX CONCURRENTLY moderation_actions_community_created_at
    ON moderation_actions (subject_community_did, created_at DESC, id DESC);

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS moderation_actions_community_created_at;
DROP INDEX CONCURRENTLY IF EXISTS moderation_actions_actor_created_at;
DROP INDEX CONCURRENTLY IF EXISTS moderation_actions_created_at;
