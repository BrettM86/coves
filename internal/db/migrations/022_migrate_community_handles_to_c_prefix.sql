-- +goose Up
-- +goose StatementBegin

-- Migration: Change community handles from .community. subdomain to c- prefix
-- This simplifies DNS/Caddy configuration (works with *.coves.social wildcard)
--
-- Examples:
--   gardening.community.coves.social -> c-gardening.coves.social
--   gaming.community.coves.social -> c-gaming.coves.social
--
-- Also updates the system email format:
--   community-gardening@community.coves.social -> c-gardening@coves.social

-- Update community handles in the communities table
UPDATE communities
SET handle = 'c-' || SPLIT_PART(handle, '.community.', 1) || '.' || SPLIT_PART(handle, '.community.', 2)
WHERE handle LIKE '%.community.%';

-- Update email addresses to match new format
-- Old: community-{name}@community.{instance}
-- New: c-{name}@{instance}
UPDATE communities
SET pds_email = 'c-' || SUBSTRING(pds_email FROM 11 FOR POSITION('@' IN pds_email) - 11) || '@' || SUBSTRING(pds_email FROM POSITION('@community.' IN pds_email) + 11)
WHERE pds_email LIKE 'community-%@community.%';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Rollback: Revert handles from c- prefix back to .community. subdomain
-- Parse: c-{name}.{instance} -> {name}.community.{instance}

UPDATE communities
SET handle = SUBSTRING(handle FROM 3 FOR POSITION('.' IN SUBSTRING(handle FROM 3)) - 1)
             || '.community.'
             || SUBSTRING(handle FROM POSITION('.' IN SUBSTRING(handle FROM 3)) + 3)
WHERE handle LIKE 'c-%' AND handle NOT LIKE '%.community.%';

-- Revert email addresses
-- New: c-{name}@{instance}
-- Old: community-{name}@community.{instance}
UPDATE communities
SET pds_email = 'community-' || SUBSTRING(pds_email FROM 3 FOR POSITION('@' IN pds_email) - 3)
                || '@community.'
                || SUBSTRING(pds_email FROM POSITION('@' IN pds_email) + 1)
WHERE pds_email LIKE 'c-%@%' AND pds_email NOT LIKE 'community-%@community.%';

-- +goose StatementEnd
