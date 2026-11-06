-- +goose Up
-- An account's PDS-confirmed takendown or suspended status, set and cleared by
-- the users consumer from Jetstream #account events after confirming with
-- com.atproto.sync.getRepoStatus. Read paths treat a row like an in-force
-- instance account ban (accountRestrictedSQL), without the write refusal.
CREATE TABLE account_status_restrictions (
    did text PRIMARY KEY,
    status text NOT NULL CHECK (status IN ('takendown', 'suspended')),
    confirmed_at timestamptz NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE account_status_restrictions;
