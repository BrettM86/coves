-- +goose Up
-- Encrypt aggregator OAuth tokens at rest using pgp_sym_encrypt
-- This addresses the security issue where OAuth tokens were stored in plaintext
-- despite migration 024 claiming "encrypted at application layer before storage"

-- +goose StatementBegin

-- Step 1: Add new encrypted columns for OAuth tokens and DPoP private key
ALTER TABLE aggregators
    ADD COLUMN oauth_access_token_encrypted BYTEA,
    ADD COLUMN oauth_refresh_token_encrypted BYTEA,
    ADD COLUMN oauth_dpop_private_key_encrypted BYTEA;

-- Step 2: Migrate existing plaintext data to encrypted columns
-- Uses the same encryption key table as community credentials (migration 006)
UPDATE aggregators
SET
    oauth_access_token_encrypted = CASE
        WHEN oauth_access_token IS NOT NULL AND oauth_access_token != ''
        THEN pgp_sym_encrypt(oauth_access_token, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END,
    oauth_refresh_token_encrypted = CASE
        WHEN oauth_refresh_token IS NOT NULL AND oauth_refresh_token != ''
        THEN pgp_sym_encrypt(oauth_refresh_token, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END,
    oauth_dpop_private_key_encrypted = CASE
        WHEN oauth_dpop_private_key_multibase IS NOT NULL AND oauth_dpop_private_key_multibase != ''
        THEN pgp_sym_encrypt(oauth_dpop_private_key_multibase, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END
WHERE oauth_access_token IS NOT NULL
   OR oauth_refresh_token IS NOT NULL
   OR oauth_dpop_private_key_multibase IS NOT NULL;

-- Step 3: Drop the old plaintext columns
ALTER TABLE aggregators
    DROP COLUMN oauth_access_token,
    DROP COLUMN oauth_refresh_token,
    DROP COLUMN oauth_dpop_private_key_multibase;

-- Step 4: Add security comments
COMMENT ON COLUMN aggregators.oauth_access_token_encrypted IS 'SENSITIVE: Encrypted OAuth access token (pgp_sym_encrypt) for PDS operations';
COMMENT ON COLUMN aggregators.oauth_refresh_token_encrypted IS 'SENSITIVE: Encrypted OAuth refresh token (pgp_sym_encrypt) for session renewal';
COMMENT ON COLUMN aggregators.oauth_dpop_private_key_encrypted IS 'SENSITIVE: Encrypted DPoP private key (pgp_sym_encrypt) for token refresh';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore plaintext columns
ALTER TABLE aggregators
    ADD COLUMN oauth_access_token TEXT,
    ADD COLUMN oauth_refresh_token TEXT,
    ADD COLUMN oauth_dpop_private_key_multibase TEXT;

-- Decrypt data back to plaintext (for rollback)
UPDATE aggregators
SET
    oauth_access_token = CASE
        WHEN oauth_access_token_encrypted IS NOT NULL
        THEN pgp_sym_decrypt(oauth_access_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END,
    oauth_refresh_token = CASE
        WHEN oauth_refresh_token_encrypted IS NOT NULL
        THEN pgp_sym_decrypt(oauth_refresh_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END,
    oauth_dpop_private_key_multibase = CASE
        WHEN oauth_dpop_private_key_encrypted IS NOT NULL
        THEN pgp_sym_decrypt(oauth_dpop_private_key_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
        ELSE NULL
    END
WHERE oauth_access_token_encrypted IS NOT NULL
   OR oauth_refresh_token_encrypted IS NOT NULL
   OR oauth_dpop_private_key_encrypted IS NOT NULL;

-- Drop encrypted columns
ALTER TABLE aggregators
    DROP COLUMN oauth_access_token_encrypted,
    DROP COLUMN oauth_refresh_token_encrypted,
    DROP COLUMN oauth_dpop_private_key_encrypted;

-- Restore comments
COMMENT ON COLUMN aggregators.oauth_access_token IS 'SENSITIVE: OAuth access token for PDS operations';
COMMENT ON COLUMN aggregators.oauth_refresh_token IS 'SENSITIVE: OAuth refresh token for session renewal';
COMMENT ON COLUMN aggregators.oauth_dpop_private_key_multibase IS 'SENSITIVE: DPoP private key in multibase format for token refresh';

-- +goose StatementEnd
