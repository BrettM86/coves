-- +goose Up
-- Add deletion reason tracking to preserve thread structure while respecting privacy
-- When comments are deleted, we blank content but keep the record for threading

-- Create enum type for deletion reasons
CREATE TYPE deletion_reason AS ENUM ('author', 'moderator');

-- Add new columns to comments table
ALTER TABLE comments ADD COLUMN deletion_reason deletion_reason;
ALTER TABLE comments ADD COLUMN deleted_by TEXT;

-- Add comments for new columns
COMMENT ON COLUMN comments.deletion_reason IS 'Reason for deletion: author (user deleted), moderator (community mod removed)';
COMMENT ON COLUMN comments.deleted_by IS 'DID of the actor who performed the deletion';

-- Backfill existing deleted comments as author-deleted
-- This handles existing soft-deleted comments gracefully
UPDATE comments
SET deletion_reason = 'author',
    deleted_by = commenter_did
WHERE deleted_at IS NOT NULL AND deletion_reason IS NULL;

-- Modify existing indexes to NOT filter deleted_at IS NULL
-- This allows deleted comments to appear in thread queries for structure preservation
-- Note: We drop and recreate to change the partial index condition

-- Drop old partial indexes that exclude deleted comments
DROP INDEX IF EXISTS idx_comments_root;
DROP INDEX IF EXISTS idx_comments_parent;
DROP INDEX IF EXISTS idx_comments_parent_score;
DROP INDEX IF EXISTS idx_comments_uri_active;

-- Recreate indexes without the deleted_at filter (include all comments for threading)
CREATE INDEX idx_comments_root ON comments(root_uri, created_at DESC);
CREATE INDEX idx_comments_parent ON comments(parent_uri, created_at DESC);
CREATE INDEX idx_comments_parent_score ON comments(parent_uri, score DESC, created_at DESC);
CREATE INDEX idx_comments_uri_lookup ON comments(uri);

-- Add index for querying by deletion_reason (for moderation dashboard)
CREATE INDEX idx_comments_deleted_reason ON comments(deletion_reason, deleted_at DESC)
WHERE deleted_at IS NOT NULL;

-- Add index for querying by deleted_by (for moderation audit/filtering)
CREATE INDEX idx_comments_deleted_by ON comments(deleted_by, deleted_at DESC)
WHERE deleted_at IS NOT NULL;

-- +goose Down
-- Remove deletion metadata columns and restore original indexes

DROP INDEX IF EXISTS idx_comments_deleted_by;
DROP INDEX IF EXISTS idx_comments_deleted_reason;
DROP INDEX IF EXISTS idx_comments_uri_lookup;
DROP INDEX IF EXISTS idx_comments_parent_score;
DROP INDEX IF EXISTS idx_comments_parent;
DROP INDEX IF EXISTS idx_comments_root;

-- Restore original partial indexes (excluding deleted comments)
CREATE INDEX idx_comments_root ON comments(root_uri, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent ON comments(parent_uri, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent_score ON comments(parent_uri, score DESC, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_uri_active ON comments(uri) WHERE deleted_at IS NULL;

ALTER TABLE comments DROP COLUMN IF EXISTS deleted_by;
ALTER TABLE comments DROP COLUMN IF EXISTS deletion_reason;

DROP TYPE IF EXISTS deletion_reason;
