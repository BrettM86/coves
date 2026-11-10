-- +goose Up
-- The generated credentials of a community PDS account, written before
-- createAccount and deleted once the communities row stores them. A creation
-- that fails part-way (even one whose createAccount response is lost) can then
-- sign in to the account it made instead of leaving it with no known password.
-- The password is sealed with the AppView credential cipher.
CREATE TABLE community_provisioning_accounts (
    handle TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    created_by_did TEXT NOT NULL,
    hosted_by_did TEXT NOT NULL,
    pds_url TEXT NOT NULL,
    pds_email TEXT NOT NULL,
    pds_password_encrypted BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE community_provisioning_accounts;
