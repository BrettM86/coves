-- +goose Up
-- +goose StatementBegin

-- Migration: Rename community handles from .communities. to .community.
-- This aligns with AT Protocol lexicon naming conventions (singular form)
--
-- Background:
-- All atProto record types use singular form (app.bsky.feed.post, app.bsky.graph.follow)
-- This migration updates existing community handles to follow the same pattern
--
-- Examples:
--   gardening.communities.coves.social -> gardening.community.coves.social
--   gaming.communities.coves.social -> gaming.community.coves.social
--
-- Safety: Uses REPLACE which only affects handles containing '.communities.'
-- Rollback: Available via down migration below

-- Update community handles in the communities table
UPDATE communities
SET handle = REPLACE(handle, '.communities.', '.community.')
WHERE handle LIKE '%.communities.%';

-- Verify the migration (optional - can be commented out in production)
-- This will fail if any .communities. handles remain
DO $$
DECLARE
    old_format_count INTEGER;
BEGIN
    SELECT COUNT(*) INTO old_format_count
    FROM communities
    WHERE handle LIKE '%.communities.%';

    IF old_format_count > 0 THEN
        RAISE EXCEPTION 'Migration incomplete: % communities still have .communities. format', old_format_count;
    END IF;

    RAISE NOTICE 'Migration successful: All community handles updated to .community. format';
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Rollback: Revert handles from .community. back to .communities.
-- Only use this if you need to rollback the naming convention change

UPDATE communities
SET handle = REPLACE(handle, '.community.', '.communities.')
WHERE handle LIKE '%.community.%';

-- Verify the rollback
DO $$
DECLARE
    new_format_count INTEGER;
BEGIN
    SELECT COUNT(*) INTO new_format_count
    FROM communities
    WHERE handle LIKE '%.community.%';

    IF new_format_count > 0 THEN
        RAISE EXCEPTION 'Rollback incomplete: % communities still have .community. format', new_format_count;
    END IF;

    RAISE NOTICE 'Rollback successful: All community handles reverted to .communities. format';
END $$;

-- +goose StatementEnd
