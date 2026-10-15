-- +goose Up
ALTER TABLE users
    ADD COLUMN origin TEXT,
    ADD COLUMN origin_username TEXT,
    ADD COLUMN origin_url TEXT,
    ADD COLUMN origin_software TEXT;

-- +goose Down
ALTER TABLE users
    DROP COLUMN origin,
    DROP COLUMN origin_username,
    DROP COLUMN origin_url,
    DROP COLUMN origin_software;
