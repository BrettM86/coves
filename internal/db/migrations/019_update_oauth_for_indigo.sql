-- +goose Up
-- Update OAuth tables to match indigo's ClientAuthStore interface requirements
-- This migration adds columns needed for OAuth client sessions and auth requests

-- Update oauth_requests table
-- Add columns for request URI, auth server endpoints, scopes, and DPoP key
ALTER TABLE oauth_requests
    ADD COLUMN request_uri TEXT,
    ADD COLUMN auth_server_token_endpoint TEXT,
    ADD COLUMN auth_server_revocation_endpoint TEXT,
    ADD COLUMN scopes TEXT[],
    ADD COLUMN dpop_private_key_multibase TEXT;

-- Make original dpop_private_jwk nullable (we now use dpop_private_key_multibase)
ALTER TABLE oauth_requests ALTER COLUMN dpop_private_jwk DROP NOT NULL;

-- Make did nullable (indigo's AuthRequestData.AccountDID is a pointer - optional)
ALTER TABLE oauth_requests ALTER COLUMN did DROP NOT NULL;

-- Make handle and pds_url nullable too (derived from DID resolution, not always available at auth request time)
ALTER TABLE oauth_requests ALTER COLUMN handle DROP NOT NULL;
ALTER TABLE oauth_requests ALTER COLUMN pds_url DROP NOT NULL;

-- Update existing oauth_requests data
-- Convert dpop_private_jwk (JSONB) to multibase format if needed
-- Note: This will leave the multibase column NULL for now since conversion requires crypto logic
-- The application will need to handle NULL values or regenerate keys on next auth flow
UPDATE oauth_requests
SET
    auth_server_token_endpoint = auth_server_iss || '/oauth/token',
    scopes = ARRAY['atproto']::TEXT[]
WHERE auth_server_token_endpoint IS NULL;

-- Add indexes for new columns
CREATE INDEX idx_oauth_requests_request_uri ON oauth_requests(request_uri) WHERE request_uri IS NOT NULL;

-- Update oauth_sessions table
-- Add session_id column (will become part of composite key)
ALTER TABLE oauth_sessions
    ADD COLUMN session_id TEXT,
    ADD COLUMN host_url TEXT,
    ADD COLUMN auth_server_token_endpoint TEXT,
    ADD COLUMN auth_server_revocation_endpoint TEXT,
    ADD COLUMN scopes TEXT[],
    ADD COLUMN dpop_private_key_multibase TEXT;

-- Make original dpop_private_jwk nullable (we now use dpop_private_key_multibase)
ALTER TABLE oauth_sessions ALTER COLUMN dpop_private_jwk DROP NOT NULL;

-- Populate session_id for existing sessions (use DID as default for single-session per account)
-- In production, you may want to generate unique session IDs
UPDATE oauth_sessions
SET
    session_id = 'default',
    host_url = pds_url,
    auth_server_token_endpoint = auth_server_iss || '/oauth/token',
    scopes = ARRAY['atproto']::TEXT[]
WHERE session_id IS NULL;

-- Make session_id NOT NULL after populating existing data
ALTER TABLE oauth_sessions
    ALTER COLUMN session_id SET NOT NULL;

-- Drop old unique constraint on did only
ALTER TABLE oauth_sessions
    DROP CONSTRAINT IF EXISTS oauth_sessions_did_key;

-- Create new composite unique constraint for (did, session_id)
-- This allows multiple sessions per account
-- Note: UNIQUE constraint automatically creates an index, so no separate index needed
ALTER TABLE oauth_sessions
    ADD CONSTRAINT oauth_sessions_did_session_id_key UNIQUE (did, session_id);

-- Add comment explaining the schema change
COMMENT ON COLUMN oauth_sessions.session_id IS 'Session identifier to support multiple concurrent sessions per account';
COMMENT ON CONSTRAINT oauth_sessions_did_session_id_key ON oauth_sessions IS 'Composite key allowing multiple sessions per DID';

-- +goose Down
-- Rollback: Remove added columns and restore original unique constraint

-- oauth_sessions rollback
-- Drop composite unique constraint (this also drops the associated index)
ALTER TABLE oauth_sessions
    DROP CONSTRAINT IF EXISTS oauth_sessions_did_session_id_key;

-- Delete all but the most recent session per DID before restoring unique constraint
-- This ensures the UNIQUE (did) constraint can be added without conflicts
DELETE FROM oauth_sessions a
USING oauth_sessions b
WHERE a.did = b.did
  AND a.created_at < b.created_at;

-- Restore old unique constraint
ALTER TABLE oauth_sessions
    ADD CONSTRAINT oauth_sessions_did_key UNIQUE (did);

-- Restore NOT NULL constraint on dpop_private_jwk
ALTER TABLE oauth_sessions
    ALTER COLUMN dpop_private_jwk SET NOT NULL;

ALTER TABLE oauth_sessions
    DROP COLUMN IF EXISTS dpop_private_key_multibase,
    DROP COLUMN IF EXISTS scopes,
    DROP COLUMN IF EXISTS auth_server_revocation_endpoint,
    DROP COLUMN IF EXISTS auth_server_token_endpoint,
    DROP COLUMN IF EXISTS host_url,
    DROP COLUMN IF EXISTS session_id;

-- oauth_requests rollback
DROP INDEX IF EXISTS idx_oauth_requests_request_uri;

-- Restore NOT NULL constraints
ALTER TABLE oauth_requests
    ALTER COLUMN dpop_private_jwk SET NOT NULL,
    ALTER COLUMN did SET NOT NULL,
    ALTER COLUMN handle SET NOT NULL,
    ALTER COLUMN pds_url SET NOT NULL;

ALTER TABLE oauth_requests
    DROP COLUMN IF EXISTS dpop_private_key_multibase,
    DROP COLUMN IF EXISTS scopes,
    DROP COLUMN IF EXISTS auth_server_revocation_endpoint,
    DROP COLUMN IF EXISTS auth_server_token_endpoint,
    DROP COLUMN IF EXISTS request_uri;
