-- +goose Up
-- +goose StatementBegin
-- V2.0: Add encrypted password column for PDS account recovery
-- CRITICAL FIX: Password must be encrypted (not hashed) for session recovery
-- When access/refresh tokens expire (90-day window), we need the plaintext password
-- to call com.atproto.server.createSession - bcrypt hashing prevents this

-- Add encrypted password column
ALTER TABLE communities ADD COLUMN pds_password_encrypted BYTEA;

-- Drop legacy plaintext token columns (we now use *_encrypted versions from migration 006)
ALTER TABLE communities DROP COLUMN IF EXISTS pds_access_token;
ALTER TABLE communities DROP COLUMN IF EXISTS pds_refresh_token;

-- Drop legacy password_hash column from migration 005 (never used in production)
ALTER TABLE communities DROP COLUMN IF EXISTS pds_password_hash;

-- Add comment
COMMENT ON COLUMN communities.pds_password_encrypted IS 'Encrypted community PDS password (pgp_sym_encrypt) - required for session recovery when tokens expire';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restore legacy columns (for rollback compatibility)
ALTER TABLE communities ADD COLUMN pds_access_token TEXT;
ALTER TABLE communities ADD COLUMN pds_refresh_token TEXT;
ALTER TABLE communities ADD COLUMN pds_password_hash TEXT;

-- Drop encrypted password
ALTER TABLE communities DROP COLUMN IF EXISTS pds_password_encrypted;

-- Restore old comment
COMMENT ON COLUMN communities.pds_password_hash IS 'bcrypt hash of community PDS password (DEPRECATED - cannot recover plaintext)';
-- +goose StatementEnd
