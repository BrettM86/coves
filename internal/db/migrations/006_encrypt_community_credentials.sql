-- +goose Up
-- Enable pgcrypto extension for encryption at rest
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Create encryption key table (single-row config table)
-- SECURITY: In production, use environment variable or external key management
CREATE TABLE encryption_keys (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    key_data BYTEA NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    rotated_at TIMESTAMP WITH TIME ZONE
);

-- Insert default encryption key
INSERT INTO encryption_keys (id, key_data)
VALUES (1, gen_random_bytes(32))
ON CONFLICT (id) DO NOTHING;

-- Add encrypted columns
ALTER TABLE communities
    ADD COLUMN pds_access_token_encrypted BYTEA,
    ADD COLUMN pds_refresh_token_encrypted BYTEA;

-- Add index for communities with credentials
CREATE INDEX idx_communities_encrypted_tokens ON communities(did) WHERE pds_access_token_encrypted IS NOT NULL;

-- Security comments
COMMENT ON TABLE encryption_keys IS 'Encryption keys for sensitive data - RESTRICT ACCESS';
COMMENT ON COLUMN communities.pds_access_token_encrypted IS 'Encrypted JWT - decrypt with pgp_sym_decrypt';
COMMENT ON COLUMN communities.pds_refresh_token_encrypted IS 'Encrypted refresh token - decrypt with pgp_sym_decrypt';

-- +goose Down
DROP INDEX IF EXISTS idx_communities_encrypted_tokens;

ALTER TABLE communities
    DROP COLUMN IF EXISTS pds_access_token_encrypted,
    DROP COLUMN IF EXISTS pds_refresh_token_encrypted;

DROP TABLE IF EXISTS encryption_keys;
