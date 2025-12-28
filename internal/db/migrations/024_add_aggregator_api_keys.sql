-- +goose Up
-- Add API key authentication and OAuth credential storage for aggregators
-- This enables aggregators to authenticate using API keys backed by OAuth sessions

-- ============================================================================
-- Add API key columns to aggregators table
-- ============================================================================
ALTER TABLE aggregators
    -- API key identification (prefix for log correlation, hash for auth)
    ADD COLUMN api_key_prefix VARCHAR(12),
    ADD COLUMN api_key_hash VARCHAR(64) UNIQUE,

    -- OAuth credentials (encrypted at application layer before storage)
    -- SECURITY: These columns contain sensitive OAuth tokens
    ADD COLUMN oauth_access_token TEXT,
    ADD COLUMN oauth_refresh_token TEXT,
    ADD COLUMN oauth_token_expires_at TIMESTAMPTZ,

    -- OAuth session metadata for token refresh
    ADD COLUMN oauth_pds_url TEXT,
    ADD COLUMN oauth_auth_server_iss TEXT,
    ADD COLUMN oauth_auth_server_token_endpoint TEXT,

    -- DPoP keys and nonces for token refresh (multibase encoded)
    -- SECURITY: Contains private key material
    ADD COLUMN oauth_dpop_private_key_multibase TEXT,
    ADD COLUMN oauth_dpop_authserver_nonce TEXT,
    ADD COLUMN oauth_dpop_pds_nonce TEXT,

    -- API key lifecycle timestamps
    ADD COLUMN api_key_created_at TIMESTAMPTZ,
    ADD COLUMN api_key_revoked_at TIMESTAMPTZ,
    ADD COLUMN api_key_last_used_at TIMESTAMPTZ;

-- Index for API key lookup during authentication
-- Partial index excludes NULL values since not all aggregators have API keys
CREATE INDEX idx_aggregators_api_key_hash
    ON aggregators(api_key_hash)
    WHERE api_key_hash IS NOT NULL;

-- ============================================================================
-- Security comments on sensitive columns
-- ============================================================================
COMMENT ON COLUMN aggregators.api_key_prefix IS 'First 12 characters of API key for identification in logs (not secret)';
COMMENT ON COLUMN aggregators.api_key_hash IS 'SHA-256 hash of full API key for authentication lookup';
COMMENT ON COLUMN aggregators.oauth_access_token IS 'SENSITIVE: Encrypted OAuth access token for PDS operations';
COMMENT ON COLUMN aggregators.oauth_refresh_token IS 'SENSITIVE: Encrypted OAuth refresh token for session renewal';
COMMENT ON COLUMN aggregators.oauth_token_expires_at IS 'When the OAuth access token expires (triggers refresh)';
COMMENT ON COLUMN aggregators.oauth_pds_url IS 'PDS URL for this aggregators OAuth session';
COMMENT ON COLUMN aggregators.oauth_auth_server_iss IS 'OAuth authorization server issuer URL';
COMMENT ON COLUMN aggregators.oauth_auth_server_token_endpoint IS 'OAuth token refresh endpoint URL';
COMMENT ON COLUMN aggregators.oauth_dpop_private_key_multibase IS 'SENSITIVE: DPoP private key in multibase format for token refresh';
COMMENT ON COLUMN aggregators.oauth_dpop_authserver_nonce IS 'Latest DPoP nonce from authorization server';
COMMENT ON COLUMN aggregators.oauth_dpop_pds_nonce IS 'Latest DPoP nonce from PDS';
COMMENT ON COLUMN aggregators.api_key_created_at IS 'When the API key was generated';
COMMENT ON COLUMN aggregators.api_key_revoked_at IS 'When the API key was revoked (NULL = active)';
COMMENT ON COLUMN aggregators.api_key_last_used_at IS 'Last successful authentication using this API key';

-- +goose Down
-- Remove API key columns from aggregators table
DROP INDEX IF EXISTS idx_aggregators_api_key_hash;

ALTER TABLE aggregators
    DROP COLUMN IF EXISTS api_key_prefix,
    DROP COLUMN IF EXISTS api_key_hash,
    DROP COLUMN IF EXISTS oauth_access_token,
    DROP COLUMN IF EXISTS oauth_refresh_token,
    DROP COLUMN IF EXISTS oauth_token_expires_at,
    DROP COLUMN IF EXISTS oauth_pds_url,
    DROP COLUMN IF EXISTS oauth_auth_server_iss,
    DROP COLUMN IF EXISTS oauth_auth_server_token_endpoint,
    DROP COLUMN IF EXISTS oauth_dpop_private_key_multibase,
    DROP COLUMN IF EXISTS oauth_dpop_authserver_nonce,
    DROP COLUMN IF EXISTS oauth_dpop_pds_nonce,
    DROP COLUMN IF EXISTS api_key_created_at,
    DROP COLUMN IF EXISTS api_key_revoked_at,
    DROP COLUMN IF EXISTS api_key_last_used_at;
