-- +goose Up
-- Migration: Update comment URIs from social.coves.feed.comment to social.coves.community.comment
-- This updates the namespace for all comment records in the database.
-- Since we're pre-production, we're only updating the comments table (not votes).

-- Update main comment URIs
UPDATE comments
SET uri = REPLACE(uri, '/social.coves.feed.comment/', '/social.coves.community.comment/')
WHERE uri LIKE '%/social.coves.feed.comment/%';

-- Update root references (when root is a comment, not a post)
UPDATE comments
SET root_uri = REPLACE(root_uri, '/social.coves.feed.comment/', '/social.coves.community.comment/')
WHERE root_uri LIKE '%/social.coves.feed.comment/%';

-- Update parent references (when parent is a comment)
UPDATE comments
SET parent_uri = REPLACE(parent_uri, '/social.coves.feed.comment/', '/social.coves.community.comment/')
WHERE parent_uri LIKE '%/social.coves.feed.comment/%';

-- +goose Down
-- Rollback: Revert comment URIs from social.coves.community.comment to social.coves.feed.comment

UPDATE comments
SET uri = REPLACE(uri, '/social.coves.community.comment/', '/social.coves.feed.comment/')
WHERE uri LIKE '%/social.coves.community.comment/%';

UPDATE comments
SET root_uri = REPLACE(root_uri, '/social.coves.community.comment/', '/social.coves.feed.comment/')
WHERE root_uri LIKE '%/social.coves.community.comment/%';

UPDATE comments
SET parent_uri = REPLACE(parent_uri, '/social.coves.community.comment/', '/social.coves.feed.comment/')
WHERE parent_uri LIKE '%/social.coves.community.comment/%';
