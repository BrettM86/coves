-- +goose Up
-- The literal 1 below is credentialcipher.Version, the leading byte of every
-- value the application cipher writes; pgcrypto output starts with 0xC3, so a
-- first byte other than 1 (or an empty value) is legacy data. The AppView's
-- startup pass (postgres.ReencryptLegacyCredentials) runs before goose and
-- converts such rows; this guard is the independent backstop so the key is
-- never dropped while something still needs it. It assumes the single-writer
-- deployment Coves runs: one AppView, replaced on deploy, so no older binary
-- can write pgcrypto ciphertext between this check and the DROP.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM communities
        WHERE (pds_password_encrypted IS NOT NULL AND
                (octet_length(pds_password_encrypted) = 0 OR get_byte(pds_password_encrypted, 0) <> 1))
           OR (pds_access_token_encrypted IS NOT NULL AND
                (octet_length(pds_access_token_encrypted) = 0 OR get_byte(pds_access_token_encrypted, 0) <> 1))
           OR (pds_refresh_token_encrypted IS NOT NULL AND
                (octet_length(pds_refresh_token_encrypted) = 0 OR get_byte(pds_refresh_token_encrypted, 0) <> 1))
        UNION ALL
        SELECT 1
        FROM aggregators
        WHERE (oauth_access_token_encrypted IS NOT NULL AND
                (octet_length(oauth_access_token_encrypted) = 0 OR get_byte(oauth_access_token_encrypted, 0) <> 1))
           OR (oauth_refresh_token_encrypted IS NOT NULL AND
                (octet_length(oauth_refresh_token_encrypted) = 0 OR get_byte(oauth_refresh_token_encrypted, 0) <> 1))
           OR (oauth_dpop_private_key_encrypted IS NOT NULL AND
                (octet_length(oauth_dpop_private_key_encrypted) = 0 OR get_byte(oauth_dpop_private_key_encrypted, 0) <> 1))
    ) THEN
        RAISE EXCEPTION 'legacy pgcrypto ciphertext remains; the AppView must re-encrypt credentials with ENCRYPTION_KEY before migration 046 can drop encryption_keys';
    END IF;
END
$$;
-- +goose StatementEnd

COMMENT ON COLUMN communities.pds_password_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY; required for session recovery when tokens expire';
COMMENT ON COLUMN communities.pds_access_token_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY for community PDS access';
COMMENT ON COLUMN communities.pds_refresh_token_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY for community PDS session renewal';
COMMENT ON COLUMN aggregators.oauth_access_token_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY for PDS operations';
COMMENT ON COLUMN aggregators.oauth_refresh_token_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY for session renewal';
COMMENT ON COLUMN aggregators.oauth_dpop_private_key_encrypted IS 'SENSITIVE: AES-256-GCM ciphertext sealed by the application with the key from ENCRYPTION_KEY for token refresh';

DROP TABLE encryption_keys;

-- +goose Down
-- Schema-only rollback. The table comes back with a FRESH random key and the
-- credential columns keep their AES-256-GCM values, so a pre-046 binary cannot
-- read any stored credential after this runs. There is no reverse conversion:
-- to actually roll back, restore a pre-cutover backup or NULL the credential
-- columns and re-provision the affected communities and aggregators.
CREATE TABLE encryption_keys (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    key_data BYTEA NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    rotated_at TIMESTAMP WITH TIME ZONE
);

INSERT INTO encryption_keys (id, key_data)
VALUES (1, gen_random_bytes(32))
ON CONFLICT (id) DO NOTHING;

COMMENT ON TABLE encryption_keys IS 'Encryption keys for sensitive data - RESTRICT ACCESS';
COMMENT ON COLUMN communities.pds_password_encrypted IS 'Encrypted community PDS password (pgp_sym_encrypt) - required for session recovery when tokens expire';
COMMENT ON COLUMN communities.pds_access_token_encrypted IS 'Encrypted JWT - decrypt with pgp_sym_decrypt';
COMMENT ON COLUMN communities.pds_refresh_token_encrypted IS 'Encrypted refresh token - decrypt with pgp_sym_decrypt';
COMMENT ON COLUMN aggregators.oauth_access_token_encrypted IS 'SENSITIVE: Encrypted OAuth access token (pgp_sym_encrypt) for PDS operations';
COMMENT ON COLUMN aggregators.oauth_refresh_token_encrypted IS 'SENSITIVE: Encrypted OAuth refresh token (pgp_sym_encrypt) for session renewal';
COMMENT ON COLUMN aggregators.oauth_dpop_private_key_encrypted IS 'SENSITIVE: Encrypted DPoP private key (pgp_sym_encrypt) for token refresh';
